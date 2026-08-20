package httpclient

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/git-pkgs/proxy/internal/accesslog"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestAccessLogTransportRecordsUpstreamStatus(t *testing.T) {
	path := filepath.Join(t.TempDir(), "access.jsonl")
	accessLogger, err := accesslog.Open(path)
	if err != nil {
		t.Fatal(err)
	}

	base := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusTooManyRequests,
			Body:       io.NopCloser(strings.NewReader("rate limited")),
			Request:    req,
		}, nil
	})
	client := &http.Client{Transport: NewAccessLogTransport(base, accessLogger, slog.Default())}
	req, err := http.NewRequest(http.MethodGet, "https://user:password@registry.example/package.tgz?token=secret", nil)
	if err != nil {
		t.Fatal(err)
	}
	req = req.WithContext(accesslog.WithRequestID(req.Context(), "request-123"))

	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if err := accessLogger.Close(); err != nil {
		t.Fatal(err)
	}

	entry := readAccessLogEntry(t, path)
	if entry.Event != accesslog.EventUpstream {
		t.Errorf("event = %q, want %q", entry.Event, accesslog.EventUpstream)
	}
	if entry.RequestID != "request-123" {
		t.Errorf("request_id = %q, want %q", entry.RequestID, "request-123")
	}
	if entry.StatusCode != http.StatusTooManyRequests {
		t.Errorf("status_code = %d, want %d", entry.StatusCode, http.StatusTooManyRequests)
	}
	if entry.URL != "https://registry.example/package.tgz" {
		t.Errorf("url = %q, want URL without credentials or query", entry.URL)
	}
}

func TestAccessLogTransportRecordsUpstreamError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "access.jsonl")
	accessLogger, err := accesslog.Open(path)
	if err != nil {
		t.Fatal(err)
	}

	wantErr := errors.New("GET https://user:password@registry.example/package.tgz?token=secret: connection refused")
	base := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, wantErr
	})
	client := &http.Client{Transport: NewAccessLogTransport(base, accessLogger, slog.Default())}

	_, err = client.Get("https://user:password@registry.example/package.tgz?token=secret")
	if !errors.Is(err, wantErr) {
		t.Fatalf("GET error = %v, want %v", err, wantErr)
	}
	if err := accessLogger.Close(); err != nil {
		t.Fatal(err)
	}

	entry := readAccessLogEntry(t, path)
	if entry.StatusCode != 0 {
		t.Errorf("status_code = %d, want 0", entry.StatusCode)
	}
	if strings.Contains(entry.Error, "password") || strings.Contains(entry.Error, "secret") {
		t.Errorf("error contains URL credentials or query: %q", entry.Error)
	}
	if !strings.Contains(entry.Error, "connection refused") {
		t.Errorf("error = %q, want connection failure", entry.Error)
	}
}

func readAccessLogEntry(t *testing.T, path string) accesslog.Entry {
	t.Helper()

	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()

	scanner := bufio.NewScanner(file)
	if !scanner.Scan() {
		t.Fatalf("access log is empty: %v", scanner.Err())
	}

	var entry accesslog.Entry
	if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
		t.Fatalf("decoding access log: %v", err)
	}
	return entry
}
