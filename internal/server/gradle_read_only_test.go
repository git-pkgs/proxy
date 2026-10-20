package server

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/git-pkgs/proxy/internal/config"
)

func startGradleServer(t *testing.T, readOnly bool) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving proxy address: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	tempDir := t.TempDir()
	cfg := config.Default()
	cfg.Listen = listener.Addr().String()
	cfg.BaseURL = "http://" + cfg.Listen
	cfg.Database.Path = filepath.Join(tempDir, "proxy.db")
	cfg.Storage.URL = "file://" + filepath.Join(tempDir, "artifacts")
	cfg.Gradle.BuildCache.ReadOnly = readOnly
	if err := cfg.Validate(); err != nil {
		t.Fatalf("validating config: %v", err)
	}

	proxyServer, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), BuildInfo{Version: "test", Commit: "test"})
	if err != nil {
		t.Fatalf("creating server: %v", err)
	}
	startErr := make(chan error, 1)
	go func() { startErr <- proxyServer.Start(listener) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = proxyServer.Shutdown(ctx)
		<-startErr
	})
	return cfg.BaseURL
}

func gradleRequest(t *testing.T, method, url, body string) int {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

func TestGradleBuildCacheReadOnlyFollowsConfig(t *testing.T) {
	readOnly := startGradleServer(t, config.Default().Gradle.BuildCache.ReadOnly)
	if got := gradleRequest(t, http.MethodPut, readOnly+"/gradle/abc123", "entry"); got != http.StatusMethodNotAllowed {
		t.Errorf("PUT with default config = %d, want %d", got, http.StatusMethodNotAllowed)
	}
	if got := gradleRequest(t, http.MethodGet, readOnly+"/gradle/abc123", ""); got != http.StatusNotFound {
		t.Errorf("GET after rejected PUT = %d, want %d", got, http.StatusNotFound)
	}

	writable := startGradleServer(t, false)
	if got := gradleRequest(t, http.MethodPut, writable+"/gradle/abc123", "entry"); got != http.StatusCreated {
		t.Errorf("PUT with read_only false = %d, want %d", got, http.StatusCreated)
	}
	if got := gradleRequest(t, http.MethodGet, writable+"/gradle/abc123", ""); got != http.StatusOK {
		t.Errorf("GET after PUT = %d, want %d", got, http.StatusOK)
	}
}
