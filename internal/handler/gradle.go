package handler

import (
	"errors"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/git-pkgs/proxy/internal/metrics"
	"github.com/git-pkgs/proxy/internal/storage"
)

const (
	gradleBuildCacheContentType = "application/vnd.gradle.build-cache-artifact.v2"
	gradleBuildCacheStorageRoot = "_gradle/http-build-cache"
	defaultGradleMaxUploadSize  = 100 << 20
)

var gradleBuildCacheKeyPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// GradleBuildCacheHandler handles Gradle HttpBuildCache GET/HEAD/PUT requests.
//
// This handler accepts /{key} when mounted under a base URL.
type GradleBuildCacheHandler struct {
	proxy *Proxy
}

// NewGradleBuildCacheHandler creates a Gradle HttpBuildCache handler.
func NewGradleBuildCacheHandler(proxy *Proxy) *GradleBuildCacheHandler {
	return &GradleBuildCacheHandler{proxy: proxy}
}

// Routes returns the HTTP handler for Gradle HttpBuildCache requests.
func (h *GradleBuildCacheHandler) Routes() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &statusCapturingResponseWriter{ResponseWriter: w, status: http.StatusOK}
		defer func() {
			metrics.RecordRequest("gradle", rw.status, time.Since(start))
		}()

		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodPut:
		default:
			http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		key, statusCode := h.parseCacheKey(r.URL.Path)
		if statusCode != http.StatusOK {
			if statusCode == http.StatusNotFound {
				http.NotFound(rw, r)
				return
			}
			http.Error(rw, "invalid cache key", statusCode)
			return
		}

		if r.Method == http.MethodPut {
			if h.proxy.GradleReadOnly {
				http.Error(rw, "gradle build cache is read-only", http.StatusMethodNotAllowed)
				return
			}
			h.handlePut(rw, r, key)
			return
		}

		h.handleGetOrHead(rw, r, key)
	})
}

func (h *GradleBuildCacheHandler) parseCacheKey(urlPath string) (string, int) {
	keyPath := strings.TrimPrefix(urlPath, "/")
	if keyPath == "" {
		return "", http.StatusNotFound
	}

	if containsPathTraversal(keyPath) {
		return "", http.StatusBadRequest
	}

	if keyPath == "" || strings.Contains(keyPath, "/") {
		return "", http.StatusNotFound
	}

	if !gradleBuildCacheKeyPattern.MatchString(keyPath) {
		return "", http.StatusBadRequest
	}

	return keyPath, http.StatusOK
}

func (h *GradleBuildCacheHandler) cacheStoragePath(key string) string {
	return gradleBuildCacheStorageRoot + "/" + key
}

func (h *GradleBuildCacheHandler) handleGetOrHead(w http.ResponseWriter, r *http.Request, key string) {
	storagePath := h.cacheStoragePath(key)
	w.Header().Set("Content-Type", gradleBuildCacheContentType)

	if r.Method == http.MethodHead {
		existsStart := time.Now()
		exists, err := h.proxy.Storage.Exists(r.Context(), storagePath)
		metrics.RecordStorageOperation("read", time.Since(existsStart))
		if err != nil {
			metrics.RecordStorageError("read")
			h.proxy.Logger.Error("failed to check gradle build cache entry", "key", key, "error", err)
			http.Error(w, "failed to read cache entry", http.StatusInternalServerError)
			return
		}
		if !exists {
			metrics.RecordCacheMiss("gradle")
			http.NotFound(w, r)
			return
		}
		metrics.RecordCacheHit("gradle")

		sizeStart := time.Now()
		if size, err := h.proxy.Storage.Size(r.Context(), storagePath); err == nil && size >= 0 {
			metrics.RecordStorageOperation("read", time.Since(sizeStart))
			w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
		} else if err != nil {
			metrics.RecordStorageOperation("read", time.Since(sizeStart))
			metrics.RecordStorageError("read")
		}

		w.WriteHeader(http.StatusOK)
		return
	}

	readStart := time.Now()
	reader, err := h.proxy.Storage.Open(r.Context(), storagePath)
	metrics.RecordStorageOperation("read", time.Since(readStart))
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			metrics.RecordCacheMiss("gradle")
			http.NotFound(w, r)
			return
		}
		metrics.RecordStorageError("read")
		h.proxy.Logger.Error("failed to open gradle build cache entry", "key", key, "error", err)
		http.Error(w, "failed to read cache entry", http.StatusInternalServerError)
		return
	}
	defer func() { _ = reader.Close() }()
	metrics.RecordCacheHit("gradle")

	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, reader)
}

func (h *GradleBuildCacheHandler) handlePut(w http.ResponseWriter, r *http.Request, key string) {
	storagePath := h.cacheStoragePath(key)
	maxUploadSize := h.proxy.GradleMaxUploadSize
	if maxUploadSize <= 0 {
		maxUploadSize = defaultGradleMaxUploadSize
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxUploadSize)

	storeStart := time.Now()
	_, hash, err := h.proxy.Storage.Store(r.Context(), storagePath, r.Body)
	metrics.RecordStorageOperation("write", time.Since(storeStart))
	if err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			http.Error(w, "cache entry too large", http.StatusRequestEntityTooLarge)
			return
		}

		metrics.RecordStorageError("write")
		h.proxy.Logger.Error("failed to store gradle build cache entry", "key", key, "error", err)
		http.Error(w, "failed to write cache entry", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Length", "0")
	w.Header().Set("ETag", `"`+hash+`"`)

	w.WriteHeader(http.StatusCreated)
}

type statusCapturingResponseWriter struct {
	http.ResponseWriter
	status int
}

func (rw *statusCapturingResponseWriter) WriteHeader(code int) {
	rw.status = code
	rw.ResponseWriter.WriteHeader(code)
}
