package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"interim/internal/config"
	"interim/internal/objects"
	"interim/internal/server"
	"interim/internal/store"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		slog.Error("configuration error", "error", err)
		os.Exit(1)
	}

	level := slog.LevelInfo
	switch cfg.LogLevel {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(ctx, cfg.DatabasePath)
	if err != nil {
		logger.Error("open database", "error", err)
		os.Exit(1)
	}
	defer st.Close()

	if err := st.ResetForStartup(ctx); err != nil {
		logger.Error("prepare database", "error", err)
		os.Exit(1)
	}

	storage, err := objects.NewS3(ctx, cfg, logger)
	if err != nil {
		logger.Error("connect object storage", "error", err)
		os.Exit(1)
	}

	app, err := server.New(cfg, st, storage, logger)
	if err != nil {
		logger.Error("initialise server", "error", err)
		os.Exit(1)
	}

	app.MaintenanceOnce(ctx)
	go app.RunMaintenance(ctx)

	httpServer := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           app.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("interim starting",
			"url", cfg.PublicBaseURL,
			"listen", cfg.ListenAddr,
			"storage_limit", cfg.MaxStorage,
			"max_file_size", cfg.MaxFileSize,
			"retention", cfg.Retention.String(),
			"bucket", cfg.S3.Bucket,
		)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	failed := false
	select {
	case <-ctx.Done():
		logger.Info("shutting down")
	case err := <-errCh:
		logger.Error("http server failed", "error", err)
		failed = true
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil && !strings.Contains(err.Error(), "context deadline") {
		logger.Warn("graceful shutdown incomplete", "error", err)
	}
	if failed {
		_ = st.Close()
		os.Exit(1)
	}
}
