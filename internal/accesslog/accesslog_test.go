package accesslog

import (
	"bufio"
	"context"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestLoggerWritesJSONLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "access.jsonl")
	logger, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}

	const entries = 20
	var wg sync.WaitGroup
	for range entries {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := logger.Write(Entry{
				Event:      EventUpstream,
				RequestID:  "request-id",
				Method:     "GET",
				URL:        "https://registry.example/packages/example",
				StatusCode: 429,
			}); err != nil {
				t.Errorf("Write: %v", err)
			}
		}()
	}
	wg.Wait()

	if err := logger.Close(); err != nil {
		t.Fatal(err)
	}

	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()

	scanner := bufio.NewScanner(file)
	count := 0
	for scanner.Scan() {
		var entry Entry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			t.Fatalf("line %d is not JSON: %v", count+1, err)
		}
		if entry.Time.IsZero() {
			t.Errorf("line %d has no time", count+1)
		}
		if entry.StatusCode != 429 {
			t.Errorf("line %d status_code = %d, want 429", count+1, entry.StatusCode)
		}
		count++
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if count != entries {
		t.Errorf("lines = %d, want %d", count, entries)
	}
}

func TestRequestID(t *testing.T) {
	ctx := WithRequestID(context.Background(), "abc-123")
	if got := RequestID(ctx); got != "abc-123" {
		t.Errorf("RequestID = %q, want %q", got, "abc-123")
	}
}

func TestURLWithoutSecrets(t *testing.T) {
	value, err := url.Parse("https://user:password@registry.example/package.tgz?token=secret#fragment")
	if err != nil {
		t.Fatal(err)
	}

	got := URLWithoutSecrets(value)
	want := "https://registry.example/package.tgz"
	if got != want {
		t.Errorf("URLWithoutSecrets = %q, want %q", got, want)
	}
}
