package handler

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const (
	// defaultGoVulnDBURL is the database govulncheck uses without -db.
	defaultGoVulnDBURL = "https://vuln.go.dev"

	// goVulnDBMaxIDLength bounds an entry ID in a request path. Go IDs look
	// like GO-2024-1234; other OSV databases use longer ones such as
	// GHSA-xxxx-xxxx-xxxx.
	goVulnDBMaxIDLength = 128

	// goVulnDBMaxJSONSize bounds a decompressed .json.gz response while it is
	// checked. index/modules.json, the largest file, is a few MB.
	goVulnDBMaxJSONSize = 256 << 20
)

// SetVulnDB sets the Go vulnerability database served at /vulndb/. An empty
// URL keeps the default, vuln.go.dev.
func (h *GoHandler) SetVulnDB(vulnDBURL string) {
	h.vulnDBURL = configuredUpstreamURL(vulnDBURL, defaultGoVulnDBURL)
}

// handleVulnDB serves the Go vulnerability database at
// /vulndb/{path}. Only the files the database API defines are passed on:
// index/db.json, index/modules.json, index/vulns.json and ID/{id}.json, each
// also as .json.gz, which is what govulncheck fetches. Every file can change,
// so all of them follow the metadata TTL and fall back to the cached copy
// when the database cannot be reached. The bytes are stored and served
// unchanged: a .json.gz file is gzip data, not a gzip-encoded response.
func (h *GoHandler) handleVulnDB(w http.ResponseWriter, r *http.Request, path string) {
	if !validGoVulnDBPath(path) {
		http.NotFound(w, r)
		return
	}
	upstreamURL := h.vulnDBURL + "/" + path
	p := h.proxy
	if !p.CacheMetadata {
		p.proxyMetadataStream(w, r, upstreamURL, "identity", "*/*")
		return
	}
	cacheKey := goVulnDBCacheKey(h.vulnDBURL, path)
	res := p.lookupMetadata(r.Context(), goEcosystem, cacheKey, upstreamURL, "*/*", "identity", goVulnDBResponseCheck(path))
	if res.err != nil {
		p.writeMetadataFetchError(w, res.err)
		return
	}
	p.writeMetadataCachedResponseWithEncoding(w, r, goEcosystem, cacheKey, res.body, res.contentType, res.contentEncoding)
}

// validGoVulnDBPath reports whether path is a file the vulnerability
// database API defines. Entry IDs are limited to letters, digits, '-' and
// '_', which keeps '.', '/', '%' and the like out of the upstream URL.
func validGoVulnDBPath(path string) bool {
	name, ok := strings.CutSuffix(path, ".json.gz")
	if !ok {
		name, ok = strings.CutSuffix(path, ".json")
	}
	if !ok {
		return false
	}
	switch name {
	case "index/db", "index/modules", "index/vulns":
		return true
	}
	id, ok := strings.CutPrefix(name, "ID/")
	if !ok || id == "" || len(id) > goVulnDBMaxIDLength {
		return false
	}
	for _, c := range id {
		if !isASCIILetterOrDigit(c) && c != '-' && c != '_' {
			return false
		}
	}
	return true
}

func isASCIILetterOrDigit(c rune) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// goVulnDBResponseCheck returns a check that a response to path is JSON,
// after decompressing it for a .json.gz path. A 200 that is something else
// (an egress proxy's block page, a cut-off download) is refused rather than
// cached, where it would also replace the copy kept for an outage.
func goVulnDBResponseCheck(path string) func([]byte) error {
	compressed := strings.HasSuffix(path, ".gz")
	return func(body []byte) error {
		data := body
		if compressed {
			zr, err := gzip.NewReader(bytes.NewReader(body))
			if err != nil {
				return fmt.Errorf("vulnerability database file %s is not gzip data: %w", path, err)
			}
			data, err = io.ReadAll(io.LimitReader(zr, goVulnDBMaxJSONSize+1))
			if err == nil && len(data) > goVulnDBMaxJSONSize {
				err = errors.New("too large")
			}
			if err != nil {
				return fmt.Errorf("vulnerability database file %s does not decompress: %w", path, err)
			}
		}
		if !json.Valid(data) {
			return fmt.Errorf("vulnerability database file %s is not JSON", path)
		}
		return nil
	}
}

// goVulnDBCacheKey hashes the database URL and the request path, as
// goSumDBCacheKey does, so repointing upstream.go_vulndb does not serve the
// old database's entries.
func goVulnDBCacheKey(vulnDBURL, path string) string {
	digest := sha256.Sum256([]byte(vulnDBURL + "\x00" + path))
	return "vulndb/" + hex.EncodeToString(digest[:])
}
