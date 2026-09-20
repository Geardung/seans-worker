package qbit

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type TorrentFile struct {
	Index    int     `json:"index"`
	Name     string  `json:"name"`
	Size     int64   `json:"size"`
	Progress float64 `json:"progress"`
	Priority int     `json:"priority"`
}

type TorrentInfo struct {
	Hash       string  `json:"hash"`
	Name       string  `json:"name"`
	SavePath   string  `json:"save_path"`
	Progress   float64 `json:"progress"`
	DlSpeed    int64   `json:"dlspeed"`
	Downloaded int64   `json:"downloaded"`
	State      string  `json:"state"`
	Size       int64   `json:"size"`
}

// Client wraps the qBittorrent Web API v2.
type Client struct {
	baseURL    string
	user       string
	pass       string
	httpClient *http.Client
	logger     *slog.Logger
	loggedIn   bool
}

func New(baseURL, user, pass string, logger *slog.Logger) (*Client, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, fmt.Errorf("cookie jar: %w", err)
	}
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		user:    user,
		pass:    pass,
		httpClient: &http.Client{
			Jar:     jar,
			Timeout: 60 * time.Second,
		},
		logger: logger,
	}, nil
}

// Login authenticates with qBittorrent.
func (c *Client) Login(ctx context.Context) error {
	form := url.Values{
		"username": {c.user},
		"password": {c.pass},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/v2/auth/login", strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("login request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("login: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	trimmed := strings.TrimSpace(string(body))
	if resp.StatusCode == 204 || (resp.StatusCode == 200 && trimmed == "Ok.") {
		// success
	} else {
		return fmt.Errorf("login failed: status=%d body=%s", resp.StatusCode, string(body))
	}

	c.loggedIn = true
	c.logger.Info("logged in to qBittorrent")
	return nil
}

// AddTorrentMagnet adds a magnet link.
func (c *Client) AddTorrentMagnet(ctx context.Context, magnet, savePath, category string) error {
	form := url.Values{
		"urls":     {magnet},
		"savepath": {savePath},
		"category": {category},
		"paused":   {"false"},
	}
	return c.postForm(ctx, "/api/v2/torrents/add", form)
}

// AddTorrentFile adds a base64-encoded .torrent file.
func (c *Client) AddTorrentFile(ctx context.Context, torrentB64, savePath, category string) error {
	torrentData, err := base64.StdEncoding.DecodeString(torrentB64)
	if err != nil {
		return fmt.Errorf("decode torrent b64: %w", err)
	}

	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)

	part, err := writer.CreateFormFile("torrents", "upload.torrent")
	if err != nil {
		return fmt.Errorf("create form file: %w", err)
	}
	if _, err := part.Write(torrentData); err != nil {
		return fmt.Errorf("write form file: %w", err)
	}

	writer.WriteField("savepath", savePath)
	writer.WriteField("category", category)
	writer.WriteField("paused", "false")
	writer.Close()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/v2/torrents/add", &buf)
	if err != nil {
		return fmt.Errorf("add torrent request: %w", err)
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("add torrent: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if strings.TrimSpace(string(respBody)) != "Ok." {
		return fmt.Errorf("add torrent failed: %s", string(respBody))
	}
	return nil
}

// GetTorrents returns torrents matching the given category.
func (c *Client) GetTorrents(ctx context.Context, category string) ([]TorrentInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/api/v2/torrents/info?category="+url.QueryEscape(category), nil)
	if err != nil {
		return nil, fmt.Errorf("get torrents request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("get torrents: %w", err)
	}
	defer resp.Body.Close()

	var torrents []TorrentInfo
	if err := json.NewDecoder(resp.Body).Decode(&torrents); err != nil {
		return nil, fmt.Errorf("decode torrents: %w", err)
	}
	return torrents, nil
}

// GetTorrentBySavePath finds a torrent with matching save path in the given category.
func (c *Client) GetTorrentBySavePath(ctx context.Context, category, savePath string) (*TorrentInfo, error) {
	torrents, err := c.GetTorrents(ctx, category)
	if err != nil {
		return nil, err
	}
	for _, t := range torrents {
		if t.SavePath == savePath {
			return &t, nil
		}
	}
	return nil, nil
}

// GetFiles returns the file list for a torrent hash.
func (c *Client) GetFiles(ctx context.Context, hash string) ([]TorrentFile, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/api/v2/torrents/files?hash="+url.QueryEscape(hash), nil)
	if err != nil {
		return nil, fmt.Errorf("get files request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("get files: %w", err)
	}
	defer resp.Body.Close()

	var files []TorrentFile
	if err := json.NewDecoder(resp.Body).Decode(&files); err != nil {
		return nil, fmt.Errorf("decode files: %w", err)
	}
	return files, nil
}

// SetFilePriority sets download priority for specific file indices.
// priority: 0=skip, 1=normal, 6=high, 7=maximal
func (c *Client) SetFilePriority(ctx context.Context, hash string, indices []int, priority int) error {
	indexStrs := make([]string, len(indices))
	for i, idx := range indices {
		indexStrs[i] = strconv.Itoa(idx)
	}

	form := url.Values{
		"hash":     {hash},
		"id":       {strings.Join(indexStrs, "|")},
		"priority": {strconv.Itoa(priority)},
	}
	return c.postForm(ctx, "/api/v2/torrents/filePrio", form)
}

// DeleteTorrent removes a torrent, optionally deleting files.
func (c *Client) DeleteTorrent(ctx context.Context, hash string, deleteFiles bool) error {
	form := url.Values{
		"hashes":      {hash},
		"deleteFiles": {strconv.FormatBool(deleteFiles)},
	}
	return c.postForm(ctx, "/api/v2/torrents/delete", form)
}

// PauseTorrents pauses torrents by hash.
func (c *Client) PauseTorrents(ctx context.Context, hashes string) error {
	form := url.Values{
		"hashes": {hashes},
	}
	return c.postForm(ctx, "/api/v2/torrents/pause", form)
}

func (c *Client) postForm(ctx context.Context, path string, form url.Values) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("post form %s: %w", path, err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("post %s: %w", path, err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	trimmed := strings.TrimSpace(string(body))
	if trimmed != "Ok." && trimmed != "" {
		return fmt.Errorf("post %s failed: %s", path, trimmed)
	}
	return nil
}

// TempFile writes base64 data to a temp file and returns its path. Caller must remove.
func TempFile(data []byte, suffix string) (string, error) {
	f, err := os.CreateTemp("", "seans-*"+suffix)
	if err != nil {
		return "", fmt.Errorf("create temp: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", fmt.Errorf("write temp: %w", err)
	}
	f.Close()
	return f.Name(), nil
}

// CategorySavePath returns the qBittorrent save path for a task.
func CategorySavePath(qbitDownloads, taskID string) string {
	return filepath.Join(qbitDownloads, taskID)
}