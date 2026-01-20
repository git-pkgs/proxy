package upstream

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestFetchSuccess(t *testing.T) {
	content := "test artifact content"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/gzip")
		w.Header().Set("Content-Length", "21")
		w.Header().Set("ETag", `"abc123"`)
		_, _ = w.Write([]byte(content))
	}))
	defer server.Close()

	f := New()
	artifact, err := f.Fetch(context.Background(), server.URL+"/test.tgz")
	if err != nil {
		t.Fatalf("Fetch failed: %v", err)
	}
	defer func() { _ = artifact.Body.Close() }()

	if artifact.Size != 21 {
		t.Errorf("Size = %d, want 21", artifact.Size)
	}
	if artifact.ContentType != "application/gzip" {
		t.Errorf("ContentType = %q, want %q", artifact.ContentType, "application/gzip")
	}
	if artifact.ETag != `"abc123"` {
		t.Errorf("ETag = %q, want %q", artifact.ETag, `"abc123"`)
	}

	body, err := io.ReadAll(artifact.Body)
	if err != nil {
		t.Fatalf("ReadAll failed: %v", err)
	}
	if string(body) != content {
		t.Errorf("body = %q, want %q", string(body), content)
	}
}

func TestFetchNotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	f := New()
	_, err := f.Fetch(context.Background(), server.URL+"/missing.tgz")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Fetch = %v, want ErrNotFound", err)
	}
}

func TestFetchRateLimitRetry(t *testing.T) {
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts < 3 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte("success"))
	}))
	defer server.Close()

	f := New(WithBaseDelay(10 * time.Millisecond))
	artifact, err := f.Fetch(context.Background(), server.URL+"/test.tgz")
	if err != nil {
		t.Fatalf("Fetch failed: %v", err)
	}
	defer func() { _ = artifact.Body.Close() }()

	if attempts != 3 {
		t.Errorf("attempts = %d, want 3", attempts)
	}
}

func TestFetchServerErrorRetry(t *testing.T) {
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts < 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("success"))
	}))
	defer server.Close()

	f := New(WithBaseDelay(10 * time.Millisecond))
	artifact, err := f.Fetch(context.Background(), server.URL+"/test.tgz")
	if err != nil {
		t.Fatalf("Fetch failed: %v", err)
	}
	defer func() { _ = artifact.Body.Close() }()

	if attempts != 2 {
		t.Errorf("attempts = %d, want 2", attempts)
	}
}

func TestFetchMaxRetries(t *testing.T) {
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	f := New(WithMaxRetries(2), WithBaseDelay(10*time.Millisecond))
	_, err := f.Fetch(context.Background(), server.URL+"/test.tgz")
	if err == nil {
		t.Error("expected error after max retries")
	}
	if !errors.Is(err, ErrUpstreamDown) {
		t.Errorf("expected ErrUpstreamDown, got %v", err)
	}

	// Initial attempt + 2 retries = 3 total
	if attempts != 3 {
		t.Errorf("attempts = %d, want 3", attempts)
	}
}

func TestFetchContextCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
		_, _ = w.Write([]byte("success"))
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	f := New()
	_, err := f.Fetch(ctx, server.URL+"/test.tgz")
	if err == nil {
		t.Error("expected error on context cancellation")
	}
}

func TestFetchUnknownSize(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Chunked encoding, no Content-Length
		w.Header().Set("Transfer-Encoding", "chunked")
		_, _ = w.Write([]byte("chunk1"))
	}))
	defer server.Close()

	f := New()
	artifact, err := f.Fetch(context.Background(), server.URL+"/test.tgz")
	if err != nil {
		t.Fatalf("Fetch failed: %v", err)
	}
	defer func() { _ = artifact.Body.Close() }()

	if artifact.Size != -1 {
		t.Errorf("Size = %d, want -1 for unknown", artifact.Size)
	}
}

func TestHead(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead {
			t.Errorf("Method = %s, want HEAD", r.Method)
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", "12345")
	}))
	defer server.Close()

	f := New()
	size, contentType, err := f.Head(context.Background(), server.URL+"/test.tgz")
	if err != nil {
		t.Fatalf("Head failed: %v", err)
	}

	if size != 12345 {
		t.Errorf("size = %d, want 12345", size)
	}
	if contentType != "application/octet-stream" {
		t.Errorf("contentType = %q, want %q", contentType, "application/octet-stream")
	}
}

func TestHeadNotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	f := New()
	_, _, err := f.Head(context.Background(), server.URL+"/missing.tgz")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Head = %v, want ErrNotFound", err)
	}
}

func TestFetchUserAgent(t *testing.T) {
	var receivedUA string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedUA = r.Header.Get("User-Agent")
		_, _ = w.Write([]byte("ok"))
	}))
	defer server.Close()

	f := New(WithUserAgent("custom-agent/2.0"))
	artifact, _ := f.Fetch(context.Background(), server.URL+"/test.tgz")
	if artifact != nil {
		_ = artifact.Body.Close()
	}

	if receivedUA != "custom-agent/2.0" {
		t.Errorf("User-Agent = %q, want %q", receivedUA, "custom-agent/2.0")
	}
}

func TestFetchLargeArtifact(t *testing.T) {
	// 1MB artifact
	content := strings.Repeat("x", 1024*1024)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(content))
	}))
	defer server.Close()

	f := New()
	artifact, err := f.Fetch(context.Background(), server.URL+"/large.tgz")
	if err != nil {
		t.Fatalf("Fetch failed: %v", err)
	}
	defer func() { _ = artifact.Body.Close() }()

	body, err := io.ReadAll(artifact.Body)
	if err != nil {
		t.Fatalf("ReadAll failed: %v", err)
	}
	if len(body) != len(content) {
		t.Errorf("body length = %d, want %d", len(body), len(content))
	}
}
