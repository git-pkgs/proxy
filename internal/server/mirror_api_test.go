package server

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/git-pkgs/proxy/internal/database"
	"github.com/git-pkgs/proxy/internal/handler"
	"github.com/git-pkgs/proxy/internal/mirror"
	"github.com/git-pkgs/proxy/internal/storage"
	"github.com/git-pkgs/registries/fetch"
	"github.com/go-chi/chi/v5"
)

func setupMirrorAPI(t *testing.T) *MirrorAPIHandler {
	t.Helper()

	dbPath := t.TempDir() + "/test.db"
	db, err := database.Create(dbPath)
	if err != nil {
		t.Fatalf("creating database: %v", err)
	}
	if err := db.MigrateSchema(); err != nil {
		t.Fatalf("migrating schema: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	storeDir := t.TempDir()
	store, err := storage.OpenBucket(context.Background(), "file://"+storeDir)
	if err != nil {
		t.Fatalf("opening storage: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	fetcher := fetch.NewFetcher()
	resolver := fetch.NewResolver()
	proxy := handler.NewProxy(db, store, fetcher, resolver, logger)

	m := mirror.New(proxy, db, store, logger, 1)
	js := mirror.NewJobStore(m)
	return NewMirrorAPIHandler(js)
}

func TestMirrorAPICreateJob(t *testing.T) {
	h := setupMirrorAPI(t)

	body, _ := json.Marshal(mirror.JobRequest{
		PURLs: []string{"pkg:npm/lodash@4.17.21"},
	})

	req := httptest.NewRequest("POST", "/api/mirror", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.HandleCreate(w, req)

	if w.Code != http.StatusAccepted {
		t.Errorf("status = %d, want %d", w.Code, http.StatusAccepted)
	}

	var resp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp["id"] == "" {
		t.Error("expected non-empty job ID")
	}
}

func TestMirrorAPICreateInvalidBody(t *testing.T) {
	h := setupMirrorAPI(t)

	req := httptest.NewRequest("POST", "/api/mirror", bytes.NewReader([]byte("not json")))
	w := httptest.NewRecorder()
	h.HandleCreate(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestMirrorAPICreateEmptyRequest(t *testing.T) {
	h := setupMirrorAPI(t)

	body, _ := json.Marshal(mirror.JobRequest{})
	req := httptest.NewRequest("POST", "/api/mirror", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.HandleCreate(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestMirrorAPIGetNotFound(t *testing.T) {
	h := setupMirrorAPI(t)

	r := chi.NewRouter()
	r.Get("/api/mirror/{id}", h.HandleGet)

	req := httptest.NewRequest("GET", "/api/mirror/nonexistent", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestMirrorAPICancelNotFound(t *testing.T) {
	h := setupMirrorAPI(t)

	r := chi.NewRouter()
	r.Delete("/api/mirror/{id}", h.HandleCancel)

	req := httptest.NewRequest("DELETE", "/api/mirror/nonexistent", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestMirrorAPICreateAndGetJob(t *testing.T) {
	h := setupMirrorAPI(t)

	// Create a job
	body, _ := json.Marshal(mirror.JobRequest{
		PURLs: []string{"pkg:npm/lodash@4.17.21"},
	})
	createReq := httptest.NewRequest("POST", "/api/mirror", bytes.NewReader(body))
	createW := httptest.NewRecorder()
	h.HandleCreate(createW, createReq)

	var createResp map[string]string
	_ = json.NewDecoder(createW.Body).Decode(&createResp)
	jobID := createResp["id"]

	// Get the job
	r := chi.NewRouter()
	r.Get("/api/mirror/{id}", h.HandleGet)

	getReq := httptest.NewRequest("GET", "/api/mirror/"+jobID, nil)
	getW := httptest.NewRecorder()
	r.ServeHTTP(getW, getReq)

	if getW.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", getW.Code, http.StatusOK)
	}

	var job mirror.Job
	if err := json.NewDecoder(getW.Body).Decode(&job); err != nil {
		t.Fatalf("decoding job: %v", err)
	}
	if job.ID != jobID {
		t.Errorf("job ID = %q, want %q", job.ID, jobID)
	}
}
