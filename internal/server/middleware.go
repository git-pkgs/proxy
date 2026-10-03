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

		// Deferred because a truncated upstream relay aborts the handler with
		// http.ErrAbortHandler, which would otherwise leave the request out of
		// the log, the metrics and the access log entirely.
		defer func() {
			duration := time.Since(start)
			userAgent := r.UserAgent()
			client := clientName(userAgent)
			addr := clientAddr(r, s.trustsForwardedFor())
			ecosystem := requestEcosystem(r.URL.Path)

			s.logger.Info("request",
				"request_id", requestID,
				"method", r.Method,
				"path", r.URL.Path,
				"status", rw.status,
				"duration", duration,
				"bytes", rw.bytes,
				"client", client,
				"remote", r.RemoteAddr,
				"remote_ip", addr)

			// Scrapes of /metrics would otherwise attribute themselves,
			// burying real callers under whatever polls the proxy most often.
			if r.URL.Path != "/metrics" {
				metrics.RecordRequest(ecosystem, rw.status, duration)
				metrics.RecordResponse(ecosystem, client, rw.bytes)
				s.sources.Record(addr, client, rw.bytes)
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
					RemoteIP:   addr,
					UserAgent:  userAgent,
					Client:     client,
					Ecosystem:  ecosystem,
					Bytes:      rw.bytes,
				}); err != nil {
					s.logger.Error("failed to write access log", "error", err)
				}
			}
		}()

		next.ServeHTTP(rw, r)
	})
}

// requestEcosystem names the ecosystem a request path belongs to.
//
// Every mounted package route must appear here. An unlisted one falls to
// "other" along with the UI, health and metrics paths, pooling a real
// ecosystem's traffic with traffic that belongs to no ecosystem at all.
func requestEcosystem(path string) string {
	segment, _, _ := strings.Cut(strings.TrimPrefix(path, "/"), "/")
	switch segment {
	case "npm", "cargo", "hex", "pub", "pypi", "maven", "gradle", "nuget",
		"conan", "conda", "cran", "julia", "debian", "rpm",
		"helm", "homebrew", "generic", "swift", "url":
		return segment
	case "apk":
		return "alpine"
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
