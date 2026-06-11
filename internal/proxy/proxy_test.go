package proxy

import (
	"context"
	"errors"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ylecuyer/dtk/internal/config"
	"github.com/ylecuyer/dtk/internal/manager"
)

type fakeManager struct {
	mu sync.Mutex

	infos      []manager.GroupInfo
	wakeResult bool
	wakeCalls  []string
	restartErr error
	restarts   []string
}

func (f *fakeManager) WakeAndWait(_ context.Context, host string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.wakeCalls = append(f.wakeCalls, host)
	if !f.wakeResult {
		return false
	}
	return true
}

func (f *fakeManager) Info() []manager.GroupInfo {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]manager.GroupInfo, len(f.infos))
	copy(out, f.infos)
	return out
}

func (f *fakeManager) Restart(_ context.Context, host string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.restarts = append(f.restarts, host)
	return f.restartErr
}

func (f *fakeManager) wakeCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.wakeCalls)
}

func (f *fakeManager) restartHosts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.restarts))
	copy(out, f.restarts)
	return out
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func testGroup(target string) config.Group {
	return config.Group{
		Name:       "app",
		Host:       "app.example.com",
		Containers: []string{"app"},
		Target:     target,
	}
}

func TestServeHTTPUnknownHost(t *testing.T) {
	fm := &fakeManager{wakeResult: true}
	s := New([]config.Group{}, fm, testLogger())

	req := httptest.NewRequest(http.MethodGet, "http://unknown.example.com/", nil)
	req.Host = "unknown.example.com"
	rr := httptest.NewRecorder()

	s.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}

func TestServeHTTPStatusPathUsesDashboard(t *testing.T) {
	fm := &fakeManager{
		wakeResult: true,
		infos: []manager.GroupInfo{{
			Name: "app", Host: "app.example.com", State: manager.StateRunning,
		}},
	}
	s := New([]config.Group{testGroup("http://127.0.0.1")}, fm, testLogger())

	req := httptest.NewRequest(http.MethodGet, "http://app.example.com/_dtk/status", nil)
	req.Host = "app.example.com"
	rr := httptest.NewRecorder()

	s.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "Docker Time Keeper") {
		t.Fatalf("response missing dashboard title, body=%q", body)
	}
}

func TestServeHTTPBrowserGetsLoadingPageWhenPaused(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer upstream.Close()

	fm := &fakeManager{
		wakeResult: true,
		infos: []manager.GroupInfo{{
			Name: "app", Host: "app.example.com", State: manager.StatePaused,
		}},
	}
	s := New([]config.Group{testGroup(upstream.URL)}, fm, testLogger())

	req := httptest.NewRequest(http.MethodGet, "http://app.example.com/", nil)
	req.Host = "app.example.com"
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	rr := httptest.NewRecorder()

	s.ServeHTTP(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "Starting") {
		t.Fatalf("response missing loading content, body=%q", body)
	}

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if fm.wakeCallCount() > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("expected background wake call for loading page")
}

func TestServeHTTPAPICallsWakeAndForwards(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("upstream:" + r.URL.Path))
	}))
	defer upstream.Close()

	fm := &fakeManager{
		wakeResult: true,
		infos: []manager.GroupInfo{{
			Name: "app", Host: "app.example.com", State: manager.StatePaused,
		}},
	}
	s := New([]config.Group{testGroup(upstream.URL)}, fm, testLogger())

	req := httptest.NewRequest(http.MethodGet, "http://app.example.com/api/v1", nil)
	req.Host = "app.example.com:443"
	req.Header.Set("Accept", "application/json")
	rr := httptest.NewRecorder()

	s.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if got := rr.Body.String(); !strings.Contains(got, "upstream:/api/v1") {
		t.Fatalf("body = %q, want forwarded response", got)
	}
	if fm.wakeCallCount() == 0 {
		t.Fatal("expected WakeAndWait to be called")
	}
}

func TestHandleRestartMethodNotAllowed(t *testing.T) {
	fm := &fakeManager{wakeResult: true}
	s := New(nil, fm, testLogger())

	req := httptest.NewRequest(http.MethodGet, "http://any/_dtk/restart", nil)
	req.Host = "app.example.com"
	rr := httptest.NewRecorder()

	s.ServeHTTP(rr, req)

	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rr.Code)
	}
}

func TestHandleRestartMissingHost(t *testing.T) {
	fm := &fakeManager{wakeResult: true}
	s := New(nil, fm, testLogger())

	req := httptest.NewRequest(http.MethodPost, "http://any/_dtk/restart", nil)
	req.Host = "app.example.com"
	rr := httptest.NewRecorder()

	s.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}

func TestHandleRestartErrorMapping(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		status int
	}{
		{name: "unknown host", err: manager.ErrUnknownHost, status: http.StatusNotFound},
		{name: "deadline", err: context.DeadlineExceeded, status: http.StatusGatewayTimeout},
		{name: "other", err: errors.New("boom"), status: http.StatusInternalServerError},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fm := &fakeManager{wakeResult: true, restartErr: tc.err}
			s := New(nil, fm, testLogger())

			req := httptest.NewRequest(http.MethodPost, "http://any/_dtk/restart", strings.NewReader("host=app.example.com"))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.Host = "app.example.com"
			rr := httptest.NewRecorder()

			s.ServeHTTP(rr, req)

			if rr.Code != tc.status {
				t.Fatalf("status = %d, want %d", rr.Code, tc.status)
			}
		})
	}
}

func TestHandleRestartSuccessRedirects(t *testing.T) {
	fm := &fakeManager{wakeResult: true}
	s := New(nil, fm, testLogger())

	req := httptest.NewRequest(http.MethodPost, "http://any/_dtk/restart", strings.NewReader("host=app.example.com"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Host = "app.example.com"
	rr := httptest.NewRecorder()

	s.ServeHTTP(rr, req)

	if rr.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rr.Code)
	}
	if loc := rr.Header().Get("Location"); loc != "/_dtk/status" {
		t.Fatalf("location = %q, want /_dtk/status", loc)
	}
	hosts := fm.restartHosts()
	if len(hosts) != 1 || hosts[0] != "app.example.com" {
		t.Fatalf("restart hosts = %v, want [app.example.com]", hosts)
	}
}

func TestNewPanicsOnInvalidTargetURL(t *testing.T) {
	fm := &fakeManager{wakeResult: true}
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("New() did not panic for invalid target URL")
		}
	}()

	_ = New([]config.Group{testGroup("://bad-url")}, fm, testLogger())
}

func TestReverseProxyErrorHandlerReturnsBadGateway(t *testing.T) {
	fm := &fakeManager{wakeResult: true}
	s := New([]config.Group{testGroup("http://127.0.0.1:1")}, fm, testLogger())

	req := httptest.NewRequest(http.MethodGet, "http://app.example.com/api", nil)
	req.Host = "app.example.com"
	rr := httptest.NewRecorder()

	s.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rr.Code)
	}
}

func TestIsGroupPausedFalseWhenRunningOrMissing(t *testing.T) {
	fm := &fakeManager{
		wakeResult: true,
		infos:      []manager.GroupInfo{{Name: "app", Host: "app.example.com", State: manager.StateRunning}},
	}
	s := New([]config.Group{testGroup("http://127.0.0.1")}, fm, testLogger())

	if got := s.isGroupPaused("app.example.com"); got {
		t.Fatal("isGroupPaused(running) = true, want false")
	}
	if got := s.isGroupPaused("missing.example.com"); got {
		t.Fatal("isGroupPaused(missing) = true, want false")
	}
}

func TestServeLoadingPageFallsBackToHostName(t *testing.T) {
	fm := &fakeManager{wakeResult: true}
	s := New([]config.Group{testGroup("http://127.0.0.1")}, fm, testLogger())

	req := httptest.NewRequest(http.MethodGet, "http://other.example.com/", nil)
	req.Host = "other.example.com"
	rr := httptest.NewRecorder()

	s.serveLoadingPage(rr, req, "other.example.com")

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "other.example.com") {
		t.Fatalf("body = %q, want host fallback", rr.Body.String())
	}
}

func TestServeHTTPUsesProxyMapEntry(t *testing.T) {
	fm := &fakeManager{wakeResult: true}
	s := New([]config.Group{testGroup("http://127.0.0.1")}, fm, testLogger())

	hit := false
	s.proxies["app.example.com"] = &httputil.ReverseProxy{
		Director: func(*http.Request) {},
		Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			hit = true
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader("ok")),
			}, nil
		}),
	}

	req := httptest.NewRequest(http.MethodGet, "http://app.example.com/", nil)
	req.Host = "app.example.com"
	req.Header.Set("Accept", "application/json")
	rr := httptest.NewRecorder()

	s.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if !hit {
		t.Fatal("expected custom reverse proxy transport to be called")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestServeLoadingPageTemplateErrorPath(t *testing.T) {
	original := loadingTmpl
	loadingTmpl = template.Must(template.New("loading").Option("missingkey=error").Parse("{{.Missing}}"))
	t.Cleanup(func() {
		loadingTmpl = original
	})

	fm := &fakeManager{wakeResult: true}
	s := New([]config.Group{testGroup("http://127.0.0.1")}, fm, testLogger())

	req := httptest.NewRequest(http.MethodGet, "http://app.example.com/", nil)
	req.Host = "app.example.com"
	rr := httptest.NewRecorder()

	s.serveLoadingPage(rr, req, "app.example.com")

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rr.Code)
	}
}
