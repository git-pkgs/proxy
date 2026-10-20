package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/git-pkgs/proxy/internal/config"
)

// Exercise the real serve wiring: a slow artifact must use artifact_timeout,
// while waiting for it also consumes the independent response write deadline.
func TestServeRequestTimeouts(t *testing.T) {
	for _, tc := range []struct {
		name, write, artifact string
		wantOK                bool
		wantConnectionError   bool
	}{
		{"independent from metadata timeout", "5s", "5s", true, false},
		{"write deadline expires while waiting", "1ms", "5s", false, true},
		{"artifact download times out", "5s", "1ms", false, false},
		{"both timeouts disabled", "0", "0", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, body, err := fetchWithRequestTimeouts(t, tc.write, tc.artifact)
			if tc.wantConnectionError {
				if err == nil {
					t.Fatal("response succeeded after its write deadline")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantOK {
				if status != http.StatusOK || body != "release asset" {
					t.Fatalf("response = %d %q", status, body)
				}
			} else if status < 500 {
				t.Fatalf("artifact timeout response = %d %q, want server error", status, body)
			}
		})
	}
}

func fetchWithRequestTimeouts(t *testing.T, writeTimeout, artifactTimeout string) (int, string, error) {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
		_, _ = io.WriteString(w, "release asset")
	}))
	defer upstream.Close()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	dir := t.TempDir()
	cfg := config.Default()
	cfg.Listen = listener.Addr().String()
	cfg.BaseURL = "http://" + cfg.Listen
	cfg.Database.Path = filepath.Join(dir, "proxy.db")
	cfg.Storage.URL = "file://" + filepath.Join(dir, "artifacts")
	cfg.Upstream.Generic = map[string]string{"releases": upstream.URL}
	cfg.Upstream.AllowLoopback = true
	cfg.HTTPTimeout = "1ns"
	cfg.WriteTimeout, cfg.ArtifactTimeout = writeTimeout, artifactTimeout
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	srv, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), BuildInfo{Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- srv.Start(listener) }()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			t.Error(err)
		}
		if err := <-done; !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("Start() = %v", err)
		}
	}()
	client := &http.Client{Timeout: 10 * time.Second}
	defer client.CloseIdleConnections()

	resp, err := client.Get(cfg.BaseURL + "/generic/releases/owner/repo/releases/download/v1/asset.bin")
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body), err
}

func TestMirrorIgnoresServeArtifactTimeout(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(20 * time.Millisecond)
		_, _ = io.WriteString(w, "mirrored artifact")
	}))
	defer upstream.Close()
	db, store := setupEvictionTest(t)
	defer func() { _ = store.Close() }()
	cfg := config.Default()
	cfg.Upstream.AllowLoopback = true
	cfg.ArtifactTimeout = "1ns"
	cfg.WriteTimeout = "1ns"
	proxy, _ := NewMirrorProxy(cfg, db, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer proxy.HTTPClient.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := proxy.GetOrFetchArtifactFromURL(ctx, "generic", "mirror-timeout", "v1", "asset.bin", upstream.URL+"/asset.bin")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = result.Reader.Close() }()
	body, err := io.ReadAll(result.Reader)
	if err != nil || string(body) != "mirrored artifact" {
		t.Fatalf("mirror result = %q, %v", body, err)
	}
}
