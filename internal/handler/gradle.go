package handler

import (
	"errors"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/git-pkgs/proxy/internal/storage"
)

const (
	gradleBuildCacheContentType = "application/vnd.gradle.build-cache-artifact.v2"
	gradleBuildCachePathPrefix  = "cache/"
	gradleBuildCacheStorageRoot = "_gradle/http-build-cache"
)

var gradleBuildCacheKeyPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// GradleBuildCacheHandler handles Gradle HttpBuildCache GET/HEAD/PUT requests.
//
// Gradle clients commonly use paths like /cache/{key}, but this handler also
// accepts /{key} so it can be mounted under flexible base URLs.
type GradleBuildCacheHandler struct {
	proxy *Proxy
}

// NewGradleBuildCacheHandler creates a Gradle HttpBuildCache handler.
func NewGradleBuildCacheHandler(proxy *Proxy, _ string) *GradleBuildCacheHandler {
	return &GradleBuildCacheHandler{proxy: proxy}
}

// Routes returns the HTTP handler for Gradle HttpBuildCache requests.
func (h *GradleBuildCacheHandler) Routes() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodPut:
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		key, statusCode := h.parseCacheKey(r.URL.Path)
		if statusCode != http.StatusOK {
			if statusCode == http.StatusNotFound {
				http.NotFound(w, r)
				return
			}
			http.Error(w, "invalid cache key", statusCode)
			return
		}

		if r.Method == http.MethodPut {
			h.handlePut(w, r, key)
			return
		}

		h.handleGetOrHead(w, r, key)
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

	if strings.HasPrefix(keyPath, gradleBuildCachePathPrefix) {
		keyPath = strings.TrimPrefix(keyPath, gradleBuildCachePathPrefix)
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

	reader, err := h.proxy.Storage.Open(r.Context(), storagePath)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		h.proxy.Logger.Error("failed to open gradle build cache entry", "key", key, "error", err)
		http.Error(w, "failed to read cache entry", http.StatusInternalServerError)
		return
	}
	defer func() { _ = reader.Close() }()

	w.Header().Set("Content-Type", gradleBuildCacheContentType)
	if size, err := h.proxy.Storage.Size(r.Context(), storagePath); err == nil && size >= 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	}

	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}

	_, _ = io.Copy(w, reader)
}

func (h *GradleBuildCacheHandler) handlePut(w http.ResponseWriter, r *http.Request, key string) {
	storagePath := h.cacheStoragePath(key)

	exists, err := h.proxy.Storage.Exists(r.Context(), storagePath)
	if err != nil {
		h.proxy.Logger.Error("failed to check gradle build cache entry", "key", key, "error", err)
		http.Error(w, "failed to write cache entry", http.StatusInternalServerError)
		return
	}

	defer func() { _ = r.Body.Close() }()
	size, hash, err := h.proxy.Storage.Store(r.Context(), storagePath, r.Body)
	if err != nil {
		h.proxy.Logger.Error("failed to store gradle build cache entry", "key", key, "error", err)
		http.Error(w, "failed to write cache entry", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Length", "0")
	w.Header().Set("ETag", `"`+hash+`"`)
	w.Header().Set("X-Cache-Size", strconv.FormatInt(size, 10))

	if exists {
		w.WriteHeader(http.StatusOK)
		return
	}

	w.WriteHeader(http.StatusCreated)
}
