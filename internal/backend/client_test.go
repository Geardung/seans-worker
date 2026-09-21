package backend

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func newTestClient(t *testing.T, handler http.Handler) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	c := New(srv.URL, "reg-token", testLogger())
	t.Cleanup(srv.Close)
	return c, srv
}

func assertWorkerToken(t *testing.T, r *http.Request, expected string) {
	t.Helper()
	got := r.Header.Get("X-Worker-Token")
	if got != expected {
		t.Errorf("expected X-Worker-Token %q, got %q", expected, got)
	}
}

func TestRegister_HappyPath(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/worker/register", func(w http.ResponseWriter, r *http.Request) {
		assertWorkerToken(t, r, "reg-token")
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if body["hostname"] != "test-host" {
			t.Errorf("expected hostname test-host, got %v", body["hostname"])
		}
		respondJSON(w, map[string]any{
			"worker_id":              "w_abc",
			"worker_token":           "jwt-token-123",
			"heartbeat_interval_sec": 30,
		})
	})

	c, _ := newTestClient(t, mux)
	interval, err := c.Register(context.Background(), "test-host", "0.1.0", 1, 100.0)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if c.WorkerID() != "w_abc" {
		t.Errorf("expected worker_id w_abc, got %s", c.WorkerID())
	}
	if interval != 30*time.Second {
		t.Errorf("expected 30s interval, got %v", interval)
	}
	if !c.IsRegistered() {
		t.Error("expected client to be registered")
	}
}

func TestRegister_EmptyResponse_UsesRegToken(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/worker/register", func(w http.ResponseWriter, r *http.Request) {
		respondJSON(w, map[string]any{})
	})

	c, _ := newTestClient(t, mux)
	interval, err := c.Register(context.Background(), "h", "v", 1, 100)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if interval != 60*time.Second {
		t.Errorf("expected default 60s interval, got %v", interval)
	}
	if !c.IsRegistered() {
		t.Error("expected client to be registered (using regToken fallback)")
	}
}

func TestClaim_HappyPath(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/worker/register", func(w http.ResponseWriter, r *http.Request) {
		respondJSON(w, map[string]any{
			"worker_token": "jwt-token",
		})
	})
	mux.HandleFunc("POST /api/worker/claim", func(w http.ResponseWriter, r *http.Request) {
		assertWorkerToken(t, r, "jwt-token")
		respondJSON(w, map[string]any{
			"task": map[string]any{
				"task_id":       "tsk_001",
				"torrent_kind":  "magnet",
				"torrent_data":  "magnet:?xt=urn:btih:abc",
				"select_files":  nil,
				"dest_prefix":   "media/users/u1/md1/",
				"max_bytes":     1024,
				"lease_minutes": 10,
			},
		})
	})

	c, _ := newTestClient(t, mux)
	c.Register(context.Background(), "h", "v", 1, 100)

	task, err := c.Claim(context.Background())
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if task == nil {
		t.Fatal("expected task, got nil")
	}
	if task.TaskID != "tsk_001" {
		t.Errorf("expected tsk_001, got %s", task.TaskID)
	}
	if task.TorrentKind != "magnet" {
		t.Errorf("expected magnet, got %s", task.TorrentKind)
	}
}

func TestClaim_NoTask(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/worker/register", func(w http.ResponseWriter, r *http.Request) {
		respondJSON(w, map[string]any{
			"worker_token": "jwt-token",
		})
	})
	mux.HandleFunc("POST /api/worker/claim", func(w http.ResponseWriter, r *http.Request) {
		respondJSON(w, map[string]any{"task": nil})
	})

	c, _ := newTestClient(t, mux)
	c.Register(context.Background(), "h", "v", 1, 100)

	task, err := c.Claim(context.Background())
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if task != nil {
		t.Errorf("expected nil task, got %+v", task)
	}
}

func TestComplete_HappyPath(t *testing.T) {
	var receivedBody map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/worker/register", func(w http.ResponseWriter, r *http.Request) {
		respondJSON(w, map[string]any{
			"worker_token": "jwt-token",
		})
	})
	mux.HandleFunc("POST /api/worker/complete", func(w http.ResponseWriter, r *http.Request) {
		assertWorkerToken(t, r, "jwt-token")
		data, _ := io.ReadAll(r.Body)
		json.Unmarshal(data, &receivedBody)
		respondJSON(w, map[string]any{})
	})

	c, _ := newTestClient(t, mux)
	c.Register(context.Background(), "h", "v", 1, 100)

	files := []FileInfo{{S3Key: "media/users/u1/md1/movie.mkv", Size: 1024}}
	stats := TaskStats{DownloadSeconds: 60, UploadSeconds: 30}
	err := c.Complete(context.Background(), "tsk_001", files, stats)
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if receivedBody == nil {
		t.Fatal("expected body to be received")
	}
	if receivedBody["task_id"] != "tsk_001" {
		t.Errorf("expected task_id=tsk_001, got %v", receivedBody["task_id"])
	}
}

func TestFail_HappyPath(t *testing.T) {
	var receivedBody map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/worker/register", func(w http.ResponseWriter, r *http.Request) {
		respondJSON(w, map[string]any{
			"worker_token": "jwt-token",
		})
	})
	mux.HandleFunc("POST /api/worker/fail", func(w http.ResponseWriter, r *http.Request) {
		assertWorkerToken(t, r, "jwt-token")
		data, _ := io.ReadAll(r.Body)
		json.Unmarshal(data, &receivedBody)
		respondJSON(w, map[string]any{})
	})

	c, _ := newTestClient(t, mux)
	c.Register(context.Background(), "h", "v", 1, 100)

	err := c.Fail(context.Background(), "tsk_001", "stalled", true)
	if err != nil {
		t.Fatalf("fail: %v", err)
	}
	if receivedBody["task_id"] != "tsk_001" {
		t.Errorf("expected task_id=tsk_001, got %v", receivedBody["task_id"])
	}
	if receivedBody["permanent"] != true {
		t.Errorf("expected permanent=true, got %v", receivedBody["permanent"])
	}
	if receivedBody["reason"] != "stalled" {
		t.Errorf("expected reason=stalled, got %v", receivedBody["reason"])
	}
}

func TestHeartbeat_HappyPath(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/worker/register", func(w http.ResponseWriter, r *http.Request) {
		respondJSON(w, map[string]any{
			"worker_token": "jwt-token",
		})
	})
	mux.HandleFunc("POST /api/worker/heartbeat", func(w http.ResponseWriter, r *http.Request) {
		assertWorkerToken(t, r, "jwt-token")
		var body HeartbeatRequest
		json.NewDecoder(r.Body).Decode(&body)
		if len(body.Tasks) != 1 {
			t.Errorf("expected 1 task, got %d", len(body.Tasks))
		}
		if body.Tasks[0].TaskID != "tsk_001" {
			t.Errorf("expected tsk_001, got %s", body.Tasks[0].TaskID)
		}
		respondJSON(w, map[string]any{})
	})

	c, _ := newTestClient(t, mux)
	c.Register(context.Background(), "h", "v", 1, 100)

	tasks := []HeartbeatTask{
		{TaskID: "tsk_001", Stage: "downloading", ProgressPct: 50.0, SpeedMbps: 1.5},
	}
	err := c.Heartbeat(context.Background(), tasks, 100.0)
	if err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
}

func Test401_TriggersReRegistrationFlag(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/worker/register", func(w http.ResponseWriter, r *http.Request) {
		respondJSON(w, map[string]any{
			"worker_token": "jwt-token",
		})
	})
	mux.HandleFunc("POST /api/worker/claim", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		w.Write([]byte(`{"detail":"unauthorized"}`))
	})

	c, _ := newTestClient(t, mux)
	c.Register(context.Background(), "h", "v", 1, 100)

	_, err := c.Claim(context.Background())
	if err == nil {
		t.Fatal("expected error for 401")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("expected 401 in error, got: %v", err)
	}
}

func respondJSON(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(data)
}