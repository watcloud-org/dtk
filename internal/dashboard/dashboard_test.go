package dashboard

import (
	"html/template"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ylecuyer/dtk/internal/manager"
)

type fakeMgr struct {
	infos []manager.GroupInfo
}

func (f *fakeMgr) Info() []manager.GroupInfo {
	out := make([]manager.GroupInfo, len(f.infos))
	copy(out, f.infos)
	return out
}

func TestHandlerRendersDashboardAndHeaders(t *testing.T) {
	mgr := &fakeMgr{infos: []manager.GroupInfo{
		{
			Name:        "run",
			Host:        "run.example.com",
			Containers:  []string{"a", "b"},
			State:       manager.StateRunning,
			LastRequest: time.Now().Add(-5 * time.Second),
			IdleTimeout: 30 * time.Minute,
			WakeDelay:   2 * time.Second,
		},
		{
			Name:        "wake",
			Host:        "wake.example.com",
			Containers:  []string{"c"},
			State:       manager.StateWaking,
			LastRequest: time.Now().Add(-6 * time.Second),
			IdleTimeout: 15 * time.Minute,
			WakeDelay:   time.Second,
		},
		{
			Name:        "pause",
			Host:        "pause.example.com",
			Containers:  []string{"d"},
			State:       manager.StatePausing,
			LastRequest: time.Time{},
			IdleTimeout: 10 * time.Minute,
			WakeDelay:   time.Second,
		},
		{
			Name:        "default-paused",
			Host:        "paused.example.com",
			Containers:  []string{"e"},
			State:       manager.State(99),
			LastRequest: time.Time{},
			IdleTimeout: 5 * time.Minute,
			WakeDelay:   time.Second,
		},
	}}

	h := Handler(mgr)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://example.com/_dtk/status", nil)

	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if got := rr.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
		t.Fatalf("content-type = %q, want text/html; charset=utf-8", got)
	}
	if got := rr.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("cache-control = %q, want no-store", got)
	}

	body := rr.Body.String()
	for _, want := range []string{
		"Docker Time Keeper",
		"running",
		"waking",
		"pausing",
		"paused",
		"never",
		"ago",
		"30m0s",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("dashboard body missing %q", want)
		}
	}
}

func TestHandlerTemplateExecuteError(t *testing.T) {
	orig := statusTmpl
	defer func() { statusTmpl = orig }()

	// Missing field access causes Execute to return an error.
	statusTmpl = template.Must(template.New("broken").Parse("{{.Missing.Field}}"))

	h := Handler(&fakeMgr{})
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://example.com/_dtk/status", nil)

	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "template error") {
		t.Fatalf("body = %q, want template error", rr.Body.String())
	}
}
