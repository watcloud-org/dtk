// Package manager implements the state machine and lifecycle management for
// each container group. It is the heart of dtk: it decides when to pause and
// unpause groups, serialises concurrent wake requests, and tracks idle time.
package manager

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/ylecuyer/dtk/internal/config"
)

// DockerClient is the subset of Docker operations manager needs.
type DockerClient interface {
	ContainerStates(ctx context.Context, names []string) (map[string]string, error)
	PauseGroup(ctx context.Context, names []string) error
	UnpauseGroup(ctx context.Context, names []string) error
	RestartGroup(ctx context.Context, names []string) error
}

// State represents the lifecycle state of a container group.
type State int

const (
	// StatePaused — all containers are paused, no traffic.
	StatePaused State = iota
	// StateWaking — unpause is in progress; requests wait.
	StateWaking
	// StateRunning — containers are running; requests are forwarded immediately.
	StateRunning
	// StatePausing — idle timer fired; pause is in progress.
	StatePausing
)

func (s State) String() string {
	switch s {
	case StatePaused:
		return "paused"
	case StateWaking:
		return "waking"
	case StateRunning:
		return "running"
	case StatePausing:
		return "pausing"
	default:
		return "unknown"
	}
}

// GroupInfo is a snapshot of a group's runtime status, used by the dashboard.
type GroupInfo struct {
	Name        string
	Host        string
	Containers  []string
	State       State
	LastRequest time.Time
	IdleTimeout time.Duration
	WakeDelay   time.Duration
}

// groupState holds the mutable runtime state of a single group.
type groupState struct {
	mu          sync.Mutex
	state       State
	lastRequest time.Time
	// wakeCh is non-nil while state == StateWaking.
	// It is closed (and set to nil) when StateRunning is reached.
	wakeCh chan struct{}
	// idleTimer fires when the group should be paused.
	idleTimer *time.Timer
}

// Manager manages all configured container groups.
type Manager struct {
	groups map[string]*groupEntry // keyed by host
	docker DockerClient
	log    *slog.Logger
}

// ErrUnknownHost is returned when the requested host does not map to a group.
var ErrUnknownHost = errors.New("unknown host")

type groupEntry struct {
	cfg   config.Group
	state *groupState
}

// New creates a Manager for the given groups. It syncs the internal state
// with the actual Docker container states on startup.
func New(groups []config.Group, dc DockerClient, log *slog.Logger) *Manager {
	m := &Manager{
		groups: make(map[string]*groupEntry, len(groups)),
		docker: dc,
		log:    log,
	}
	for _, g := range groups {
		g := g // capture
		m.groups[g.Host] = &groupEntry{
			cfg: g,
			state: &groupState{
				state: StatePaused,
			},
		}
	}

	// Sync actual container states on startup.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	for host, e := range m.groups {
		states, err := dc.ContainerStates(ctx, e.cfg.Containers)
		if err != nil {
			log.Warn("failed to query container states at startup", "group", e.cfg.Name, "err", err)
			continue
		}

		// Check if any containers are running (not paused, not exited, etc).
		anyRunning := false
		for _, containerName := range e.cfg.Containers {
			state := states[containerName]
			if state == "running" {
				anyRunning = true
				break
			}
		}

		if anyRunning {
			// Containers are actually running; update internal state to match.
			gs := e.state
			gs.mu.Lock()
			gs.state = StateRunning
			gs.lastRequest = time.Now()
			gs.idleTimer = time.AfterFunc(e.cfg.IdleTimeout.Duration, func() {
				m.pauseGroup(e)
			})
			gs.mu.Unlock()
			log.Info("group is running at startup", "host", host, "group", e.cfg.Name)
		}
	}

	return m
}

// WakeAndWait ensures the group for the given host is running and blocks until
// it is safe to forward a request. It returns false if the host is unknown.
//
// This is the primary entry point called by the HTTP proxy for every request.
func (m *Manager) WakeAndWait(ctx context.Context, host string) bool {
	e, ok := m.groups[host]
	if !ok {
		return false
	}
	gs := e.state

	for {
		gs.mu.Lock()

		switch gs.state {
		case StateRunning:
			// Reset idle timer and return immediately.
			gs.lastRequest = time.Now()
			if gs.idleTimer != nil {
				gs.idleTimer.Reset(e.cfg.IdleTimeout.Duration)
			}
			gs.mu.Unlock()
			return true

		case StateWaking:
			// Another goroutine is already waking the group.
			// Wait for it to finish.
			ch := gs.wakeCh
			gs.mu.Unlock()
			select {
			case <-ch:
				// Waking complete; loop to re-check state.
			case <-ctx.Done():
				return true // best-effort: proceed even if ctx cancelled
			}

		case StatePaused:
			// We are the first — take ownership of the wake.
			ch := make(chan struct{})
			gs.state = StateWaking
			gs.wakeCh = ch
			gs.lastRequest = time.Now()
			gs.mu.Unlock()

			m.doWake(ctx, e, ch)
			return true

		case StatePausing:
			// Pause is in progress. Wait briefly and retry.
			gs.mu.Unlock()
			select {
			case <-time.After(100 * time.Millisecond):
			case <-ctx.Done():
				return true
			}
		}
	}
}

// doWake unpauses the group's containers, waits wake_delay, then sets the
// group state to Running and closes ch to unblock waiting goroutines.
// It is called with the mutex NOT held.
func (m *Manager) doWake(ctx context.Context, e *groupEntry, ch chan struct{}) {
	gs := e.state
	log := m.log.With("group", e.cfg.Name)

	log.Info("waking group", "containers", e.cfg.Containers)
	if err := m.docker.UnpauseGroup(ctx, e.cfg.Containers); err != nil {
		log.Error("unpause failed", "err", err)
	}

	// Wait for containers to be ready.
	if e.cfg.WakeDelay.Duration > 0 {
		select {
		case <-time.After(e.cfg.WakeDelay.Duration):
		case <-ctx.Done():
		}
	}

	gs.mu.Lock()
	gs.state = StateRunning
	gs.wakeCh = nil
	// Start the idle timer.
	gs.idleTimer = time.AfterFunc(e.cfg.IdleTimeout.Duration, func() {
		m.pauseGroup(e)
	})
	close(ch)
	gs.mu.Unlock()

	log.Info("group running")
}

// pauseGroup is called by the idle timer. It pauses all containers and
// transitions the group to StatePaused.
func (m *Manager) pauseGroup(e *groupEntry) {
	gs := e.state
	log := m.log.With("group", e.cfg.Name)

	gs.mu.Lock()
	if gs.state != StateRunning {
		gs.mu.Unlock()
		return
	}
	gs.state = StatePausing
	gs.idleTimer = nil
	gs.mu.Unlock()

	log.Info("pausing idle group", "containers", e.cfg.Containers)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := m.docker.PauseGroup(ctx, e.cfg.Containers); err != nil {
		log.Error("pause failed", "err", err)
	}

	gs.mu.Lock()
	gs.state = StatePaused
	gs.mu.Unlock()

	log.Info("group paused")
}

// Info returns a snapshot of all group states for the dashboard.
func (m *Manager) Info() []GroupInfo {
	infos := make([]GroupInfo, 0, len(m.groups))
	for _, e := range m.groups {
		gs := e.state
		gs.mu.Lock()
		info := GroupInfo{
			Name:        e.cfg.Name,
			Host:        e.cfg.Host,
			Containers:  e.cfg.Containers,
			State:       gs.state,
			LastRequest: gs.lastRequest,
			IdleTimeout: e.cfg.IdleTimeout.Duration,
			WakeDelay:   e.cfg.WakeDelay.Duration,
		}
		gs.mu.Unlock()
		infos = append(infos, info)
	}

	sort.Slice(infos, func(i, j int) bool {
		if infos[i].Name == infos[j].Name {
			return infos[i].Host < infos[j].Host
		}
		return infos[i].Name < infos[j].Name
	})

	return infos
}

// Shutdown pauses all running groups. Called on graceful shutdown.
func (m *Manager) Shutdown(ctx context.Context) {
	for _, e := range m.groups {
		gs := e.state
		gs.mu.Lock()
		if gs.state == StateRunning {
			if gs.idleTimer != nil {
				gs.idleTimer.Stop()
				gs.idleTimer = nil
			}
			gs.mu.Unlock()
			m.pauseGroup(e)
		} else {
			gs.mu.Unlock()
		}
	}
}

// Restart restarts every container for the host's group and moves the group
// back to running once the wake delay has elapsed.
func (m *Manager) Restart(ctx context.Context, host string) error {
	e, ok := m.groups[host]
	if !ok {
		return ErrUnknownHost
	}
	gs := e.state

	for {
		gs.mu.Lock()
		switch gs.state {
		case StateWaking, StatePausing:
			gs.mu.Unlock()
			select {
			case <-time.After(100 * time.Millisecond):
			case <-ctx.Done():
				return ctx.Err()
			}
			continue

		case StateRunning, StatePaused:
			if gs.idleTimer != nil {
				gs.idleTimer.Stop()
				gs.idleTimer = nil
			}
			ch := make(chan struct{})
			gs.state = StateWaking
			gs.wakeCh = ch
			gs.lastRequest = time.Now()
			gs.mu.Unlock()

			err := m.doRestart(ctx, e)

			gs.mu.Lock()
			if err != nil {
				gs.state = StatePaused
				gs.wakeCh = nil
				close(ch)
				gs.mu.Unlock()
				return err
			}

			gs.state = StateRunning
			gs.wakeCh = nil
			gs.idleTimer = time.AfterFunc(e.cfg.IdleTimeout.Duration, func() {
				m.pauseGroup(e)
			})
			close(ch)
			gs.mu.Unlock()

			return nil
		}
	}
}

func (m *Manager) doRestart(ctx context.Context, e *groupEntry) error {
	log := m.log.With("group", e.cfg.Name)
	log.Info("restarting group", "containers", e.cfg.Containers)

	if err := m.docker.RestartGroup(ctx, e.cfg.Containers); err != nil {
		log.Error("restart failed", "err", err)
		return err
	}

	if e.cfg.WakeDelay.Duration > 0 {
		select {
		case <-time.After(e.cfg.WakeDelay.Duration):
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	log.Info("group restarted")
	return nil
}
