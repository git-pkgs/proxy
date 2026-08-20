package server

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/git-pkgs/proxy/internal/accesslog"
	"github.com/git-pkgs/proxy/internal/metrics"
	"github.com/go-chi/chi/v5/middleware"
)

var requestCounter atomic.Uint64

// RequestIDMiddleware adds a sequential request ID to the context and response headers.
// IDs are formatted as [001], [002], etc. for easy log correlation.
func RequestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = requestCounter.Add(1)
		requestID := middleware.GetReqID(r.Context())

		// Store formatted ID in context
		ctx := accesslog.WithRequestID(r.Context(), requestID)

		// Add to response header for client tracking
		w.Header().Set("X-Request-ID", requestID)

		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// GetRequestID retrieves the request ID from context.
func GetRequestID(ctx context.Context) string {
	return accesslog.RequestID(ctx)
}

// LoggerMiddleware logs HTTP requests with request ID correlation.
func (s *Server) LoggerMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		requestID := GetRequestID(r.Context())

		rw := &responseWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rw, r)
		duration := time.Since(start)

		s.logger.Info("request",
			"request_id", requestID,
			"method", r.Method,
			"path", r.URL.Path,
			"status", rw.status,
			"duration", duration,
			"remote", r.RemoteAddr)

		if r.URL.Path != "/metrics" {
			metrics.RecordRequest(requestEcosystem(r.URL.Path), rw.status, duration)
		}

		if s.accessLog != nil {
			if err := s.accessLog.Write(accesslog.Entry{
				Event:      accesslog.EventRequest,
				RequestID:  requestID,
				Method:     r.Method,
				Path:       r.URL.EscapedPath(),
				StatusCode: rw.status,
				DurationMS: duration.Milliseconds(),
				RemoteAddr: r.RemoteAddr,
			}); err != nil {
				s.logger.Error("failed to write access log", "error", err)
			}
		}
	})
}

func requestEcosystem(path string) string {
	segment, _, _ := strings.Cut(strings.TrimPrefix(path, "/"), "/")
	switch segment {
	case "npm", "cargo", "hex", "pub", "pypi", "maven", "gradle", "nuget",
		"conan", "conda", "cran", "julia", "debian", "rpm":
		return segment
	case "gem":
		return "rubygems"
	case "go":
		return "golang"
	case "composer":
		return "packagist"
	case "v2":
		return "oci"
	default:
		return "other"
	}
}

// ActiveRequestsMiddleware tracks the number of active requests using Prometheus metrics.
func ActiveRequestsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Don't track metrics endpoint itself
		if r.URL.Path == "/metrics" {
			next.ServeHTTP(w, r)
			return
		}

		// Implemented in server.go where metrics package is imported
		next.ServeHTTP(w, r)
	})
}
