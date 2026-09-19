package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

type Config struct {
	BackendURL          string
	WorkerRegisterToken string
	WorkerName          string
	QBitURL             string
	QBitUser            string
	QBitPass            string
	QBitDownloads       string
	StagingDir          string
	S3AccessKey         string
	S3SecretKey         string
	MockS3              bool
	ClaimInterval       time.Duration
	MaxConcurrentTasks  int
	HeartbeatInterval   time.Duration
	DownloadStallTimeout time.Duration
	TorrentMetadataTimeout time.Duration
	TaskMaxDuration     time.Duration
	DrainOnShutdown     bool
	DrainTimeout        time.Duration
	StateDir            string
}

func Load() (*Config, error) {
	cfg := &Config{
		BackendURL:          os.Getenv("BACKEND_URL"),
		WorkerRegisterToken: os.Getenv("WORKER_REGISTER_TOKEN"),
		WorkerName:          envOr("WORKER_NAME", hostname()),
		QBitURL:             envOr("QBIT_URL", "http://qbittorrent:8080"),
		QBitUser:            envOr("QBIT_USER", "admin"),
		QBitPass:            os.Getenv("QBIT_PASS"),
		QBitDownloads:       envOr("QBIT_DOWNLOADS", "/downloads"),
		StagingDir:          envOr("STAGING_DIR", "/staging"),
		S3AccessKey:         os.Getenv("S3_ACCESS_KEY"),
		S3SecretKey:         os.Getenv("S3_SECRET_KEY"),
		MockS3:              envBool("MOCK_S3", false),
		ClaimInterval:       envDuration("CLAIM_INTERVAL_SEC", 10*time.Second),
		MaxConcurrentTasks:  envInt("MAX_CONCURRENT_TASKS", 1),
		HeartbeatInterval:   envDuration("HEARTBEAT_INTERVAL_SEC", 60*time.Second),
		DownloadStallTimeout: envDuration("DOWNLOAD_STALL_TIMEOUT_MIN", 30*time.Minute),
		TorrentMetadataTimeout: envDuration("TORRENT_METADATA_TIMEOUT", 5*time.Minute),
		TaskMaxDuration:     envDurationHours("TASK_MAX_HOURS", 12),
		DrainOnShutdown:     envBool("DRAIN_ON_SHUTDOWN", false),
		DrainTimeout:        envDuration("DRAIN_TIMEOUT_SEC", 3600*time.Second),
		StateDir:            envOr("STATE_DIR", "/state"),
	}

	if cfg.BackendURL == "" {
		return nil, fmt.Errorf("BACKEND_URL is required")
	}
	if cfg.WorkerRegisterToken == "" {
		return nil, fmt.Errorf("WORKER_REGISTER_TOKEN is required")
	}
	if cfg.QBitPass == "" {
		return nil, fmt.Errorf("QBIT_PASS is required")
	}
	if !cfg.MockS3 {
		if cfg.S3AccessKey == "" {
			return nil, fmt.Errorf("S3_ACCESS_KEY is required (or set MOCK_S3=true)")
		}
		if cfg.S3SecretKey == "" {
			return nil, fmt.Errorf("S3_SECRET_KEY is required (or set MOCK_S3=true)")
		}
	}

	return cfg, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envBool(key string, fallback bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return fallback
	}
	return b
}

func envInt(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return n
}

func envDuration(key string, fallback time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return time.Duration(n) * time.Second
}

func envDurationHours(key string, fallback time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return time.Duration(n) * time.Hour
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return "worker-unknown"
	}
	return h
}