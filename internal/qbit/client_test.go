package qbit

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newTestServer(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

func TestLogin(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v2/auth/login", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
		}
		if r.FormValue("username") != "admin" || r.FormValue("password") != "secret" {
			http.Error(w, "Unauthorized", http.StatusForbidden)
			return
		}
		w.Write([]byte("Ok."))
	})

	srv := newTestServer(t, mux)
	c, err := New(srv.URL, "admin", "secret", testLogger())
	if err != nil {
		t.Fatal(err)
	}

	if err := c.Login(context.Background()); err != nil {
		t.Fatalf("login failed: %v", err)
	}
}

func TestLogin_BadCredentials(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v2/auth/login", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte("Fails."))
	})

	srv := newTestServer(t, mux)
	c, err := New(srv.URL, "admin", "wrong", testLogger())
	if err != nil {
		t.Fatal(err)
	}

	if err := c.Login(context.Background()); err == nil {
		t.Fatal("expected error for bad credentials")
	}
}

func TestAddTorrentMagnet(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v2/torrents/add", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
		}
		if r.FormValue("urls") == "" {
			t.Error("expected urls field")
		}
		if r.FormValue("category") != "seans" {
			t.Errorf("expected category=seans, got %s", r.FormValue("category"))
		}
		w.Write([]byte("Ok."))
	})

	srv := newTestServer(t, mux)
	c, err := New(srv.URL, "admin", "pass", testLogger())
	if err != nil {
		t.Fatal(err)
	}

	err = c.AddTorrentMagnet(context.Background(), "magnet:?xt=urn:btih:abc123", "/downloads/tsk1", "seans")
	if err != nil {
		t.Fatalf("add magnet failed: %v", err)
	}
}

func TestAddTorrentFile(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v2/torrents/add", func(w http.ResponseWriter, r *http.Request) {
		ct := r.Header.Get("Content-Type")
		if !strings.Contains(ct, "multipart/form-data") {
			t.Errorf("expected multipart, got %s", ct)
		}
		if err := r.ParseMultipartForm(10 << 20); err != nil {
			t.Errorf("parse multipart: %v", err)
		}
		if r.FormValue("category") != "seans" {
			t.Errorf("expected category=seans")
		}
		_, _, err := r.FormFile("torrents")
		if err != nil {
			t.Errorf("expected torrents file: %v", err)
		}
		w.Write([]byte("Ok."))
	})

	srv := newTestServer(t, mux)
	c, err := New(srv.URL, "admin", "pass", testLogger())
	if err != nil {
		t.Fatal(err)
	}

	// base64 of a minimal torrent-like data
	torrentB64 := "ZDg6YW5ub3VuY2UxMzpodHRwOi8vZXhhbXBsZTFlNDpOYW1lMTA6dGVzdC50b3JyZW50ZQ=="
	err = c.AddTorrentFile(context.Background(), torrentB64, "/downloads/tsk1", "seans")
	if err != nil {
		t.Fatalf("add file failed: %v", err)
	}
}

func TestAddTorrent_FailsResponse(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v2/torrents/add", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("Fails."))
	})

	srv := newTestServer(t, mux)
	c, err := New(srv.URL, "admin", "pass", testLogger())
	if err != nil {
		t.Fatal(err)
	}

	err = c.AddTorrentMagnet(context.Background(), "magnet:?xt=urn:btih:abc", "/dl", "seans")
	if err == nil {
		t.Fatal("expected error for Fails. response")
	}
}

func TestGetTorrents(t *testing.T) {
	torrents := []TorrentInfo{
		{Hash: "abc123", Name: "Test", SavePath: "/downloads/tsk1", Progress: 0.5, DlSpeed: 1024},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v2/torrents/info", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("category") != "seans" {
			t.Errorf("expected category=seans")
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(torrents)
	})

	srv := newTestServer(t, mux)
	c, err := New(srv.URL, "admin", "pass", testLogger())
	if err != nil {
		t.Fatal(err)
	}

	result, err := c.GetTorrents(context.Background(), "seans")
	if err != nil {
		t.Fatalf("get torrents: %v", err)
	}
	if len(result) != 1 {
		t.Fatalf("expected 1 torrent, got %d", len(result))
	}
	if result[0].Hash != "abc123" {
		t.Errorf("expected hash abc123, got %s", result[0].Hash)
	}
}

func TestGetFiles(t *testing.T) {
	files := []TorrentFile{
		{Index: 0, Name: "movie.mkv", Size: 1024, Progress: 1.0, Priority: 1},
		{Index: 1, Name: "sub.srt", Size: 512, Progress: 0.5, Priority: 1},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v2/torrents/files", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("hash") != "abc123" {
			t.Errorf("expected hash=abc123")
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(files)
	})

	srv := newTestServer(t, mux)
	c, err := New(srv.URL, "admin", "pass", testLogger())
	if err != nil {
		t.Fatal(err)
	}

	result, err := c.GetFiles(context.Background(), "abc123")
	if err != nil {
		t.Fatalf("get files: %v", err)
	}
	if len(result) != 2 {
		t.Fatalf("expected 2 files, got %d", len(result))
	}
	if result[0].Name != "movie.mkv" {
		t.Errorf("expected movie.mkv, got %s", result[0].Name)
	}
}

func TestSetFilePriority(t *testing.T) {
	var receivedForm string
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v2/torrents/filePrio", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
		}
		receivedForm = r.Form.Encode()
		if r.FormValue("hash") != "abc123" {
			t.Errorf("expected hash=abc123, got %s", r.FormValue("hash"))
		}
		if r.FormValue("priority") != "0" {
			t.Errorf("expected priority=0, got %s", r.FormValue("priority"))
		}
		w.Write([]byte("Ok."))
	})

	srv := newTestServer(t, mux)
	c, err := New(srv.URL, "admin", "pass", testLogger())
	if err != nil {
		t.Fatal(err)
	}

	err = c.SetFilePriority(context.Background(), "abc123", []int{1, 2}, 0)
	if err != nil {
		t.Fatalf("set priority: %v", err)
	}
	if !strings.Contains(receivedForm, "id=") {
		t.Errorf("expected id in form, got %s", receivedForm)
	}
}

func TestDeleteTorrent(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v2/torrents/delete", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
		}
		if r.FormValue("hashes") != "abc123" {
			t.Errorf("expected hashes=abc123")
		}
		if r.FormValue("deleteFiles") != "true" {
			t.Errorf("expected deleteFiles=true")
		}
		w.Write([]byte("Ok."))
	})

	srv := newTestServer(t, mux)
	c, err := New(srv.URL, "admin", "pass", testLogger())
	if err != nil {
		t.Fatal(err)
	}

	err = c.DeleteTorrent(context.Background(), "abc123", true)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
}

func TestGetTorrentBySavePath(t *testing.T) {
	torrents := []TorrentInfo{
		{Hash: "aaa", Name: "A", SavePath: "/downloads/tsk1"},
		{Hash: "bbb", Name: "B", SavePath: "/downloads/tsk2"},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v2/torrents/info", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(torrents)
	})

	srv := newTestServer(t, mux)
	c, err := New(srv.URL, "admin", "pass", testLogger())
	if err != nil {
		t.Fatal(err)
	}

	found, err := c.GetTorrentBySavePath(context.Background(), "seans", "/downloads/tsk2")
	if err != nil {
		t.Fatalf("find by save path: %v", err)
	}
	if found == nil {
		t.Fatal("expected to find torrent")
	}
	if found.Hash != "bbb" {
		t.Errorf("expected hash bbb, got %s", found.Hash)
	}

	notFound, err := c.GetTorrentBySavePath(context.Background(), "seans", "/downloads/nonexistent")
	if err != nil {
		t.Fatalf("find nonexistent: %v", err)
	}
	if notFound != nil {
		t.Error("expected nil for nonexistent save path")
	}
}