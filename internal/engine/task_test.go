package engine

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/Geardung/seans-worker/internal/backend"
	"github.com/Geardung/seans-worker/internal/config"
	"github.com/Geardung/seans-worker/internal/qbit"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// mockQBit implements a minimal qBittorrent client for testing.
type mockQBit struct {
	torrents []qbit.TorrentInfo
	files    []qbit.TorrentFile
	addErr   error
}

func (m *mockQBit) GetTorrentBySavePath(_ context.Context, _, _ string) (*qbit.TorrentInfo, error) {
	if len(m.torrents) > 0 {
		return &m.torrents[0], nil
	}
	return nil, nil
}

func (m *mockQBit) GetTorrents(_ context.Context, _ string) ([]qbit.TorrentInfo, error) {
	return m.torrents, nil
}

func (m *mockQBit) GetFiles(_ context.Context, _ string) ([]qbit.TorrentFile, error) {
	return m.files, nil
}

func (m *mockQBit) AddTorrentMagnet(_ context.Context, _, _, _ string) error {
	return m.addErr
}

func (m *mockQBit) AddTorrentFile(_ context.Context, _, _, _ string) error {
	return m.addErr
}

func (m *mockQBit) SetFilePriority(_ context.Context, _ string, _ []int, _ int) error {
	return nil
}

func (m *mockQBit) DeleteTorrent(_ context.Context, _ string, _ bool) error {
	return nil
}

func (m *mockQBit) PauseTorrents(_ context.Context, _ string) error {
	return nil
}

// mockBackend implements a minimal backend client for testing.
type mockBackend struct {
	completed []string
	failed    []struct {
		TaskID    string
		Reason    string
		Permanent bool
	}
}

func (m *mockBackend) Complete(_ context.Context, taskID string, _ []backend.FileInfo, _ backend.TaskStats) error {
	m.completed = append(m.completed, taskID)
	return nil
}

func (m *mockBackend) Fail(_ context.Context, taskID, reason string, permanent bool) error {
	m.failed = append(m.failed, struct {
		TaskID    string
		Reason    string
		Permanent bool
	}{taskID, reason, permanent})
	return nil
}

func (m *mockBackend) Register(_ context.Context, _, _ string, _ int, _ float64) (time.Duration, error) {
	return 60 * time.Second, nil
}

func (m *mockBackend) Claim(_ context.Context) (*backend.Task, error) {
	return nil, nil
}

func (m *mockBackend) Heartbeat(_ context.Context, _ []backend.HeartbeatTask, _ float64) error {
	return nil
}

func (m *mockBackend) WorkerID() string { return "w_mock" }

func (m *mockBackend) IsRegistered() bool { return true }

func TestMatchFiles_ExactMatch(t *testing.T) {
	tr := &taskRunner{
		task: &backend.Task{
			SelectFiles: []string{"Show/S01/E01.mkv", "Show/S01/E02.mkv"},
		},
		logger: testLogger(),
	}

	files := []qbit.TorrentFile{
		{Index: 0, Name: "Show/S01/E01.mkv", Size: 100},
		{Index: 1, Name: "Show/S01/E02.mkv", Size: 200},
		{Index: 2, Name: "Show/S01/E03.mkv", Size: 300},
	}

	matched := tr.matchFiles(files, tr.task.SelectFiles)
	if len(matched) != 2 {
		t.Fatalf("expected 2 matches, got %d", len(matched))
	}
	if matched[0] != 0 || matched[1] != 1 {
		t.Errorf("expected indices [0,1], got %v", matched)
	}
}

func TestMatchFiles_SuffixFallback(t *testing.T) {
	tr := &taskRunner{
		task: &backend.Task{
			SelectFiles: []string{"E01.mkv"},
		},
		logger: testLogger(),
	}

	files := []qbit.TorrentFile{
		{Index: 0, Name: "Show/Season 1/E01.mkv", Size: 100},
		{Index: 1, Name: "Show/Season 1/E02.mkv", Size: 200},
	}

	matched := tr.matchFiles(files, tr.task.SelectFiles)
	if len(matched) != 1 {
		t.Fatalf("expected 1 match via suffix, got %d", len(matched))
	}
	if matched[0] != 0 {
		t.Errorf("expected index 0, got %d", matched[0])
	}
}

func TestMatchFiles_NoMatch(t *testing.T) {
	tr := &taskRunner{
		task: &backend.Task{
			SelectFiles: []string{"nonexistent.mkv"},
		},
		logger: testLogger(),
	}

	files := []qbit.TorrentFile{
		{Index: 0, Name: "movie.mkv", Size: 100},
	}

	matched := tr.matchFiles(files, tr.task.SelectFiles)
	if len(matched) != 0 {
		t.Errorf("expected 0 matches, got %d", len(matched))
	}
}

func TestIsActiveState(t *testing.T) {
	tests := []struct {
		state string
		want  bool
	}{
		{"downloading", true},
		{"forcedDL", true},
		{"stalledDL", true},
		{"checkingDL", true},
		{"queuedDL", true},
		{"metaDL", true},
		{"stoppedUP", false},
		{"pausedUP", false},
		{"uploading", false},
		{"stalledUP", false},
		{"unknown", false},
	}

	for _, tt := range tests {
		if got := isActiveState(tt.state); got != tt.want {
			t.Errorf("isActiveState(%q) = %v, want %v", tt.state, got, tt.want)
		}
	}
}

func TestRelativePath(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"/Show/S01/E01.mkv", "Show/S01/E01.mkv"},
		{"Show/S01/E01.mkv", "Show/S01/E01.mkv"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := relativePath(tt.input); got != tt.want {
			t.Errorf("relativePath(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestTaskError(t *testing.T) {
	err := &TaskError{Reason: "stalled", Permanent: true}
	if err.Error() != "stalled" {
		t.Errorf("expected stalled, got %s", err.Error())
	}
}

func TestConfigLoad_Defaults(t *testing.T) {
	t.Setenv("BACKEND_URL", "http://localhost:9999")
	t.Setenv("WORKER_REGISTER_TOKEN", "token")
	t.Setenv("QBIT_API_KEY", "qbt_testkey123")
	t.Setenv("MOCK_S3", "true")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if cfg.BackendURL != "http://localhost:9999" {
		t.Errorf("expected backend URL, got %s", cfg.BackendURL)
	}
	if cfg.ClaimInterval != 10*time.Second {
		t.Errorf("expected 10s claim interval, got %v", cfg.ClaimInterval)
	}
	if cfg.HeartbeatInterval != 60*time.Second {
		t.Errorf("expected 60s heartbeat, got %v", cfg.HeartbeatInterval)
	}
	if cfg.MaxConcurrentTasks != 1 {
		t.Errorf("expected 1 max tasks, got %d", cfg.MaxConcurrentTasks)
	}
}

func TestConfigLoad_MissingRequired(t *testing.T) {
	// Clear all env
	t.Setenv("BACKEND_URL", "")
	t.Setenv("WORKER_REGISTER_TOKEN", "")
	t.Setenv("QBIT_API_KEY", "")

	_, err := config.Load()
	if err == nil {
		t.Fatal("expected error for missing required config")
	}
}