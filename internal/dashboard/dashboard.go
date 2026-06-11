// Package dashboard renders the /_dtk/status HTML page.
package dashboard

import (
	_ "embed"
	"html/template"
	"net/http"
	"strings"
	"time"

	"github.com/ylecuyer/dtk/internal/manager"
)

//go:embed status.html
var statusHTML string

var statusTmpl = template.Must(template.New("status").Funcs(template.FuncMap{
	"stateClass": func(s manager.State) string {
		switch s {
		case manager.StateRunning:
			return "running"
		case manager.StateWaking:
			return "waking"
		case manager.StatePausing:
			return "pausing"
		default:
			return "paused"
		}
	},
	"join": strings.Join,
	"since": func(t time.Time) string {
		if t.IsZero() {
			return "never"
		}
		d := time.Since(t).Truncate(time.Second)
		return d.String() + " ago"
	},
	"humanDuration": func(d time.Duration) string {
		return d.String()
	},
}).Parse(statusHTML))

// Mgr is the interface the dashboard uses to read group state.
type Mgr interface {
	Info() []manager.GroupInfo
}

// Handler returns an http.Handler that serves the status dashboard.
func Handler(mgr Mgr) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")

		data := struct {
			Groups    []manager.GroupInfo
			Timestamp string
		}{
			Groups:    mgr.Info(),
			Timestamp: time.Now().Format("2006-01-02 15:04:05 MST"),
		}

		if err := statusTmpl.Execute(w, data); err != nil {
			http.Error(w, "template error", http.StatusInternalServerError)
		}
	})
}
