package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Geardung/seans-worker/internal/backend"
	"github.com/Geardung/seans-worker/internal/config"
	"github.com/Geardung/seans-worker/internal/engine"
	"github.com/Geardung/seans-worker/internal/qbit"
	"github.com/Geardung/seans-worker/internal/s3uploader"
	"github.com/Geardung/seans-worker/internal/sysutil"
)

var version = "dev"

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug}))
	slog.SetDefault(logger)

	logger.Info("seans worker starting", "version", version)

	cfg, err := config.Load()
	if err != nil {
		logger.Error("config error", "error", err)
		os.Exit(1)
	}

	// Init qBittorrent client
	qc, err := qbit.New(cfg.QBitURL, cfg.QBitUser, cfg.QBitPass, logger)
	if err != nil {
		logger.Error("qbit client error", "error", err)
		os.Exit(1)
	}

	// Login to qBittorrent (retry until ready)
	ctx := context.Background()
	qbitReady := false
	for i := 0; i < 30; i++ {
		if err := qc.Login(ctx); err != nil {
			logger.Warn("qbit login failed, retrying in 2s", "error", err, "attempt", i+1)
			time.Sleep(2 * time.Second)
			continue
		}
		qbitReady = true
		break
	}
	if !qbitReady {
		logger.Error("qbit login failed after 30 attempts")
		os.Exit(1)
	}

	// Init rclone uploader
	uploader, err := s3uploader.New(cfg.StateDir, cfg.S3AccessKey, cfg.S3SecretKey, cfg.MockS3, logger)
	if err != nil {
		logger.Error("uploader init error", "error", err)
		os.Exit(1)
	}

	// Init backend client
	bc := backend.New(cfg.BackendURL, cfg.WorkerRegisterToken, logger)

	// Register with backend (retry loop)
	diskFree, _ := sysutil.DiskFreeGB(cfg.StagingDir)
	heartbeatInterval := cfg.HeartbeatInterval

	for {
		interval, err := bc.Register(ctx, cfg.WorkerName, version, cfg.MaxConcurrentTasks, diskFree)
		if err != nil {
			logger.Error("registration failed, retrying in 10s", "error", err)
			time.Sleep(10 * time.Second)
			continue
		}
		heartbeatInterval = interval
		break
	}

	// Create and run engine
	eng := engine.New(cfg, bc, qc, uploader, logger)

	// Graceful shutdown on SIGTERM/SIGINT
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	runCtx, runCancel := context.WithCancel(context.Background())
	go func() {
		<-sigCh
		logger.Info("received shutdown signal")
		eng.Shutdown(runCtx)
		runCancel()
	}()

	logger.Info("worker ready",
		"worker_id", bc.WorkerID(),
		"heartbeat_sec", int(heartbeatInterval.Seconds()),
		"staging_dir", cfg.StagingDir,
		"mock_s3", cfg.MockS3,
	)

	eng.Run(runCtx)
	fmt.Println("worker exited")
}