package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/git-pkgs/proxy/internal/config"
	"github.com/git-pkgs/proxy/internal/database"
	"github.com/git-pkgs/proxy/internal/handler"
	"github.com/git-pkgs/proxy/internal/storage"
	"github.com/git-pkgs/registries/fetch"
	"github.com/go-chi/chi/v5"
)

// TestAPKSnapshotsConfigured checks that upstream.apk_snapshots enables the
// dated APK paths, with the digest the file storage records for the cached
// index matching the copy kept as a snapshot.
func TestAPKSnapshotsConfigured(t *testing.T) {
	const index = "signed index"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, index)
	}))
	defer upstream.Close()

	db, err := database.Create(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("creating database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store, err := storage.OpenBucket(context.Background(), "file://"+t.TempDir())
	if err != nil {
		t.Fatalf("opening storage: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	proxy := handler.NewProxy(db, store, fetch.NewFetcher(), fetch.NewResolver(), logger)
	proxy.HTTPClient = upstream.Client()
	proxy.CacheMetadata = true

	cfg := config.Default()
	cfg.BaseURL = "http://proxy.test"
	cfg.Upstream.APK = map[string]string{"alpine": upstream.URL}
	cfg.Upstream.APKSnapshots = "30d"
	s := &Server{cfg: cfg, logger: logger}
	r := chi.NewRouter()
	s.mountProtocolHandlers(r, proxy)

	today := time.Now().UTC().Format("2006-01-02")
	for _, target := range []string{
		"/apk/alpine/v3.22/main/x86_64/APKINDEX.tar.gz",
		"/apk/alpine@" + today + "/v3.22/main/x86_64/APKINDEX.tar.gz",
	} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, target, nil))
		if w.Code != http.StatusOK || w.Body.String() != index {
			t.Fatalf("%s: status = %d, body = %q, want 200 %q", target, w.Code, w.Body.String(), index)
		}
	}
}
