// Package accesslog writes proxy activity as JSON Lines.
package accesslog

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"sync"
	"time"
)

const (
	accessLogFileMode os.FileMode = 0o600

	// EventRequest identifies the response sent by the proxy to a client.
	EventRequest = "request"
	// EventUpstream identifies one HTTP exchange with an upstream service.
	EventUpstream = "upstream"
)

type requestIDKey struct{}

// Entry is one proxy activity record.
type Entry struct {
	Time       time.Time `json:"time"`
	Event      string    `json:"event"`
	RequestID  string    `json:"request_id,omitempty"`
	Method     string    `json:"method"`
	Path       string    `json:"path,omitempty"`
	URL        string    `json:"url,omitempty"`
	StatusCode int       `json:"status_code,omitempty"`
	DurationMS int64     `json:"duration_ms"`
	RemoteAddr string    `json:"remote_addr,omitempty"`
	Error      string    `json:"error,omitempty"`
}

// Logger appends complete JSON objects to a file, one per line.
type Logger struct {
	mu      sync.Mutex
	file    *os.File
	encoder *json.Encoder
}

// Open opens path for append, creating it with owner-only permissions when needed.
func Open(path string) (*Logger, error) {
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, accessLogFileMode)
	if err != nil {
		return nil, fmt.Errorf("opening access log: %w", err)
	}

	return &Logger{
		file:    file,
		encoder: json.NewEncoder(file),
	}, nil
}

// Write appends an entry to the log.
func (l *Logger) Write(entry Entry) error {
	if entry.Time.IsZero() {
		entry.Time = time.Now().UTC()
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	if err := l.encoder.Encode(entry); err != nil {
		return fmt.Errorf("writing access log: %w", err)
	}
	return nil
}

// Close closes the log file after any active writer finishes.
func (l *Logger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if err := l.file.Close(); err != nil {
		return fmt.Errorf("closing access log: %w", err)
	}
	return nil
}

// WithRequestID stores a proxy request ID in ctx.
func WithRequestID(ctx context.Context, requestID string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, requestID)
}

// RequestID returns the proxy request ID stored in ctx.
func RequestID(ctx context.Context) string {
	requestID, _ := ctx.Value(requestIDKey{}).(string)
	return requestID
}

// URLWithoutSecrets returns a URL without user information, query values, or fragments.
func URLWithoutSecrets(value *url.URL) string {
	if value == nil {
		return ""
	}

	clean := *value
	clean.User = nil
	clean.RawQuery = ""
	clean.ForceQuery = false
	clean.Fragment = ""
	clean.RawFragment = ""
	return clean.String()
}
