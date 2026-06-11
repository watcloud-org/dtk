package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func writeConfigFile(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestLoadAppliesDefaults(t *testing.T) {
	path := writeConfigFile(t, `
groups:
  - name: app
    host: app.example.com
    containers: [app]
    target: http://app:8080
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if cfg.Listen != ":8080" {
		t.Fatalf("Listen = %q, want :8080", cfg.Listen)
	}
	if len(cfg.Groups) != 1 {
		t.Fatalf("groups len = %d, want 1", len(cfg.Groups))
	}
	g := cfg.Groups[0]
	if g.IdleTimeout.Duration != 30*time.Minute {
		t.Fatalf("idle timeout = %s, want 30m", g.IdleTimeout.Duration)
	}
	if g.WakeDelay.Duration != 3*time.Second {
		t.Fatalf("wake delay = %s, want 3s", g.WakeDelay.Duration)
	}
}

func TestLoadRespectsConfiguredDurations(t *testing.T) {
	path := writeConfigFile(t, `
listen: ":9090"
groups:
  - name: app
    host: app.example.com
    containers: [app]
    target: http://app:8080
    idle_timeout: 45m
    wake_delay: 7s
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if cfg.Listen != ":9090" {
		t.Fatalf("Listen = %q, want :9090", cfg.Listen)
	}
	g := cfg.Groups[0]
	if g.IdleTimeout.Duration != 45*time.Minute {
		t.Fatalf("idle timeout = %s, want 45m", g.IdleTimeout.Duration)
	}
	if g.WakeDelay.Duration != 7*time.Second {
		t.Fatalf("wake delay = %s, want 7s", g.WakeDelay.Duration)
	}
}

func TestLoadValidationErrors(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantSub string
	}{
		{
			name:    "no groups",
			yaml:    "listen: :8080\n",
			wantSub: "at least one group",
		},
		{
			name: "missing name",
			yaml: `
groups:
  - host: app.example.com
    containers: [app]
    target: http://app:8080
`,
			wantSub: "name is required",
		},
		{
			name: "missing host",
			yaml: `
groups:
  - name: app
    containers: [app]
    target: http://app:8080
`,
			wantSub: "host is required",
		},
		{
			name: "duplicate host",
			yaml: `
groups:
  - name: one
    host: app.example.com
    containers: [c1]
    target: http://one:8080
  - name: two
    host: app.example.com
    containers: [c2]
    target: http://two:8080
`,
			wantSub: "duplicate host",
		},
		{
			name: "missing target",
			yaml: `
groups:
  - name: app
    host: app.example.com
    containers: [app]
`,
			wantSub: "target is required",
		},
		{
			name: "no containers",
			yaml: `
groups:
  - name: app
    host: app.example.com
    containers: []
    target: http://app:8080
`,
			wantSub: "at least one container",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := writeConfigFile(t, tc.yaml)
			_, err := Load(path)
			if err == nil {
				t.Fatal("Load() error = nil, want error")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("error = %q, want substring %q", err.Error(), tc.wantSub)
			}
		})
	}
}

func TestLoadUnknownFieldFails(t *testing.T) {
	path := writeConfigFile(t, `
groups:
  - name: app
    host: app.example.com
    containers: [app]
    target: http://app:8080
    not_a_field: true
`)

	_, err := Load(path)
	if err == nil {
		t.Fatal("Load() error = nil, want error")
	}
	if !strings.Contains(err.Error(), "field not found") {
		t.Fatalf("error = %q, want unknown field error", err.Error())
	}
}

func TestLoadMissingFile(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "missing.yaml"))
	if err == nil {
		t.Fatal("Load() error = nil, want error")
	}
	if !strings.Contains(err.Error(), "open config") {
		t.Fatalf("error = %q, want open config", err.Error())
	}
}

func TestDurationUnmarshalYAML(t *testing.T) {
	var d Duration
	if err := yaml.Unmarshal([]byte("2m30s"), &d); err != nil {
		t.Fatalf("yaml.Unmarshal() error = %v", err)
	}
	if d.Duration != 2*time.Minute+30*time.Second {
		t.Fatalf("duration = %s, want 2m30s", d.Duration)
	}
}

func TestDurationUnmarshalYAMLInvalid(t *testing.T) {
	var d Duration
	err := yaml.Unmarshal([]byte("not-duration"), &d)
	if err == nil {
		t.Fatal("yaml.Unmarshal() error = nil, want error")
	}
	if !strings.Contains(err.Error(), "invalid duration") {
		t.Fatalf("error = %q, want invalid duration", err.Error())
	}
}

func TestDurationUnmarshalYAMLTypeError(t *testing.T) {
	var d Duration
	err := yaml.Unmarshal([]byte("{k: v}"), &d)
	if err == nil {
		t.Fatal("yaml.Unmarshal() error = nil, want error")
	}
	if !strings.Contains(err.Error(), "cannot unmarshal") {
		t.Fatalf("error = %q, want cannot unmarshal", err.Error())
	}
}

func TestDurationMarshalYAML(t *testing.T) {
	d := Duration{Duration: 90 * time.Second}
	b, err := yaml.Marshal(d)
	if err != nil {
		t.Fatalf("yaml.Marshal() error = %v", err)
	}
	if got := strings.TrimSpace(string(b)); got != "1m30s" {
		t.Fatalf("marshal output = %q, want 1m30s", got)
	}
}
