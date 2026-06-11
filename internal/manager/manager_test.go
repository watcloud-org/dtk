package manager

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/ylecuyer/dtk/internal/config"
)

type fakeDocker struct {
	mu sync.Mutex

	states       map[string]string
	stateErr     error
	pauseErr     error
	unpauseErr   error
	restartErr   error
	unpauseBlock chan struct{}
	pauseCalls   int
	unpauseCalls int
	restartCalls int
}

func newFakeDocker(states map[string]string) *fakeDocker {
	cp := make(map[string]string, len(states))
	for k, v := range states {
		cp[k] = v
	}
	return &fakeDocker{states: cp}
}

func (f *fakeDocker) ContainerStates(_ context.Context, names []string) (map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stateErr != nil {
		return nil, f.stateErr
	}
	out := make(map[string]string, len(names))
	for _, n := range names {
		if st, ok := f.states[n]; ok {
			out[n] = st
		}
	}
	return out, nil
}

func (f *fakeDocker) PauseGroup(_ context.Context, names []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pauseCalls++
	if f.pauseErr != nil {
		return f.pauseErr
	}
	for _, n := range names {
		if _, ok := f.states[n]; ok {
			f.states[n] = "paused"
		}
	}
	return nil
}

func (f *fakeDocker) UnpauseGroup(_ context.Context, names []string) error {
	if f.unpauseBlock != nil {
		<-f.unpauseBlock
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unpauseCalls++
	if f.unpauseErr != nil {
		return f.unpauseErr
	}
	for _, n := range names {
		if _, ok := f.states[n]; ok {
			f.states[n] = "running"
		}
	}
	return nil
}

func (f *fakeDocker) RestartGroup(_ context.Context, names []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.restartCalls++
	if f.restartErr != nil {
		return f.restartErr
	}
	for _, n := range names {
		if _, ok := f.states[n]; ok {
			f.states[n] = "running"
		}
	}
	return nil
}

func (f *fakeDocker) counts() (pause, unpause, restart int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pauseCalls, f.unpauseCalls, f.restartCalls
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func testGroup(idle, wake time.Duration) config.Group {
	return config.Group{
		Name:       "app",
		Host:       "app.example.com",
		Containers: []string{"app"},
		Target:     "http://app:8080",
		IdleTimeout: config.Duration{
			Duration: idle,
		},
		WakeDelay: config.Duration{
			Duration: wake,
		},
	}
}

func stateForHost(t *testing.T, m *Manager, host string) State {
	t.Helper()
	for _, info := range m.Info() {
		if info.Host == host {
			return info.State
		}
	}
	t.Fatalf("missing host %q", host)
	return StatePaused
}

func waitForState(t *testing.T, m *Manager, host string, want State, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if stateForHost(t, m, host) == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("state did not become %s within %s (current: %s)", want, timeout, stateForHost(t, m, host))
}

func TestNewReflectsRunningContainersAtStartup(t *testing.T) {
	dc := newFakeDocker(map[string]string{"app": "running"})
	m := New([]config.Group{testGroup(200*time.Millisecond, 0)}, dc, testLogger())

	if got := stateForHost(t, m, "app.example.com"); got != StateRunning {
		t.Fatalf("state = %s, want running", got)
	}
}

func TestWakeAndWaitUnknownHost(t *testing.T) {
	dc := newFakeDocker(map[string]string{"app": "paused"})
	m := New([]config.Group{testGroup(time.Second, 0)}, dc, testLogger())

	if ok := m.WakeAndWait(context.Background(), "missing.example.com"); ok {
		t.Fatal("WakeAndWait() = true, want false for unknown host")
	}
}

func TestWakeAndWaitTransitionsPausedToRunning(t *testing.T) {
	dc := newFakeDocker(map[string]string{"app": "paused"})
	m := New([]config.Group{testGroup(time.Second, 0)}, dc, testLogger())

	if ok := m.WakeAndWait(context.Background(), "app.example.com"); !ok {
		t.Fatal("WakeAndWait() = false, want true")
	}

	if got := stateForHost(t, m, "app.example.com"); got != StateRunning {
		t.Fatalf("state = %s, want running", got)
	}
	_, unpause, _ := dc.counts()
	if unpause != 1 {
		t.Fatalf("unpause calls = %d, want 1", unpause)
	}
}

func TestWakeAndWaitConcurrentRequestsShareWake(t *testing.T) {
	dc := newFakeDocker(map[string]string{"app": "paused"})
	m := New([]config.Group{testGroup(2*time.Second, 20*time.Millisecond)}, dc, testLogger())

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if ok := m.WakeAndWait(ctx, "app.example.com"); !ok {
				t.Error("WakeAndWait() = false, want true")
			}
		}()
	}
	wg.Wait()

	_, unpause, _ := dc.counts()
	if unpause != 1 {
		t.Fatalf("unpause calls = %d, want 1", unpause)
	}
	if got := stateForHost(t, m, "app.example.com"); got != StateRunning {
		t.Fatalf("state = %s, want running", got)
	}
}

func TestIdleTimeoutPausesRunningGroup(t *testing.T) {
	dc := newFakeDocker(map[string]string{"app": "paused"})
	m := New([]config.Group{testGroup(30*time.Millisecond, 0)}, dc, testLogger())

	if ok := m.WakeAndWait(context.Background(), "app.example.com"); !ok {
		t.Fatal("WakeAndWait() = false, want true")
	}
	waitForState(t, m, "app.example.com", StatePaused, 300*time.Millisecond)

	pause, _, _ := dc.counts()
	if pause == 0 {
		t.Fatal("pause calls = 0, want at least 1")
	}
}

func TestRestartUnknownHost(t *testing.T) {
	dc := newFakeDocker(map[string]string{"app": "running"})
	m := New([]config.Group{testGroup(time.Second, 0)}, dc, testLogger())

	err := m.Restart(context.Background(), "missing.example.com")
	if !errors.Is(err, ErrUnknownHost) {
		t.Fatalf("Restart() error = %v, want ErrUnknownHost", err)
	}
}

func TestRestartSuccessSetsRunningState(t *testing.T) {
	dc := newFakeDocker(map[string]string{"app": "paused"})
	m := New([]config.Group{testGroup(time.Second, 0)}, dc, testLogger())

	err := m.Restart(context.Background(), "app.example.com")
	if err != nil {
		t.Fatalf("Restart() error = %v", err)
	}

	if got := stateForHost(t, m, "app.example.com"); got != StateRunning {
		t.Fatalf("state = %s, want running", got)
	}
	_, _, restart := dc.counts()
	if restart != 1 {
		t.Fatalf("restart calls = %d, want 1", restart)
	}
}

func TestStateString(t *testing.T) {
	tests := []struct {
		in   State
		want string
	}{
		{in: StatePaused, want: "paused"},
		{in: StateWaking, want: "waking"},
		{in: StateRunning, want: "running"},
		{in: StatePausing, want: "pausing"},
		{in: State(999), want: "unknown"},
	}

	for _, tc := range tests {
		if got := tc.in.String(); got != tc.want {
			t.Fatalf("String() = %q, want %q", got, tc.want)
		}
	}
}

func TestNewStartupContainerStateErrorLeavesGroupPaused(t *testing.T) {
	dc := newFakeDocker(map[string]string{"app": "running"})
	dc.stateErr = errors.New("boom")
	m := New([]config.Group{testGroup(time.Second, 0)}, dc, testLogger())

	if got := stateForHost(t, m, "app.example.com"); got != StatePaused {
		t.Fatalf("state = %s, want paused", got)
	}
}

func TestWakeAndWaitCanceledWhileWakingReturns(t *testing.T) {
	dc := newFakeDocker(map[string]string{"app": "paused"})
	dc.unpauseBlock = make(chan struct{})
	m := New([]config.Group{testGroup(time.Second, 100*time.Millisecond)}, dc, testLogger())

	started := make(chan struct{})
	go func() {
		close(started)
		_ = m.WakeAndWait(context.Background(), "app.example.com")
	}()
	<-started

	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		e := m.groups["app.example.com"]
		e.state.mu.Lock()
		st := e.state.state
		e.state.mu.Unlock()
		if st == StateWaking {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if ok := m.WakeAndWait(ctx, "app.example.com"); !ok {
		t.Fatal("WakeAndWait() = false, want true")
	}

	close(dc.unpauseBlock)
	waitForState(t, m, "app.example.com", StateRunning, time.Second)
}

func TestWakeAndWaitCanceledWhilePausingReturns(t *testing.T) {
	dc := newFakeDocker(map[string]string{"app": "running"})
	m := New([]config.Group{testGroup(time.Second, 0)}, dc, testLogger())
	e := m.groups["app.example.com"]

	e.state.mu.Lock()
	e.state.state = StatePausing
	e.state.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if ok := m.WakeAndWait(ctx, "app.example.com"); !ok {
		t.Fatal("WakeAndWait() = false, want true")
	}
}

func TestWakeAndWaitUnpauseErrorStillTransitionsToRunning(t *testing.T) {
	dc := newFakeDocker(map[string]string{"app": "paused"})
	dc.unpauseErr = errors.New("cannot unpause")
	m := New([]config.Group{testGroup(time.Second, 0)}, dc, testLogger())

	if ok := m.WakeAndWait(context.Background(), "app.example.com"); !ok {
		t.Fatal("WakeAndWait() = false, want true")
	}
	if got := stateForHost(t, m, "app.example.com"); got != StateRunning {
		t.Fatalf("state = %s, want running", got)
	}
}

func TestPauseGroupSkipsWhenNotRunning(t *testing.T) {
	dc := newFakeDocker(map[string]string{"app": "paused"})
	m := New([]config.Group{testGroup(time.Second, 0)}, dc, testLogger())
	e := m.groups["app.example.com"]

	m.pauseGroup(e)

	pause, _, _ := dc.counts()
	if pause != 0 {
		t.Fatalf("pause calls = %d, want 0", pause)
	}
}

func TestPauseGroupPauseErrorStillSetsPaused(t *testing.T) {
	dc := newFakeDocker(map[string]string{"app": "running"})
	dc.pauseErr = errors.New("cannot pause")
	m := New([]config.Group{testGroup(time.Second, 0)}, dc, testLogger())
	e := m.groups["app.example.com"]

	e.state.mu.Lock()
	e.state.state = StateRunning
	e.state.mu.Unlock()

	m.pauseGroup(e)

	if got := stateForHost(t, m, "app.example.com"); got != StatePaused {
		t.Fatalf("state = %s, want paused", got)
	}
}

func TestInfoSortedByNameThenHost(t *testing.T) {
	dc := newFakeDocker(map[string]string{})
	groups := []config.Group{
		{Name: "b", Host: "b.example.com", Containers: []string{"b"}, IdleTimeout: config.Duration{Duration: time.Second}, WakeDelay: config.Duration{Duration: time.Second}},
		{Name: "a", Host: "z.example.com", Containers: []string{"a1"}, IdleTimeout: config.Duration{Duration: time.Second}, WakeDelay: config.Duration{Duration: time.Second}},
		{Name: "a", Host: "a.example.com", Containers: []string{"a2"}, IdleTimeout: config.Duration{Duration: time.Second}, WakeDelay: config.Duration{Duration: time.Second}},
	}
	m := New(groups, dc, testLogger())

	infos := m.Info()
	if len(infos) != 3 {
		t.Fatalf("len(Info()) = %d, want 3", len(infos))
	}
	if infos[0].Host != "a.example.com" || infos[1].Host != "z.example.com" || infos[2].Host != "b.example.com" {
		t.Fatalf("unexpected sort order: %#v", infos)
	}
}

func TestShutdownPausesRunningGroups(t *testing.T) {
	dc := newFakeDocker(map[string]string{"app": "running"})
	m := New([]config.Group{testGroup(time.Second, 0)}, dc, testLogger())

	m.Shutdown(context.Background())

	if got := stateForHost(t, m, "app.example.com"); got != StatePaused {
		t.Fatalf("state = %s, want paused", got)
	}
	pause, _, _ := dc.counts()
	if pause == 0 {
		t.Fatal("expected at least one pause call")
	}
}

func TestRestartCanceledWhilePausing(t *testing.T) {
	dc := newFakeDocker(map[string]string{"app": "running"})
	m := New([]config.Group{testGroup(time.Second, 0)}, dc, testLogger())
	e := m.groups["app.example.com"]

	e.state.mu.Lock()
	e.state.state = StatePausing
	e.state.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := m.Restart(ctx, "app.example.com")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Restart() error = %v, want DeadlineExceeded", err)
	}
}

func TestRestartFailureSetsPaused(t *testing.T) {
	dc := newFakeDocker(map[string]string{"app": "running"})
	dc.restartErr = errors.New("cannot restart")
	m := New([]config.Group{testGroup(time.Second, 0)}, dc, testLogger())

	err := m.Restart(context.Background(), "app.example.com")
	if err == nil {
		t.Fatal("Restart() error = nil, want error")
	}
	if got := stateForHost(t, m, "app.example.com"); got != StatePaused {
		t.Fatalf("state = %s, want paused", got)
	}
}

func TestDoRestartCanceledByWakeDelay(t *testing.T) {
	dc := newFakeDocker(map[string]string{"app": "running"})
	m := New([]config.Group{testGroup(time.Second, 200*time.Millisecond)}, dc, testLogger())
	e := m.groups["app.example.com"]

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := m.doRestart(ctx, e)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("doRestart() error = %v, want DeadlineExceeded", err)
	}
}

func TestWakeAndWaitWaitsThroughPausingState(t *testing.T) {
	dc := newFakeDocker(map[string]string{"app": "paused"})
	m := New([]config.Group{testGroup(time.Second, 0)}, dc, testLogger())
	e := m.groups["app.example.com"]

	e.state.mu.Lock()
	e.state.state = StatePausing
	e.state.mu.Unlock()

	done := make(chan struct{})
	go func() {
		_ = m.WakeAndWait(context.Background(), "app.example.com")
		close(done)
	}()

	time.Sleep(130 * time.Millisecond)
	e.state.mu.Lock()
	e.state.state = StatePaused
	e.state.mu.Unlock()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("WakeAndWait() did not complete")
	}
}

func TestWakeAndWaitCanceledWhileWakingCoversDoneBranch(t *testing.T) {
	dc := newFakeDocker(map[string]string{"app": "paused"})
	dc.unpauseBlock = make(chan struct{})
	m := New([]config.Group{testGroup(time.Second, 0)}, dc, testLogger())
	e := m.groups["app.example.com"]

	ready := make(chan struct{})
	go func() {
		_ = m.WakeAndWait(context.Background(), "app.example.com")
		close(ready)
	}()

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		e.state.mu.Lock()
		st := e.state.state
		e.state.mu.Unlock()
		if st == StateWaking {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if ok := m.WakeAndWait(ctx, "app.example.com"); !ok {
		t.Fatal("WakeAndWait() = false, want true")
	}

	close(dc.unpauseBlock)
	select {
	case <-ready:
	case <-time.After(time.Second):
		t.Fatal("first WakeAndWait() did not complete")
	}
}

func TestShutdownSkipsNonRunningGroups(t *testing.T) {
	dc := newFakeDocker(map[string]string{"app": "paused"})
	m := New([]config.Group{testGroup(time.Second, 0)}, dc, testLogger())

	m.Shutdown(context.Background())

	pause, _, _ := dc.counts()
	if pause != 0 {
		t.Fatalf("pause calls = %d, want 0", pause)
	}
}

func TestRestartWaitsThroughPausingState(t *testing.T) {
	dc := newFakeDocker(map[string]string{"app": "running"})
	m := New([]config.Group{testGroup(40*time.Millisecond, 0)}, dc, testLogger())
	e := m.groups["app.example.com"]

	e.state.mu.Lock()
	e.state.state = StatePausing
	e.state.mu.Unlock()

	done := make(chan error, 1)
	go func() {
		done <- m.Restart(context.Background(), "app.example.com")
	}()

	time.Sleep(130 * time.Millisecond)
	e.state.mu.Lock()
	e.state.state = StatePaused
	e.state.mu.Unlock()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Restart() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Restart() did not complete")
	}

	waitForState(t, m, "app.example.com", StatePaused, 500*time.Millisecond)
}

func TestDoRestartWaitDelayCompletes(t *testing.T) {
	dc := newFakeDocker(map[string]string{"app": "running"})
	m := New([]config.Group{testGroup(time.Second, 10*time.Millisecond)}, dc, testLogger())
	e := m.groups["app.example.com"]

	if err := m.doRestart(context.Background(), e); err != nil {
		t.Fatalf("doRestart() error = %v", err)
	}
}
