// Package proxy is the HTTP layer of dtk.
// It routes requests by Host header, wakes sleeping container groups, and
// forwards requests to the configured upstream via a reverse proxy.
// Browser requests (Accept: text/html) for sleeping groups get a loading page
// instead of holding the connection.
package proxy

import (
	"context"
	_ "embed"
	"errors"
	"html/template"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"github.com/ylecuyer/dtk/internal/config"
	"github.com/ylecuyer/dtk/internal/dashboard"
	"github.com/ylecuyer/dtk/internal/manager"
)

//go:embed loading.html
var loadingHTML string

var loadingTmpl = template.Must(template.New("loading").Parse(loadingHTML))

// Mgr is the subset of *manager.Manager used by the proxy.
type Mgr interface {
	WakeAndWait(ctx context.Context, host string) bool
	Info() []manager.GroupInfo
	Restart(ctx context.Context, host string) error
}

// Server is the dtk HTTP server.
type Server struct {
	cfg     []config.Group
	mgr     Mgr
	proxies map[string]*httputil.ReverseProxy // keyed by host
	dash    http.Handler
	log     *slog.Logger
}

// New creates an HTTP server that routes, wakes, and forwards requests.
func New(cfg []config.Group, mgr Mgr, log *slog.Logger) *Server {
	proxies := make(map[string]*httputil.ReverseProxy, len(cfg))
	for _, g := range cfg {
		g := g
		target, err := url.Parse(g.Target)
		if err != nil {
			// Validated at config load time; this should not happen.
			panic("invalid target URL for group " + g.Name + ": " + err.Error())
		}
		rp := httputil.NewSingleHostReverseProxy(target)
		rp.ErrorLog = nil // use slog instead
		rp.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
			log.Error("upstream error", "host", r.Host, "path", r.URL.Path, "err", err)
			http.Error(w, "upstream error", http.StatusBadGateway)
		}
		proxies[g.Host] = rp
	}

	return &Server{
		cfg:     cfg,
		mgr:     mgr,
		proxies: proxies,
		dash:    dashboard.Handler(mgr),
		log:     log,
	}
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Strip port from Host header.
	host := r.Host
	if idx := strings.LastIndex(host, ":"); idx != -1 {
		host = host[:idx]
	}

	if r.URL.Path == "/_dtk/restart" {
		s.handleRestart(w, r)
		return
	}

	// Internal dashboard: any host, path prefix /_dtk/
	if strings.HasPrefix(r.URL.Path, "/_dtk/") {
		s.dash.ServeHTTP(w, r)
		return
	}

	rp, ok := s.proxies[host]
	if !ok {
		s.log.Warn("unknown host", "host", host)
		http.Error(w, "unknown host", http.StatusNotFound)
		return
	}

	// If group is paused and caller is a browser, return a loading page.
	if isBrowser(r) && s.isGroupPaused(host) {
		s.serveLoadingPage(w, r, host)
		return
	}

	// Wait for group to be running (blocks until ready).
	s.mgr.WakeAndWait(r.Context(), host)

	// Forward to upstream.
	rp.ServeHTTP(w, r)
}

// isGroupPaused reports whether the group for host is currently paused.
func (s *Server) isGroupPaused(host string) bool {
	for _, info := range s.mgr.Info() {
		if info.Host == host {
			return info.State == manager.StatePaused
		}
	}
	return false
}

// isBrowser returns true when the request's Accept header suggests a browser.
func isBrowser(r *http.Request) bool {
	accept := r.Header.Get("Accept")
	return strings.Contains(accept, "text/html")
}

// serveLoadingPage renders the loading HTML page with a meta-refresh.
func (s *Server) serveLoadingPage(w http.ResponseWriter, _ *http.Request, host string) {
	// Kick off the wake in the background so the containers start warming up.
	go func() {
		ctx := context.Background()
		s.mgr.WakeAndWait(ctx, host)
	}()

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusServiceUnavailable)

	groupName := host
	for _, g := range s.cfg {
		if g.Host == host {
			groupName = g.Name
			break
		}
	}

	data := struct {
		GroupName string
		Host      string
	}{GroupName: groupName, Host: host}

	if err := loadingTmpl.Execute(w, data); err != nil {
		s.log.Error("loading template error", "err", err)
	}
}

func (s *Server) handleRestart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	host := r.FormValue("host")
	if host == "" {
		http.Error(w, "missing host", http.StatusBadRequest)
		return
	}

	if err := s.mgr.Restart(r.Context(), host); err != nil {
		s.log.Error("restart failed", "host", host, "err", err)
		switch {
		case errors.Is(err, manager.ErrUnknownHost):
			http.Error(w, "unknown host", http.StatusNotFound)
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			http.Error(w, "restart timed out", http.StatusGatewayTimeout)
		default:
			http.Error(w, "restart failed", http.StatusInternalServerError)
		}
		return
	}

	http.Redirect(w, r, "/_dtk/status", http.StatusSeeOther)
}
