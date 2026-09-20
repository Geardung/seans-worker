package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sync"
)

type mockState struct {
	mu       sync.Mutex
	taskIssued bool
}

var state = &mockState{}

func main() {
	port := envOr("MOCK_PORT", "9999")

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/workers/register", handleRegister)
	mux.HandleFunc("POST /v1/workers/{worker_id}/heartbeat", handleHeartbeat)
	mux.HandleFunc("POST /v1/workers/{worker_id}/claim", handleClaim)
	mux.HandleFunc("POST /v1/tasks/{task_id}/complete", handleComplete)
	mux.HandleFunc("POST /v1/tasks/{task_id}/fail", handleFail)

	fmt.Printf("Mock backend listening on :%s\n", port)
	log.Fatal(http.ListenAndServe(":"+port, mux))
}

func handleRegister(w http.ResponseWriter, r *http.Request) {
	body := readBody(r)
	fmt.Printf("[REGISTER] %s\n", string(body))

	respond(w, map[string]any{
		"worker_id":               "w_mock_001",
		"worker_token":            "mock-token-abc123",
		"heartbeat_interval_sec":  60,
	})
}

func handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	workerID := r.PathValue("worker_id")
	body := readBody(r)
	fmt.Printf("[HEARTBEAT] worker=%s %s\n", workerID, string(body))
	respond(w, map[string]any{})
}

func handleClaim(w http.ResponseWriter, r *http.Request) {
	workerID := r.PathValue("worker_id")
	body := readBody(r)

	state.mu.Lock()
	issued := state.taskIssued
	state.taskIssued = true
	state.mu.Unlock()

	if issued {
		fmt.Printf("[CLAIM] worker=%s body=%s -> no task\n", workerID, string(body))
		respond(w, map[string]any{"task": nil})
		return
	}

	torrentKind := envOr("MOCK_TORRENT_KIND", "magnet")
	torrentData := envOr("MOCK_TORRENT_DATA", "")
	destPrefix := envOr("MOCK_DEST_PREFIX", "media/users/u_mock/md_test/s1/")
	maxBytes := envInt64Or("MOCK_MAX_BYTES", 2147483648)

	var selectFiles any = nil
	if sf := os.Getenv("MOCK_SELECT_FILES"); sf != "" {
		var parsed []string
		json.Unmarshal([]byte(sf), &parsed)
		selectFiles = parsed
	}

	task := map[string]any{
		"task_id":       "tsk_mock_001",
		"torrent_kind":  torrentKind,
		"torrent_data":  torrentData,
		"select_files":  selectFiles,
		"dest_prefix":   destPrefix,
		"max_bytes":     maxBytes,
		"lease_minutes": 10,
	}

	fmt.Printf("[CLAIM] worker=%s -> task tsk_mock_001 (kind=%s)\n", workerID, torrentKind)
	respond(w, map[string]any{"task": task})
}

func handleComplete(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("task_id")
	body := readBody(r)
	fmt.Printf("[COMPLETE] task=%s %s\n", taskID, string(body))
	respond(w, map[string]any{})
}

func handleFail(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("task_id")
	body := readBody(r)
	fmt.Printf("[FAIL] task=%s %s\n", taskID, string(body))
	respond(w, map[string]any{})
}

func readBody(r *http.Request) []byte {
	data, _ := io.ReadAll(r.Body)
	return data
}

func respond(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(data)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt64Or(key string, fallback int64) int64 {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	var n int64
	fmt.Sscanf(v, "%d", &n)
	return n
}