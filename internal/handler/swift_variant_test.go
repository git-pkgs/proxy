package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The Accept header and query a client sends go upstream, so the answer to
// one request is not the cached answer to a request that sent others.
func TestSwiftMetadataIsCachedPerAcceptAndQuery(t *testing.T) {
	type variant struct{ accept, query string }
	for _, tt := range []struct {
		name, path string
		// other is the request of another client, before the plain one.
		other variant
	}{
		{"release, other Accept", "/apple/example/1.2.3", variant{"application/vnd.swift.registry.v2+json", ""}},
		{"release, query", "/apple/example/1.2.3", variant{"", "x=1"}},
		{"releases, other Accept", "/apple/example", variant{"application/vnd.swift.registry.v2+json", ""}},
		{"manifest, other Accept", "/apple/example/1.2.3/Package.swift", variant{"text/html", ""}},
		{"identifiers, other Accept", "/identifiers?url=https://github.com/apple/example", variant{"application/vnd.swift.registry.v2+json", ""}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			requests := 0
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"releases": map[string]any{}, "accept": r.Header.Get("Accept"), "query": r.URL.RawQuery,
				})
			}))
			defer upstream.Close()
			proxy, _, _, _ := setupTestProxy(t)
			proxy.CacheMetadata = true
			proxy.MetadataTTL = time.Hour
			handler := NewSwiftHandler(proxy, "https://proxy.example", upstream.URL).Routes()

			get := func(v variant) (accept, query string) {
				t.Helper()
				target := tt.path
				if v.query != "" {
					target += "?" + v.query
				}
				req := httptest.NewRequest(http.MethodGet, target, nil)
				if v.accept != "" {
					req.Header.Set("Accept", v.accept)
				}
				w := httptest.NewRecorder()
				handler.ServeHTTP(w, req)
				if w.Code != http.StatusOK {
					t.Fatalf("GET %s: %d %s", target, w.Code, w.Body)
				}
				var body struct{ Accept, Query string }
				if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
					t.Fatalf("GET %s: %v in %s", target, err, w.Body)
				}
				return body.Accept, body.Query
			}

			otherAccept, otherQuery := get(tt.other)
			accept, query := get(variant{})
			if tt.other.accept != "" && (otherAccept != tt.other.accept || accept == tt.other.accept) {
				t.Errorf("Accept answered: other %q, plain %q; want %q for the other only", otherAccept, accept, tt.other.accept)
			}
			if tt.other.query != "" && (!strings.Contains(otherQuery, tt.other.query) || strings.Contains(query, tt.other.query)) {
				t.Errorf("query answered: other %q, plain %q; want %q for the other only", otherQuery, query, tt.other.query)
			}

			// Each is cached, under its own key.
			get(tt.other)
			get(variant{})
			if requests != 2 {
				t.Errorf("upstream requests = %d, want 2", requests)
			}
		})
	}
}

// A request with the default Accept header reads the keys documents were
// cached under before the header was part of them, and the release metadata
// the proxy reads for a source archive.
func TestSwiftDefaultAcceptKeepsItsCacheKeys(t *testing.T) {
	for name, keys := range map[string][2]string{
		"release": {
			swiftReleaseVariantCacheKey("apple", "example", "1.0.0", "", swiftAcceptJSON),
			swiftReleaseCacheKey("apple", "example", "1.0.0"),
		},
		"releases": {
			swiftReleasesCacheKey("apple", "example", "page=2", swiftAcceptJSON),
			swiftMetadataCacheKey("releases", "apple", "example", "page=2"),
		},
		"manifest": {
			swiftManifestCacheKey("apple", "example", "1.0.0", "swift-version=5.8", swiftAcceptManifest),
			swiftMetadataCacheKey("manifest", "apple", "example", "1.0.0", "swift-version=5.8"),
		},
	} {
		if keys[0] != keys[1] {
			t.Errorf("%s key with the default Accept = %s, want %s", name, keys[0], keys[1])
		}
	}
	// The default of one document is not the default of another.
	if swiftManifestCacheKey("apple", "example", "1.0.0", "", swiftAcceptJSON) == swiftManifestCacheKey("apple", "example", "1.0.0", "", swiftAcceptManifest) {
		t.Error("a manifest asked for as JSON shares the key of the manifest")
	}
}
