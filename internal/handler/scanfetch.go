package handler

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// scanFetchURL builds a short-lived, HMAC-signed URL for the internal
// /_internal/scan-fetch route, so an external scanner can pull the exact
// bytes staged at path without going through cooldown or the scan hook
// itself. This is generated the same way for every storage backend: it
// never depends on Storage.SignedURL, which not every backend implements.
func (p *Proxy) scanFetchURL(path string, ttl time.Duration) string {
	exp := time.Now().Add(ttl).Unix()
	return fmt.Sprintf("%s/_internal/scan-fetch?path=%s&exp=%d&sig=%s",
		p.ScanFetchBaseURL, url.QueryEscape(path), exp, hmacHex(p.ScanSigningKey, path, exp))
}

func hmacHex(key []byte, path string, exp int64) string {
	mac := hmac.New(sha256.New, key)
	_, _ = fmt.Fprintf(mac, "%s|%d", path, exp)
	return hex.EncodeToString(mac.Sum(nil))
}

// ServeScanFetch streams a storage object to a caller presenting a valid
// short-lived HMAC token, so external scanners can pull a staged artifact
// without going through cooldown or the scan hook themselves. This handler
// never calls GetOrFetchArtifact/fetchAndCache/storeArtifact — the
// separation from the normal request path is structural, not a
// conditional bypass flag.
//
// This route exists only for scanners configured under ScanningConfig; the
// URL is minted by scanFetchURL and passed as fetch_url in the scan notify
// request. It is not part of the public API and should be restricted to
// internal-network access at the ingress/network-policy layer — the HMAC
// scoping (one object, short TTL) limits what a leaked token can do, but
// isn't a substitute for network restriction.
//
// @Summary Fetch a staged artifact for scanning
// @Description Streams the exact bytes staged in storage for a pre-cache security scan.
// @Description Requires a short-lived HMAC-signed token minted by the proxy itself and
// @Description delivered via the fetch_url field of the scan notify request (see the
// @Description Artifact Scanning section of docs/configuration.md). Not part of the
// @Description public API; restrict access to the scanner network at the ingress layer.
// @Tags scanning
// @Produce application/octet-stream
// @Param path query string true "Storage path of the staged artifact"
// @Param exp query int true "Token expiry, Unix seconds"
// @Param sig query string true "HMAC-SHA256 signature over the string path|exp"
// @Success 200 {file} file
// @Failure 403 {string} string "invalid, expired, or tampered token"
// @Failure 404 {string} string "object not found in storage, or scanning is not configured"
// @Router /_internal/scan-fetch [get]
func (p *Proxy) ServeScanFetch(w http.ResponseWriter, r *http.Request) {
	if p.Scanners == nil || !p.Scanners.Enabled() || len(p.ScanSigningKey) == 0 {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	path := r.URL.Query().Get("path")
	exp, err := strconv.ParseInt(r.URL.Query().Get("exp"), 10, 64)
	if err != nil || containsPathTraversal(path) || time.Now().Unix() > exp {
		http.Error(w, "invalid or expired token", http.StatusForbidden)
		return
	}

	want := hmacHex(p.ScanSigningKey, path, exp)
	if !hmac.Equal([]byte(r.URL.Query().Get("sig")), []byte(want)) {
		http.Error(w, "invalid signature", http.StatusForbidden)
		return
	}

	reader, err := p.Storage.Open(r.Context(), path)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	defer func() { _ = reader.Close() }()

	w.Header().Set("Content-Type", "application/octet-stream")
	_, _ = io.Copy(w, reader)
}
