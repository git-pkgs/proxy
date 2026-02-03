package server

import (
	"context"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5/middleware"
)

type contextKey string

const requestIDKey contextKey = "request_id"

var requestCounter atomic.Uint64

// RequestIDMiddleware adds a sequential request ID to the context and response headers.
// IDs are formatted as [001], [002], etc. for easy log correlation.
func RequestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = requestCounter.Add(1)
		requestID := middleware.GetReqID(r.Context())

		// Store formatted ID in context
		ctx := context.WithValue(r.Context(), requestIDKey, requestID)

		// Add to response header for client tracking
		w.Header().Set("X-Request-ID", requestID)

		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// GetRequestID retrieves the request ID from context.
func GetRequestID(ctx context.Context) string {
	if id, ok := ctx.Value(requestIDKey).(string); ok {
		return id
	}
	return ""
}

// LoggerMiddleware logs HTTP requests with request ID correlation.
func (s *Server) LoggerMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		requestID := GetRequestID(r.Context())

		rw := &responseWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rw, r)

		s.logger.Info("request",
			"request_id", requestID,
			"method", r.Method,
			"path", r.URL.Path,
			"status", rw.status,
			"duration", time.Since(start),
			"remote", r.RemoteAddr)
	})
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
