package handler

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/git-pkgs/proxy/internal/database"
)

const (
	containerManifestCacheEcosystem = "oci-manifest"
	containerStaleWarning           = `110 - "Response is Stale"`
)

var manifestDigestReferencePattern = regexp.MustCompile(`^[a-z0-9]+:[a-f0-9]+$`)

type cachedContainerManifest struct {
	body          []byte
	contentType   string
	contentDigest string
	etag          string
	size          int64
	fetchedAt     time.Time
}

func (h *ContainerHandler) serveManifest(w http.ResponseWriter, r *http.Request, name, reference string) {
	accept := containerManifestAccept(r)
	cacheKey := h.containerManifestCacheKey(name, reference, accept)
	cached, err := h.loadContainerManifest(r.Context(), cacheKey)
	if err != nil {
		h.proxy.Logger.Warn("failed to read cached container manifest", "error", err)
		cached = nil
	}

	immutable := manifestDigestReferencePattern.MatchString(reference)
	if cached != nil && (immutable || h.containerManifestFresh(cached)) {
		writeContainerManifest(w, r.Method, cached, false)
		return
	}

	upstreamURL := fmt.Sprintf("%s/v2/%s/manifests/%s", h.registryURL, name, reference)
	req, err := http.NewRequestWithContext(r.Context(), r.Method, upstreamURL, nil)
	if err != nil {
		h.containerError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to create request")
		return
	}
	req.Header.Set("Accept", accept)
	if cached != nil && cached.etag != "" {
		req.Header.Set("If-None-Match", cached.etag)
	}

	resp, err := h.proxy.HTTPClient.Do(req)
	if err != nil {
		h.serveStaleManifestOrError(w, r, cached, err)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotModified && cached != nil {
		cached.fetchedAt = time.Now()
		if err := h.storeContainerManifest(r.Context(), cacheKey, cached); err != nil {
			h.proxy.Logger.Warn("failed to refresh cached container manifest", "error", err)
		}
		writeContainerManifest(w, r.Method, cached, false)
		return
	}
	if resp.StatusCode != http.StatusOK {
		if cached != nil && shouldServeStaleManifest(resp.StatusCode) {
			writeContainerManifest(w, r.Method, cached, true)
			return
		}
		copyContainerManifestHeaders(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
		return
	}

	if r.Method == http.MethodHead {
		copyContainerManifestHeaders(w.Header(), resp.Header)
		w.WriteHeader(http.StatusOK)
		return
	}

	body, err := h.proxy.ReadMetadata(resp.Body)
	if err != nil {
		h.serveStaleManifestOrError(w, r, cached, fmt.Errorf("reading manifest: %w", err))
		return
	}
	manifest := &cachedContainerManifest{
		body:          body,
		contentType:   resp.Header.Get("Content-Type"),
		contentDigest: resp.Header.Get("Docker-Content-Digest"),
		etag:          resp.Header.Get("ETag"),
		size:          int64(len(body)),
		fetchedAt:     time.Now(),
	}
	if manifest.contentDigest == "" {
		manifest.contentDigest = sha256Digest(body)
	}
	if err := h.storeContainerManifest(r.Context(), cacheKey, manifest); err != nil {
		h.proxy.Logger.Warn("failed to cache container manifest", "error", err)
	}
	if manifest.contentDigest != reference && manifestDigestReferencePattern.MatchString(manifest.contentDigest) {
		digestKey := h.containerManifestCacheKey(name, manifest.contentDigest, accept)
		if err := h.storeContainerManifest(r.Context(), digestKey, manifest); err != nil {
			h.proxy.Logger.Warn("failed to cache container manifest by digest", "error", err)
		}
	}
	writeContainerManifest(w, r.Method, manifest, false)
}

func (h *ContainerHandler) serveStaleManifestOrError(w http.ResponseWriter, r *http.Request, cached *cachedContainerManifest, err error) {
	if cached != nil {
		h.proxy.Logger.Warn("upstream manifest fetch failed, serving stale cache", "error", err)
		writeContainerManifest(w, r.Method, cached, true)
		return
	}
	h.proxy.Logger.Error("failed to fetch manifest", "error", err)
	h.containerError(w, http.StatusBadGateway, "INTERNAL_ERROR", "failed to fetch from upstream")
}

func (h *ContainerHandler) containerManifestFresh(manifest *cachedContainerManifest) bool {
	return h.proxy.MetadataTTL > 0 && !manifest.fetchedAt.IsZero() && time.Since(manifest.fetchedAt) < h.proxy.MetadataTTL
}

func (h *ContainerHandler) containerManifestCacheKey(name, reference, accept string) string {
	identity := strings.Join([]string{h.registryURL, name, reference, accept}, "\x00")
	sum := sha256.Sum256([]byte(identity))
	return hex.EncodeToString(sum[:])
}

func (h *ContainerHandler) loadContainerManifest(ctx context.Context, cacheKey string) (*cachedContainerManifest, error) {
	if h.proxy.DB == nil || h.proxy.Storage == nil {
		return nil, nil
	}
	entry, err := h.proxy.DB.GetMetadataCache(containerManifestCacheEcosystem, cacheKey)
	if err != nil || entry == nil {
		return nil, err
	}
	reader, err := h.proxy.Storage.Open(ctx, entry.StoragePath)
	if err != nil {
		return nil, nil
	}
	defer func() { _ = reader.Close() }()
	body, err := h.proxy.ReadMetadata(reader)
	if err != nil {
		return nil, err
	}

	manifest := &cachedContainerManifest{body: body, size: int64(len(body))}
	if entry.ContentType.Valid {
		manifest.contentType = entry.ContentType.String
	}
	if entry.ContentDigest.Valid {
		manifest.contentDigest = entry.ContentDigest.String
	} else {
		manifest.contentDigest = sha256Digest(body)
	}
	if entry.ETag.Valid {
		manifest.etag = entry.ETag.String
	}
	if entry.Size.Valid {
		manifest.size = entry.Size.Int64
	}
	if entry.FetchedAt.Valid {
		manifest.fetchedAt = entry.FetchedAt.Time
	}
	return manifest, nil
}

func (h *ContainerHandler) storeContainerManifest(ctx context.Context, cacheKey string, manifest *cachedContainerManifest) error {
	if h.proxy.DB == nil || h.proxy.Storage == nil {
		return nil
	}
	storagePath := metadataStoragePath(containerManifestCacheEcosystem, cacheKey)
	size, _, err := h.proxy.Storage.Store(ctx, storagePath, bytes.NewReader(manifest.body))
	if err != nil {
		return fmt.Errorf("storing manifest: %w", err)
	}
	manifest.size = size
	return h.proxy.DB.UpsertMetadataCache(&database.MetadataCacheEntry{
		Ecosystem:     containerManifestCacheEcosystem,
		Name:          cacheKey,
		StoragePath:   storagePath,
		ETag:          sql.NullString{String: manifest.etag, Valid: manifest.etag != ""},
		ContentType:   sql.NullString{String: manifest.contentType, Valid: manifest.contentType != ""},
		ContentDigest: sql.NullString{String: manifest.contentDigest, Valid: manifest.contentDigest != ""},
		Size:          sql.NullInt64{Int64: size, Valid: true},
		FetchedAt:     sql.NullTime{Time: manifest.fetchedAt, Valid: !manifest.fetchedAt.IsZero()},
	})
}

func writeContainerManifest(w http.ResponseWriter, method string, manifest *cachedContainerManifest, stale bool) {
	if manifest.contentType != "" {
		w.Header().Set("Content-Type", manifest.contentType)
	}
	w.Header().Set("Content-Length", strconv.FormatInt(manifest.size, 10))
	if manifest.contentDigest != "" {
		w.Header().Set("Docker-Content-Digest", manifest.contentDigest)
	}
	if manifest.etag != "" {
		w.Header().Set("ETag", manifest.etag)
	}
	if stale {
		w.Header().Set("Warning", containerStaleWarning)
	}
	w.WriteHeader(http.StatusOK)
	if method != http.MethodHead {
		_, _ = w.Write(manifest.body)
	}
}

func containerManifestAccept(r *http.Request) string {
	if accept := r.Header.Get("Accept"); accept != "" {
		return accept
	}
	return strings.Join([]string{
		"application/vnd.oci.image.manifest.v1+json",
		"application/vnd.oci.image.index.v1+json",
		"application/vnd.docker.distribution.manifest.v2+json",
		"application/vnd.docker.distribution.manifest.list.v2+json",
		"application/vnd.docker.distribution.manifest.v1+prettyjws",
	}, ", ")
}

func copyContainerManifestHeaders(destination, source http.Header) {
	for _, header := range []string{"Content-Type", "Content-Length", "Docker-Content-Digest", "ETag", "WWW-Authenticate"} {
		if value := source.Get(header); value != "" {
			destination.Set(header, value)
		}
	}
}

func shouldServeStaleManifest(status int) bool {
	return status == http.StatusTooManyRequests || status >= http.StatusInternalServerError
}

func sha256Digest(body []byte) string {
	digest := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(digest[:])
}
