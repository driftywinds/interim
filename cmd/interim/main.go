package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/netip"
	"net/url"
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

func isLocalOrigin(raw string) bool {
	parsed, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := parsed.Hostname()
	if host == "localhost" {
		return true
	}
	addr, err := netip.ParseAddr(host)
	return err == nil && (addr.IsLoopback() || addr.IsPrivate())
}

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

	if !cfg.TrustProxy {
		logger.Warn("TRUST_PROXY=false: per-IP limits use the direct peer address; behind Caddy/cloudflared set TRUST_PROXY=true and TRUSTED_PROXY_CIDRS")
	}
	if !isLocalOrigin(cfg.PublicBaseURL) && strings.HasPrefix(cfg.PublicBaseURL, "http://") {
		logger.Warn("PUBLIC_BASE_URL is plain HTTP; TLS is required for secure cookies and to protect presigned URLs", "url", cfg.PublicBaseURL)
	}
	if cfg.S3.PublicEndpoint != "" && !isLocalOrigin(cfg.S3.PublicEndpoint) && strings.HasPrefix(cfg.S3.PublicEndpoint, "http://") {
		logger.Warn("S3_PUBLIC_ENDPOINT is plain HTTP; presigned upload signatures can be observed in transit", "endpoint", cfg.S3.PublicEndpoint)
	}

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

	hasAdmin, err := st.HasAdmin(ctx)
	if err != nil {
		logger.Error("check admin account", "error", err)
		os.Exit(1)
	}
	if hasAdmin {
		if cfg.SetupTokenPath != "" {
			if err := os.Remove(cfg.SetupTokenPath); err != nil && !errors.Is(err, os.ErrNotExist) {
				logger.Warn("remove unused setup token", "error", err)
			}
		}
	} else {
		logger.Info("admin setup required",
			"url", cfg.PublicBaseURL+"/setup",
			"setup_token_file", cfg.SetupTokenPath,
			"setup_token_from_env", cfg.SetupTokenPath == "",
		)
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

	app.StartupMaintenance(ctx)
	go app.RunMaintenance(ctx)

	httpServer := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           app.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("interim starting",
			"url", cfg.PublicBaseURL,
			"listen", cfg.ListenAddr,
			"storage_limit", cfg.MaxStorage,
			"storage_unlimited", cfg.MaxStorage == 0,
			"max_file_size", cfg.MaxFileSize,
			"max_files", cfg.MaxFiles,
			"max_pending_uploads_per_client", cfg.MaxPendingUploadsPerClient,
			"max_reserved_bytes_per_client", cfg.MaxReservedBytesPerClient,
			"multipart_enabled", cfg.MultipartEnabled,
			"multipart_threshold", cfg.MultipartThreshold,
			"multipart_part_size", cfg.MultipartPartSize,
			"multipart_sign_part_length", cfg.MultipartSignPartLength,
			"upload_url_ttl", cfg.UploadURLTTL.String(),
			"upload_intent_ttl", cfg.UploadIntentTTL.String(),
			"retention", cfg.Retention.String(),
			"public_uploads", cfg.PublicUploadsEnabled,
			"bucket", cfg.S3.Bucket,
			"download_worker", cfg.DownloadWorkerEnabled,
			"download_workers", len(cfg.DownloadWorkerURLs),
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

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil && !strings.Contains(err.Error(), "context deadline") {
		logger.Warn("graceful shutdown incomplete", "error", err)
	}
	if failed {
		_ = st.Close()
		os.Exit(1)
	}
}
