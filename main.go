package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ylecuyer/dtk/internal/config"
	"github.com/ylecuyer/dtk/internal/docker"
	"github.com/ylecuyer/dtk/internal/manager"
	"github.com/ylecuyer/dtk/internal/proxy"
)

func main() {
	cfgPath := flag.String("config", "/etc/dtk/config.yaml", "path to config file")
	dockerSocket := flag.String("docker-socket", "/var/run/docker.sock", "path to Docker socket")
	logLevel := flag.String("log-level", "info", "log level: debug, info, warn, error")
	flag.Parse()

	log := newLogger(*logLevel)

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Error("failed to load config", "err", err)
		os.Exit(1)
	}
	log.Info("config loaded", "groups", len(cfg.Groups), "listen", cfg.Listen)

	dc := docker.New(*dockerSocket)
	defer dc.Close()

	mgr := manager.New(cfg.Groups, dc, log)

	srv := &http.Server{
		Addr:         cfg.Listen,
		Handler:      proxy.New(cfg.Groups, mgr, log),
		ReadTimeout:  120 * time.Second,
		WriteTimeout: 0, // disabled: responses may take a long time while waking
		IdleTimeout:  120 * time.Second,
	}

	// Start server.
	go func() {
		log.Info("dtk listening", "addr", cfg.Listen)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error("server error", "err", err)
			os.Exit(1)
		}
	}()

	// Graceful shutdown on SIGINT / SIGTERM.
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Error("server shutdown error", "err", err)
	}
	mgr.Shutdown(shutdownCtx)
	log.Info("goodbye")
}

func newLogger(level string) *slog.Logger {
	var l slog.Level
	switch level {
	case "debug":
		l = slog.LevelDebug
	case "warn":
		l = slog.LevelWarn
	case "error":
		l = slog.LevelError
	default:
		l = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: l}))
}
