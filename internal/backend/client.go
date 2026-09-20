package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

type Task struct {
	TaskID      string   `json:"task_id"`
	TorrentKind string   `json:"torrent_kind"`
	TorrentData string   `json:"torrent_data"`
	SelectFiles []string `json:"select_files"`
	DestPrefix  string   `json:"dest_prefix"`
	MaxBytes    int64    `json:"max_bytes"`
	LeaseMinutes int     `json:"lease_minutes"`
}

type ClaimResponse struct {
	Task *Task `json:"task"`
}

type RegisterResponse struct {
	WorkerID             string `json:"worker_id"`
	WorkerToken          string `json:"worker_token"`
	HeartbeatIntervalSec int    `json:"heartbeat_interval_sec"`
}

type FileInfo struct {
	S3Key  string `json:"s3_key"`
	Size   int64  `json:"size"`
}

type TaskStats struct {
	DownloadSeconds int `json:"download_seconds"`
	UploadSeconds   int `json:"upload_seconds"`
}

type HeartbeatTask struct {
	TaskID      string  `json:"task_id"`
	Stage       string  `json:"stage"`
	ProgressPct float64 `json:"progress_pct"`
	SpeedMbps   float64 `json:"speed_mbps"`
	ETAMin      int     `json:"eta_min"`
	Message     string  `json:"message"`
}

type HeartbeatRequest struct {
	Tasks      []HeartbeatTask `json:"tasks"`
	DiskFreeGB float64         `json:"disk_free_gb"`
}

// Client talks to the Seans backend API v1.
type Client struct {
	baseURL    string
	regToken   string
	httpClient *http.Client
	logger     *slog.Logger

	mu           sync.Mutex
	workerID     string
	workerToken  string
	reRegistered bool
}

func New(baseURL, regToken string, logger *slog.Logger) *Client {
	return &Client{
		baseURL:  baseURL,
		regToken: regToken,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
		logger: logger,
	}
}

func (c *Client) WorkerID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.workerID
}

func (c *Client) HeartbeatInterval() time.Duration {
	// Default; overridden by register response.
	return 60 * time.Second
}

// Register registers the worker with the backend. Idempotent — called at startup.
func (c *Client) Register(ctx context.Context, hostname, version string, maxConcurrent int, diskFreeGB float64) (time.Duration, error) {
	body := map[string]any{
		"hostname":               hostname,
		"version":                version,
		"max_concurrent_tasks":   maxConcurrent,
		"disk_free_gb":           diskFreeGB,
	}

	var resp RegisterResponse
	if err := c.doJSON(ctx, http.MethodPost, "/v1/workers/register", c.regToken, body, &resp); err != nil {
		return 0, fmt.Errorf("register: %w", err)
	}

	c.mu.Lock()
	c.workerID = resp.WorkerID
	c.workerToken = resp.WorkerToken
	c.reRegistered = false
	c.mu.Unlock()

	interval := time.Duration(resp.HeartbeatIntervalSec) * time.Second
	if interval == 0 {
		interval = 60 * time.Second
	}
	c.logger.Info("registered with backend", "worker_id", resp.WorkerID, "heartbeat_sec", int(interval.Seconds()))
	return interval, nil
}

// Claim requests the next task from the backend.
func (c *Client) Claim(ctx context.Context) (*Task, error) {
	body := map[string]any{"max_tasks": 1}

	var resp ClaimResponse
	if err := c.doAuthJSON(ctx, http.MethodPost, fmt.Sprintf("/v1/workers/%s/claim", c.WorkerID()), body, &resp); err != nil {
		return nil, fmt.Errorf("claim: %w", err)
	}
	return resp.Task, nil
}

// Complete reports task success.
func (c *Client) Complete(ctx context.Context, taskID string, files []FileInfo, stats TaskStats) error {
	body := map[string]any{
		"files": files,
		"stats": stats,
	}
	if err := c.doAuthJSON(ctx, http.MethodPost, fmt.Sprintf("/v1/tasks/%s/complete", taskID), body, nil); err != nil {
		return fmt.Errorf("complete %s: %w", taskID, err)
	}
	return nil
}

// Fail reports task failure.
func (c *Client) Fail(ctx context.Context, taskID, reason string, permanent bool) error {
	body := map[string]any{
		"reason":    reason,
		"permanent": permanent,
	}
	if err := c.doAuthJSON(ctx, http.MethodPost, fmt.Sprintf("/v1/tasks/%s/fail", taskID), body, nil); err != nil {
		return fmt.Errorf("fail %s: %w", taskID, err)
	}
	return nil
}

// Heartbeat sends progress for all active tasks.
func (c *Client) Heartbeat(ctx context.Context, tasks []HeartbeatTask, diskFreeGB float64) error {
	body := HeartbeatRequest{
		Tasks:      tasks,
		DiskFreeGB: diskFreeGB,
	}
	if err := c.doAuthJSON(ctx, http.MethodPost, fmt.Sprintf("/v1/workers/%s/heartbeat", c.WorkerID()), body, nil); err != nil {
		return fmt.Errorf("heartbeat: %w", err)
	}
	return nil
}

func (c *Client) doJSON(ctx context.Context, method, path, auth string, body any, result any) error {
	var reqBody io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal: %w", err)
		}
		reqBody = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reqBody)
	if err != nil {
		return fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if auth != "" {
		req.Header.Set("Authorization", "Bearer "+auth)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == 401 {
		return c.handle401(ctx, method, path, auth, body, result)
	}
	if resp.StatusCode >= 500 {
		return fmt.Errorf("server error: %d", resp.StatusCode)
	}
	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("client error %d: %s", resp.StatusCode, string(respBody))
	}

	if result != nil {
		if err := json.NewDecoder(resp.Body).Decode(result); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}

func (c *Client) doAuthJSON(ctx context.Context, method, path string, body any, result any) error {
	c.mu.Lock()
	token := c.workerToken
	c.mu.Unlock()
	if token == "" {
		return fmt.Errorf("not registered")
	}
	return c.doJSON(ctx, method, path, token, body, result)
}

func (c *Client) handle401(ctx context.Context, method, path, auth string, body any, result any) error {
	c.mu.Lock()
	alreadyReRegistered := c.reRegistered
	c.mu.Unlock()

	if alreadyReRegistered {
		return fmt.Errorf("401 after re-register, backing off")
	}

	c.logger.Warn("received 401, attempting re-registration")
	c.mu.Lock()
	c.workerToken = ""
	c.reRegistered = true
	c.mu.Unlock()

	return fmt.Errorf("401: need re-registration")
}

// ClearReRegistrationFlag resets the 401 re-registration flag.
func (c *Client) ClearReRegistrationFlag() {
	c.mu.Lock()
	c.reRegistered = false
	c.mu.Unlock()
}

// IsRegistered returns true if the client has a valid worker token.
func (c *Client) IsRegistered() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.workerToken != ""
}