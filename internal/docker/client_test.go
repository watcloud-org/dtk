package docker

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func testClient(rt roundTripFunc) *Client {
	return &Client{
		http:    &http.Client{Transport: rt},
		apiBase: "http://localhost/v1.41",
	}
}

func response(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
	}
}

func TestNewDefaultsAndClose(t *testing.T) {
	c := New("")
	if c.socketPath != "/var/run/docker.sock" {
		t.Fatalf("socketPath = %q, want /var/run/docker.sock", c.socketPath)
	}
	if c.apiBase != "http://localhost/v1.41" {
		t.Fatalf("apiBase = %q, want http://localhost/v1.41", c.apiBase)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
}

func TestNewUsesProvidedSocketPath(t *testing.T) {
	c := New("/tmp/custom.sock")
	if c.socketPath != "/tmp/custom.sock" {
		t.Fatalf("socketPath = %q, want /tmp/custom.sock", c.socketPath)
	}
}

func TestNewDialContextWorksWithUnixSocket(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "docker.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	defer os.Remove(sockPath)

	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1.41/containers/json") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[]`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()

	go func() {
		_ = srv.Serve(ln)
	}()

	c := New(sockPath)

	states, err := c.ContainerStates(context.Background(), []string{"app"})
	if err != nil {
		t.Fatalf("ContainerStates() error = %v", err)
	}
	if len(states) != 0 {
		t.Fatalf("states len = %d, want 0", len(states))
	}
}

func TestContainerStatesSuccess(t *testing.T) {
	c := testClient(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodGet {
			t.Fatalf("method = %s, want GET", req.Method)
		}
		if !strings.Contains(req.URL.String(), "/containers/json") {
			t.Fatalf("url = %s, want /containers/json", req.URL.String())
		}
		body := `[{"Names":["/app","/alias"],"State":"running"},{"Names":["/db"],"State":"paused"}]`
		return response(http.StatusOK, body), nil
	})

	got, err := c.ContainerStates(context.Background(), []string{"app", "db", "missing"})
	if err != nil {
		t.Fatalf("ContainerStates() error = %v", err)
	}
	if got["app"] != "running" {
		t.Fatalf("state app = %q, want running", got["app"])
	}
	if got["db"] != "paused" {
		t.Fatalf("state db = %q, want paused", got["db"])
	}
	if _, ok := got["missing"]; ok {
		t.Fatal("missing container should not exist in result")
	}
}

func TestContainerStatesNameEdgeCases(t *testing.T) {
	c := testClient(func(*http.Request) (*http.Response, error) {
		body := `[{"Names":["app",""],"State":"running"}]`
		return response(http.StatusOK, body), nil
	})

	got, err := c.ContainerStates(context.Background(), []string{"app"})
	if err != nil {
		t.Fatalf("ContainerStates() error = %v", err)
	}
	if got["app"] != "running" {
		t.Fatalf("state app = %q, want running", got["app"])
	}
}

func TestContainerStatesRequestAndResponseErrors(t *testing.T) {
	t.Run("request build", func(t *testing.T) {
		c := testClient(func(req *http.Request) (*http.Response, error) {
			t.Fatalf("unexpected request: %s", req.URL.String())
			return nil, nil
		})
		c.apiBase = "http://bad\n"

		_, err := c.ContainerStates(context.Background(), []string{"app"})
		if err == nil || !strings.Contains(err.Error(), "docker list request") {
			t.Fatalf("error = %v, want docker list request", err)
		}
	})

	t.Run("do error", func(t *testing.T) {
		c := testClient(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("boom")
		})

		_, err := c.ContainerStates(context.Background(), []string{"app"})
		if err == nil || !strings.Contains(err.Error(), "docker list") {
			t.Fatalf("error = %v, want docker list", err)
		}
	})

	t.Run("http status", func(t *testing.T) {
		c := testClient(func(*http.Request) (*http.Response, error) {
			return response(http.StatusInternalServerError, "failure"), nil
		})

		_, err := c.ContainerStates(context.Background(), []string{"app"})
		if err == nil || !strings.Contains(err.Error(), "HTTP 500") {
			t.Fatalf("error = %v, want HTTP 500", err)
		}
	})

	t.Run("decode", func(t *testing.T) {
		c := testClient(func(*http.Request) (*http.Response, error) {
			return response(http.StatusOK, "not-json"), nil
		})

		_, err := c.ContainerStates(context.Background(), []string{"app"})
		if err == nil || !strings.Contains(err.Error(), "docker list decode") {
			t.Fatalf("error = %v, want docker list decode", err)
		}
	})
}

func TestPauseGroupBehavior(t *testing.T) {
	var actions []string
	c := testClient(func(req *http.Request) (*http.Response, error) {
		switch {
		case req.Method == http.MethodGet && strings.Contains(req.URL.Path, "/containers/json"):
			body := `[{"Names":["/run"],"State":"running"},{"Names":["/paused"],"State":"paused"}]`
			return response(http.StatusOK, body), nil
		case req.Method == http.MethodPost:
			actions = append(actions, req.URL.Path)
			return response(http.StatusNoContent, ""), nil
		default:
			t.Fatalf("unexpected request: %s %s", req.Method, req.URL.Path)
			return nil, nil
		}
	})

	err := c.PauseGroup(context.Background(), []string{"run", "paused", "missing"})
	if err != nil {
		t.Fatalf("PauseGroup() error = %v", err)
	}
	if len(actions) != 1 || !strings.HasSuffix(actions[0], "/containers/run/pause") {
		t.Fatalf("actions = %v, want only run pause", actions)
	}
}

func TestPauseGroupErrors(t *testing.T) {
	t.Run("state lookup", func(t *testing.T) {
		c := testClient(func(*http.Request) (*http.Response, error) {
			return response(http.StatusBadGateway, "bad"), nil
		})

		err := c.PauseGroup(context.Background(), []string{"run"})
		if err == nil {
			t.Fatal("PauseGroup() error = nil, want error")
		}
	})

	t.Run("action error", func(t *testing.T) {
		c := testClient(func(req *http.Request) (*http.Response, error) {
			if req.Method == http.MethodGet {
				return response(http.StatusOK, `[{"Names":["/run"],"State":"running"}]`), nil
			}
			return response(http.StatusInternalServerError, "nope"), nil
		})

		err := c.PauseGroup(context.Background(), []string{"run"})
		if err == nil || !strings.Contains(err.Error(), `pause "run"`) {
			t.Fatalf("error = %v, want pause run", err)
		}
	})
}

func TestUnpauseAndRestartGroupBehavior(t *testing.T) {
	t.Run("unpause paused only", func(t *testing.T) {
		var actions []string
		c := testClient(func(req *http.Request) (*http.Response, error) {
			if req.Method == http.MethodGet {
				return response(http.StatusOK, `[{"Names":["/a"],"State":"paused"},{"Names":["/b"],"State":"running"}]`), nil
			}
			actions = append(actions, req.URL.Path)
			return response(http.StatusNoContent, ""), nil
		})

		err := c.UnpauseGroup(context.Background(), []string{"a", "b", "missing"})
		if err != nil {
			t.Fatalf("UnpauseGroup() error = %v", err)
		}
		if len(actions) != 1 || !strings.HasSuffix(actions[0], "/containers/a/unpause") {
			t.Fatalf("actions = %v, want only a unpause", actions)
		}
	})

	t.Run("restart existing only", func(t *testing.T) {
		var actions []string
		c := testClient(func(req *http.Request) (*http.Response, error) {
			if req.Method == http.MethodGet {
				return response(http.StatusOK, `[{"Names":["/a"],"State":"paused"},{"Names":["/b"],"State":"running"}]`), nil
			}
			actions = append(actions, req.URL.Path)
			return response(http.StatusNoContent, ""), nil
		})

		err := c.RestartGroup(context.Background(), []string{"a", "b", "missing"})
		if err != nil {
			t.Fatalf("RestartGroup() error = %v", err)
		}
		if len(actions) != 2 {
			t.Fatalf("actions = %v, want 2 restart calls", actions)
		}
	})
}

func TestUnpauseAndRestartErrors(t *testing.T) {
	t.Run("unpause state lookup error", func(t *testing.T) {
		c := testClient(func(*http.Request) (*http.Response, error) {
			return response(http.StatusBadGateway, "bad"), nil
		})

		err := c.UnpauseGroup(context.Background(), []string{"a"})
		if err == nil {
			t.Fatal("UnpauseGroup() error = nil, want error")
		}
	})

	t.Run("restart state lookup error", func(t *testing.T) {
		c := testClient(func(*http.Request) (*http.Response, error) {
			return response(http.StatusBadGateway, "bad"), nil
		})

		err := c.RestartGroup(context.Background(), []string{"a"})
		if err == nil {
			t.Fatal("RestartGroup() error = nil, want error")
		}
	})

	t.Run("unpause action error", func(t *testing.T) {
		c := testClient(func(req *http.Request) (*http.Response, error) {
			if req.Method == http.MethodGet {
				return response(http.StatusOK, `[{"Names":["/a"],"State":"paused"}]`), nil
			}
			return response(http.StatusInternalServerError, "fail"), nil
		})

		err := c.UnpauseGroup(context.Background(), []string{"a"})
		if err == nil || !strings.Contains(err.Error(), `unpause "a"`) {
			t.Fatalf("error = %v, want unpause a", err)
		}
	})

	t.Run("restart action error", func(t *testing.T) {
		c := testClient(func(req *http.Request) (*http.Response, error) {
			if req.Method == http.MethodGet {
				return response(http.StatusOK, `[{"Names":["/a"],"State":"running"}]`), nil
			}
			return response(http.StatusInternalServerError, "fail"), nil
		})

		err := c.RestartGroup(context.Background(), []string{"a"})
		if err == nil || !strings.Contains(err.Error(), `restart "a"`) {
			t.Fatalf("error = %v, want restart a", err)
		}
	})
}

func TestContainerActionBranches(t *testing.T) {
	t.Run("request build", func(t *testing.T) {
		c := testClient(func(req *http.Request) (*http.Response, error) {
			t.Fatalf("unexpected request: %s", req.URL.String())
			return nil, nil
		})
		c.apiBase = "http://bad\n"

		err := c.containerAction(context.Background(), "app", "pause")
		if err == nil {
			t.Fatal("containerAction() error = nil, want error")
		}
	})

	t.Run("do error", func(t *testing.T) {
		c := testClient(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("boom")
		})

		err := c.containerAction(context.Background(), "app", "pause")
		if err == nil {
			t.Fatal("containerAction() error = nil, want error")
		}
	})

	t.Run("non 204", func(t *testing.T) {
		c := testClient(func(*http.Request) (*http.Response, error) {
			return response(http.StatusConflict, "bad state"), nil
		})

		err := c.containerAction(context.Background(), "app", "pause")
		if err == nil || !strings.Contains(err.Error(), "HTTP 409") {
			t.Fatalf("error = %v, want HTTP 409", err)
		}
	})

	t.Run("success", func(t *testing.T) {
		c := testClient(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusNoContent,
				Body:       io.NopCloser(bytes.NewBuffer(nil)),
				Header:     make(http.Header),
			}, nil
		})

		if err := c.containerAction(context.Background(), "app", "pause"); err != nil {
			t.Fatalf("containerAction() error = %v", err)
		}
	})
}
