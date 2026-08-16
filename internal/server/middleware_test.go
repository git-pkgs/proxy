package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/git-pkgs/proxy/internal/accesslog"
	"github.com/go-chi/chi/v5/middleware"
)

func TestRequestIDMiddleware(t *testing.T) {
	// Chain with chi's RequestID middleware first
	handler := middleware.RequestID(RequestIDMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := GetRequestID(r.Context())
		if requestID == "" {
			t.Error("expected request ID in context, got empty string")
		}

		// Check response header
		if w.Header().Get("X-Request-ID") == "" {
			t.Error("expected X-Request-ID header to be set")
		}

		w.WriteHeader(http.StatusOK)
	})))

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", rec.Code)
	}
}

func TestGetRequestID(t *testing.T) {
	tests := []struct {
		name     string
		ctx      context.Context
		expected string
	}{
		{
			name:     "with request ID",
			ctx:      accesslog.WithRequestID(context.Background(), "test-123"),
			expected: "test-123",
		},
		{
			name:     "without request ID",
			ctx:      context.Background(),
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := GetRequestID(tt.ctx)
			if got != tt.expected {
				t.Errorf("GetRequestID() = %q, want %q", got, tt.expected)
			}
		})
	}
}

func TestActiveRequestsMiddleware(t *testing.T) {
	handler := ActiveRequestsMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", rec.Code)
	}
}

func TestActiveRequestsMiddleware_SkipsMetricsEndpoint(t *testing.T) {
	handler := ActiveRequestsMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", rec.Code)
	}
}

func TestLoggerMiddleware(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := &Server{logger: logger}

	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusCreated)
	})

	handler := s.LoggerMiddleware(next)

	req := httptest.NewRequest(http.MethodGet, "/test-path", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if !called {
		t.Error("expected next handler to be called")
	}

	if rec.Code != http.StatusCreated {
		t.Errorf("expected status 201, got %d", rec.Code)
	}
}

func TestLoggerMiddlewareWritesAccessLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "access.jsonl")
	activityLog, err := accesslog.Open(path)
	if err != nil {
		t.Fatal(err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := &Server{logger: logger, accessLog: activityLog}
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	handler := middleware.RequestID(RequestIDMiddleware(s.LoggerMiddleware(next)))

	req := httptest.NewRequest(http.MethodGet, "/packages/example?token=secret", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if err := activityLog.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	var entry accesslog.Entry
	if err := json.Unmarshal(data, &entry); err != nil {
		t.Fatalf("decoding access log: %v", err)
	}
	if entry.Event != accesslog.EventRequest {
		t.Errorf("event = %q, want %q", entry.Event, accesslog.EventRequest)
	}
	if entry.RequestID == "" {
		t.Error("request_id is empty")
	}
	if entry.Path != "/packages/example" {
		t.Errorf("path = %q, want query string omitted", entry.Path)
	}
	if entry.StatusCode != http.StatusNotFound {
		t.Errorf("status_code = %d, want %d", entry.StatusCode, http.StatusNotFound)
	}
}

func TestResponseWriter_WriteHeader(t *testing.T) {
	tests := []struct {
		name   string
		status int
	}{
		{"ok", http.StatusOK},
		{"not found", http.StatusNotFound},
		{"internal error", http.StatusInternalServerError},
		{"created", http.StatusCreated},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			rw := &responseWriter{ResponseWriter: rec, status: http.StatusOK}
			rw.WriteHeader(tc.status)

			if rw.status != tc.status {
				t.Errorf("expected status %d, got %d", tc.status, rw.status)
			}
			if rec.Code != tc.status {
				t.Errorf("expected underlying recorder status %d, got %d", tc.status, rec.Code)
			}
		})
	}
}
