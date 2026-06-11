package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestNewLoggerLevels(t *testing.T) {
	for _, level := range []string{"debug", "warn", "error", "info", "unknown"} {
		l := newLogger(level)
		if l == nil {
			t.Fatalf("newLogger(%q) returned nil", level)
		}
		if enabled := l.Enabled(context.Background(), slog.LevelInfo); !enabled && level == "info" {
			t.Fatalf("logger at level %q should enable info", level)
		}
	}
}

func TestMainGracefulShutdown(t *testing.T) {
	sockPath, stopDocker := startFakeDockerSocket(t)
	defer stopDocker()

	cfgPath := writeMainConfig(t, "127.0.0.1:0")

	cmd := exec.Command(
		os.Args[0],
		"-test.run", "TestMainHelperProcess",
		"--",
		"-config", cfgPath,
		"-docker-socket", sockPath,
		"-log-level", "debug",
	)
	cmd.Env = append(os.Environ(), "GO_WANT_HELPER_PROCESS=1")

	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper process: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(out.String(), "dtk listening") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatalf("send SIGINT: %v", err)
	}

	err := cmd.Wait()
	if err != nil {
		t.Fatalf("helper process failed: %v\noutput:\n%s", err, out.String())
	}

	if !strings.Contains(out.String(), "goodbye") {
		t.Fatalf("expected graceful shutdown log, output:\n%s", out.String())
	}
}

func TestMainConfigLoadFailureExits(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.yaml")

	cmd := exec.Command(
		os.Args[0],
		"-test.run", "TestMainHelperProcess",
		"--",
		"-config", missing,
	)
	cmd.Env = append(os.Environ(), "GO_WANT_HELPER_PROCESS=1")

	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	err := cmd.Run()
	if err == nil {
		t.Fatal("expected non-zero exit code, got nil")
	}

	if !strings.Contains(err.Error(), "exit status") {
		t.Fatalf("error = %v, want exit status", err)
	}
	if !strings.Contains(out.String(), "failed to load config") {
		t.Fatalf("expected config error log, output:\n%s", out.String())
	}
}

func TestMainServerStartFailureExits(t *testing.T) {
	sockPath, stopDocker := startFakeDockerSocket(t)
	defer stopDocker()

	// Invalid listen address makes ListenAndServe return immediately.
	cfgPath := writeMainConfig(t, ":-1")

	cmd := exec.Command(
		os.Args[0],
		"-test.run", "TestMainHelperProcess",
		"--",
		"-config", cfgPath,
		"-docker-socket", sockPath,
	)
	cmd.Env = append(os.Environ(), "GO_WANT_HELPER_PROCESS=1")

	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	err := cmd.Run()
	if err == nil {
		t.Fatal("expected non-zero exit code, got nil")
	}
	if !strings.Contains(out.String(), "server error") {
		t.Fatalf("expected server error log, output:\n%s", out.String())
	}
}

func TestMainShutdownErrorPath(t *testing.T) {
	sockPath, stopDocker := startFakeDockerSocket(t)
	defer stopDocker()

	port := freeTCPPort(t)
	listenAddr := "127.0.0.1:" + strconv.Itoa(port)

	content := "listen: \"" + listenAddr + "\"\n" +
		"groups:\n" +
		"  - name: app\n" +
		"    host: app.example.com\n" +
		"    containers: [app]\n" +
		"    target: http://127.0.0.1:18080\n" +
		"    idle_timeout: 1h\n" +
		"    wake_delay: 30s\n"

	cfgPath := filepath.Join(t.TempDir(), "config-timeout.yaml")
	if err := os.WriteFile(cfgPath, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cmd := exec.Command(
		os.Args[0],
		"-test.run", "TestMainHelperProcess",
		"--",
		"-config", cfgPath,
		"-docker-socket", sockPath,
	)
	cmd.Env = append(os.Environ(), "GO_WANT_HELPER_PROCESS=1")

	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper process: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(out.String(), "dtk listening") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	requestErr := make(chan error, 1)
	go func() {
		req, err := http.NewRequest(http.MethodGet, "http://"+listenAddr+"/api", nil)
		if err != nil {
			requestErr <- err
			return
		}
		req.Host = "app.example.com"
		req.Header.Set("Accept", "application/json")

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			requestErr <- err
			return
		}
		defer resp.Body.Close()
		_, _ = io.ReadAll(resp.Body)
		requestErr <- nil
	}()

	time.Sleep(200 * time.Millisecond)

	if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatalf("send SIGINT: %v", err)
	}

	err := cmd.Wait()
	if err != nil {
		t.Fatalf("helper process failed: %v\noutput:\n%s", err, out.String())
	}

	if !strings.Contains(out.String(), "server shutdown error") {
		t.Fatalf("expected shutdown error log, output:\n%s", out.String())
	}

	select {
	case err := <-requestErr:
		if err != nil {
			var uerr *url.Error
			_ = errors.As(err, &uerr)
			if !strings.Contains(err.Error(), "EOF") && !strings.Contains(err.Error(), "connection") && (uerr == nil || uerr.Err == nil) {
				// Connection errors are expected during forced shutdown.
				t.Fatalf("request error = %v", err)
			}
		}
	case <-time.After(2 * time.Second):
		// Request may still be terminating while process exits.
	}
}

func TestMainHelperProcess(_ *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}

	args := os.Args
	for i, a := range args {
		if a == "--" {
			os.Args = append([]string{args[0]}, args[i+1:]...)
			break
		}
	}

	flag.CommandLine = flag.NewFlagSet(os.Args[0], flag.ExitOnError)
	flag.CommandLine.SetOutput(io.Discard)

	main()
	os.Exit(0)
}

func writeMainConfig(t *testing.T, listen string) string {
	t.Helper()

	content := "listen: \"" + listen + "\"\n" +
		"groups:\n" +
		"  - name: app\n" +
		"    host: app.example.com\n" +
		"    containers: [app]\n" +
		"    target: http://127.0.0.1:18080\n" +
		"    idle_timeout: 1h\n" +
		"    wake_delay: 0s\n"

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func startFakeDockerSocket(t *testing.T) (string, func()) {
	t.Helper()

	sockPath := filepath.Join(t.TempDir(), "docker.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen unix socket: %v", err)
	}

	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1.41/containers/json") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"Names":["/app"],"State":"paused"}]`))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	srv := &http.Server{Handler: h}
	go func() {
		_ = srv.Serve(ln)
	}()

	cleanup := func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}

	return sockPath, cleanup
}

func freeTCPPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen tcp: %v", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}
