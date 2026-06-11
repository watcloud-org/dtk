package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the top-level configuration for dtk.
type Config struct {
	// Listen is the address dtk listens on. Default: ":8080".
	Listen string `yaml:"listen"`
	// Groups is the list of service groups managed by dtk.
	Groups []Group `yaml:"groups"`
}

// Group describes a logical service made up of one or more Docker containers.
type Group struct {
	// Name is a human-readable identifier used in logs and the dashboard.
	Name string `yaml:"name"`
	// Host is the value of the HTTP Host header that routes traffic to this group
	// (e.g. "service.home" or "service.example.com").
	Host string `yaml:"host"`
	// Containers is the list of Docker container names belonging to this group.
	// All containers are paused/unpaused together.
	Containers []string `yaml:"containers"`
	// Target is the upstream URL requests are forwarded to when containers are running
	// (e.g. "http://service_app:9999").
	Target string `yaml:"target"`
	// IdleTimeout is how long the group must be idle (no requests) before dtk
	// pauses its containers. Default: 30m.
	IdleTimeout Duration `yaml:"idle_timeout"`
	// WakeDelay is how long dtk waits after unpausing containers before it
	// starts forwarding requests. Gives services time to fully initialise.
	// Default: 3s.
	WakeDelay Duration `yaml:"wake_delay"`
}

// Duration is a yaml-deserializable wrapper around time.Duration.
type Duration struct {
	time.Duration
}

func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return err
	}
	dur, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	d.Duration = dur
	return nil
}

func (d Duration) MarshalYAML() (interface{}, error) {
	return d.String(), nil
}

// Load reads and validates the config file at path.
func Load(path string) (*Config, error) {
	f, err := os.Open(path) // #nosec G304 — path is a CLI argument supplied by the operator
	if err != nil {
		return nil, fmt.Errorf("open config: %w", err)
	}
	defer f.Close()

	var cfg Config
	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	if err := validate(&cfg); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	applyDefaults(&cfg)
	return &cfg, nil
}

func applyDefaults(cfg *Config) {
	if cfg.Listen == "" {
		cfg.Listen = ":8080"
	}
	for i := range cfg.Groups {
		g := &cfg.Groups[i]
		if g.IdleTimeout.Duration == 0 {
			g.IdleTimeout.Duration = 30 * time.Minute
		}
		if g.WakeDelay.Duration == 0 {
			g.WakeDelay.Duration = 3 * time.Second
		}
	}
}

func validate(cfg *Config) error {
	if len(cfg.Groups) == 0 {
		return fmt.Errorf("at least one group must be defined")
	}
	hosts := make(map[string]struct{}, len(cfg.Groups))
	for i, g := range cfg.Groups {
		if g.Name == "" {
			return fmt.Errorf("groups[%d]: name is required", i)
		}
		if g.Host == "" {
			return fmt.Errorf("group %q: host is required", g.Name)
		}
		if g.Target == "" {
			return fmt.Errorf("group %q: target is required", g.Name)
		}
		if len(g.Containers) == 0 {
			return fmt.Errorf("group %q: at least one container must be listed", g.Name)
		}
		if _, dup := hosts[g.Host]; dup {
			return fmt.Errorf("group %q: duplicate host %q", g.Name, g.Host)
		}
		hosts[g.Host] = struct{}{}
	}
	return nil
}
