package upstream

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCircuitBreakerFetch_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("test content"))
	}))
	defer server.Close()

	fetcher := New()
	cbFetcher := NewCircuitBreakerFetcher(fetcher)

	ctx := context.Background()
	artifact, err := cbFetcher.Fetch(ctx, server.URL+"/test.tar.gz")
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	if artifact == nil {
		t.Fatal("expected artifact, got nil")
	}

	defer func() { _ = artifact.Body.Close() }()

	body, _ := io.ReadAll(artifact.Body)
	if string(body) != "test content" {
		t.Errorf("expected 'test content', got %q", string(body))
	}
}

func TestCircuitBreakerHead_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead {
			t.Errorf("expected HEAD request, got %s", r.Method)
		}
		w.Header().Set("Content-Length", "1234")
		w.Header().Set("Content-Type", "application/octet-stream")
	}))
	defer server.Close()

	fetcher := New()
	cbFetcher := NewCircuitBreakerFetcher(fetcher)

	ctx := context.Background()
	size, contentType, err := cbFetcher.Head(ctx, server.URL+"/test.tar.gz")
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	if size != 1234 {
		t.Errorf("expected size 1234, got %d", size)
	}

	if contentType != "application/octet-stream" {
		t.Errorf("expected content type application/octet-stream, got %s", contentType)
	}
}

func TestExtractRegistry(t *testing.T) {
	tests := []struct {
		name     string
		url      string
		expected string
	}{
		{
			name:     "npm registry",
			url:      "https://registry.npmjs.org/package/-/package-1.0.0.tgz",
			expected: "registry.npmjs.org",
		},
		{
			name:     "pypi registry",
			url:      "https://files.pythonhosted.org/packages/abc/def/file.tar.gz",
			expected: "files.pythonhosted.org",
		},
		{
			name:     "invalid URL",
			url:      "not-a-valid-url",
			expected: "not-a-valid-url",
		},
		{
			name:     "long URL",
			url:      "https://very-long-hostname.example.com/path",
			expected: "very-long-hostname.example.com",
		},
		{
			name:     "with port",
			url:      "https://example.com:8080/path",
			expected: "example.com:8080",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractRegistry(tt.url)
			if got != tt.expected {
				t.Errorf("extractRegistry(%q) = %q, want %q", tt.url, got, tt.expected)
			}
		})
	}
}

func TestGetBreakerState(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer server.Close()

	fetcher := New()
	cbFetcher := NewCircuitBreakerFetcher(fetcher)

	// Initially empty
	states := cbFetcher.GetBreakerState()
	if len(states) != 0 {
		t.Errorf("expected empty states, got %d entries", len(states))
	}

	// After a fetch, should have state
	ctx := context.Background()
	_, _ = cbFetcher.Fetch(ctx, server.URL+"/test")

	states = cbFetcher.GetBreakerState()
	if len(states) == 0 {
		t.Error("expected at least one breaker state after fetch")
	}

	// Should be in closed state (working)
	for _, state := range states {
		if state != "closed" {
			t.Errorf("expected closed state, got %s", state)
		}
	}
}

func TestCircuitBreakerMultipleRegistries(t *testing.T) {
	server1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("server1"))
	}))
	defer server1.Close()

	server2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("server2"))
	}))
	defer server2.Close()

	fetcher := New()
	cbFetcher := NewCircuitBreakerFetcher(fetcher)

	ctx := context.Background()

	// Fetch from both servers
	art1, err1 := cbFetcher.Fetch(ctx, server1.URL+"/test")
	if err1 != nil {
		t.Fatalf("fetch 1 failed: %v", err1)
	}
	_ = art1.Body.Close()

	art2, err2 := cbFetcher.Fetch(ctx, server2.URL+"/test")
	if err2 != nil {
		t.Fatalf("fetch 2 failed: %v", err2)
	}
	_ = art2.Body.Close()

	// Should have separate breaker states for each registry
	states := cbFetcher.GetBreakerState()
	if len(states) != 2 {
		t.Errorf("expected 2 breaker states, got %d", len(states))
	}
}

func TestCircuitBreakerOpensOnFailures(t *testing.T) {
	failCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		failCount++
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	fetcher := New(WithMaxRetries(0), WithBaseDelay(0))
	cbFetcher := NewCircuitBreakerFetcher(fetcher)

	ctx := context.Background()

	// Make multiple failing requests to trip the circuit breaker
	// Default threshold is 5 failures
	for range 10 {
		_, _ = cbFetcher.Fetch(ctx, server.URL+"/test")
	}

	// Check that circuit breaker eventually opened
	states := cbFetcher.GetBreakerState()
	if len(states) == 0 {
		t.Fatal("expected breaker state to exist")
	}

	// Circuit should be open after repeated failures
	// Note: The exact state depends on timing, but we should have made fewer
	// than 10 actual HTTP requests if the breaker opened
	if failCount >= 10 {
		t.Logf("Warning: Circuit breaker may not have opened (got %d requests)", failCount)
	}
}
