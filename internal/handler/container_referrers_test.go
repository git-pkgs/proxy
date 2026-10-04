package handler

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const testReferrersIndex = `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[` +
	`{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"sha256:1111111111111111111111111111111111111111111111111111111111111111","size":10,"artifactType":"application/spdx+json"},` +
	`{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"sha256:2222222222222222222222222222222222222222222222222222222222222222","size":10,"artifactType":"application/vnd.cncf.notary.signature"}]}`

var testReferrersSubject = "sha256:" + sha256Hex("subject manifest")

func newReferrersTestHandler(t *testing.T, upstream *httptest.Server) (*ContainerHandler, *Proxy) {
	t.Helper()
	proxy, _, _, _ := setupTestProxy(t)
	proxy.HTTPClient = upstream.Client()
	return &ContainerHandler{proxy: proxy, registryURL: upstream.URL, proxyURL: "http://proxy.example.test"}, proxy
}

func serveReferrersRequest(h *ContainerHandler, method, target string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	h.Routes().ServeHTTP(recorder, httptest.NewRequest(method, target, nil))
	return recorder
}

func TestContainerHandler_ReferrersCachesIndexForEveryFilter(t *testing.T) {
	upstreamAvailable := true
	upstreamRequests := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamRequests++
		if r.URL.Path != "/v2/library/nginx/referrers/"+testReferrersSubject {
			http.NotFound(w, r)
			return
		}
		if r.URL.RawQuery != "" {
			t.Errorf("upstream query = %q, want artifactType stripped", r.URL.RawQuery)
		}
		if got := r.Header.Get("Accept"); got != containerReferrersMediaType {
			t.Errorf("upstream Accept = %q, want %q", got, containerReferrersMediaType)
		}
		if !upstreamAvailable {
			http.Error(w, "upstream unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", containerReferrersMediaType)
		w.Header().Set("OCI-Filters-Applied", "artifactType")
		_, _ = io.WriteString(w, testReferrersIndex)
	}))
	defer upstream.Close()

	h, proxy := newReferrersTestHandler(t, upstream)
	proxy.MetadataTTL = time.Hour
	base := "/library/nginx/referrers/" + testReferrersSubject

	first := serveReferrersRequest(h, http.MethodGet, base+"?artifactType=application/spdx%2Bjson")
	if first.Code != http.StatusOK {
		t.Fatalf("first status = %d, want 200: %s", first.Code, first.Body.String())
	}
	if first.Body.String() != testReferrersIndex {
		t.Errorf("first body = %q, want upstream index", first.Body.String())
	}
	if got := first.Header().Get("Content-Type"); got != containerReferrersMediaType {
		t.Errorf("first Content-Type = %q, want %q", got, containerReferrersMediaType)
	}
	if got := first.Header().Get("OCI-Filters-Applied"); got != "" {
		t.Errorf("OCI-Filters-Applied = %q, want absent so clients filter", got)
	}

	other := serveReferrersRequest(h, http.MethodGet, base+"?artifactType=application/vnd.cncf.notary.signature")
	if other.Code != http.StatusOK || other.Body.String() != testReferrersIndex {
		t.Fatalf("other filter = %d %q, want cached full index", other.Code, other.Body.String())
	}
	if upstreamRequests != 1 {
		t.Fatalf("upstream requests after fresh hit = %d, want 1", upstreamRequests)
	}

	proxy.MetadataTTL = 0
	upstreamAvailable = false
	stale := serveReferrersRequest(h, http.MethodGet, base)
	if stale.Code != http.StatusOK || stale.Body.String() != testReferrersIndex {
		t.Fatalf("stale = %d %q, want cached index", stale.Code, stale.Body.String())
	}
	if got := stale.Header().Get("Content-Type"); got != containerReferrersMediaType {
		t.Errorf("stale Content-Type = %q, want %q", got, containerReferrersMediaType)
	}
	if got := stale.Header().Get("Warning"); got != containerStaleWarning {
		t.Errorf("stale Warning = %q, want stale warning", got)
	}
	if upstreamRequests != 2 {
		t.Errorf("upstream requests after stale fallback = %d, want 2", upstreamRequests)
	}
}

func TestContainerHandler_ReferrersRevalidatesWithETag(t *testing.T) {
	upstreamRequests := 0
	notModified := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamRequests++
		if r.Header.Get("If-None-Match") == `"referrers-etag"` {
			notModified++
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("Content-Type", containerReferrersMediaType)
		w.Header().Set("ETag", `"referrers-etag"`)
		_, _ = io.WriteString(w, testReferrersIndex)
	}))
	defer upstream.Close()

	h, proxy := newReferrersTestHandler(t, upstream)
	proxy.MetadataTTL = time.Hour
	target := "/library/nginx/referrers/" + testReferrersSubject

	if first := serveReferrersRequest(h, http.MethodGet, target); first.Code != http.StatusOK {
		t.Fatalf("first status = %d, want 200", first.Code)
	}
	// Age the row past the TTL so the next request has to revalidate.
	cacheKey := h.containerReferrersCacheKey(upstream.URL, "library/nginx", testReferrersSubject, "")
	entry, err := proxy.DB.GetMetadataCache(containerReferrersCacheEcosystem, cacheKey)
	if err != nil || entry == nil {
		t.Fatalf("cached row = %v, %v, want one", entry, err)
	}
	entry.FetchedAt.Time = time.Now().Add(-2 * time.Hour)
	if err := proxy.DB.UpsertMetadataCache(entry); err != nil {
		t.Fatalf("ageing cached row: %v", err)
	}

	second := serveReferrersRequest(h, http.MethodGet, target)
	if second.Code != http.StatusOK || second.Body.String() != testReferrersIndex {
		t.Fatalf("revalidated = %d %q, want cached index", second.Code, second.Body.String())
	}
	if got := second.Header().Get("Warning"); got != "" {
		t.Errorf("Warning = %q, want none after 304", got)
	}

	// The 304 starts the TTL again, so this one stays in the cache.
	if third := serveReferrersRequest(h, http.MethodGet, target); third.Code != http.StatusOK {
		t.Fatalf("third status = %d, want 200", third.Code)
	}
	if upstreamRequests != 2 || notModified != 1 {
		t.Errorf("upstream requests = %d with %d answered 304, want 2 with 1", upstreamRequests, notModified)
	}
}

func TestContainerHandler_ReferrersDefaultsMissingContentType(t *testing.T) {
	upstreamRequests := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamRequests++
		// A nil value stops net/http from sniffing a Content-Type.
		w.Header()["Content-Type"] = nil
		_, _ = io.WriteString(w, testReferrersIndex)
	}))
	defer upstream.Close()

	h, proxy := newReferrersTestHandler(t, upstream)
	proxy.MetadataTTL = time.Hour
	target := "/library/nginx/referrers/" + testReferrersSubject

	// oras-go only accepts the exact image index type, both fresh and cached.
	for _, label := range []string{"fetched", "cached"} {
		got := serveReferrersRequest(h, http.MethodGet, target)
		if got.Code != http.StatusOK {
			t.Fatalf("%s status = %d, want 200", label, got.Code)
		}
		if ct := got.Header().Get("Content-Type"); ct != containerReferrersMediaType {
			t.Errorf("%s Content-Type = %q, want %q", label, ct, containerReferrersMediaType)
		}
	}
	if upstreamRequests != 1 {
		t.Errorf("upstream requests = %d, want 1", upstreamRequests)
	}
}

func TestContainerHandler_ReferrersRelaysUpstreamErrorsWithoutCaching(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		header    string
		value     string
		wantError string
	}{
		{name: "no referrers API", status: http.StatusNotFound, header: "Content-Type", value: "application/json", wantError: "NOT_FOUND"},
		{name: "repository unknown", status: http.StatusNotFound, header: "Content-Type", value: "application/json", wantError: "NAME_UNKNOWN"},
		{name: "auth challenge", status: http.StatusUnauthorized, header: "WWW-Authenticate", value: `Bearer realm="https://auth.example.test/token"`, wantError: "UNAUTHORIZED"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			upstreamRequests := 0
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				upstreamRequests++
				w.Header().Set(tt.header, tt.value)
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, `{"errors":[{"code":"`+tt.wantError+`"}]}`)
			}))
			defer upstream.Close()

			h, proxy := newReferrersTestHandler(t, upstream)
			proxy.MetadataTTL = time.Hour
			target := "/library/nginx/referrers/" + testReferrersSubject
			for range 2 {
				got := serveReferrersRequest(h, http.MethodGet, target)
				if got.Code != tt.status {
					t.Fatalf("status = %d, want %d", got.Code, tt.status)
				}
				if !strings.Contains(got.Body.String(), tt.wantError) {
					t.Errorf("body = %q, want upstream error %s", got.Body.String(), tt.wantError)
				}
				if got.Header().Get(tt.header) != tt.value {
					t.Errorf("%s = %q, want %q", tt.header, got.Header().Get(tt.header), tt.value)
				}
			}
			if upstreamRequests != 2 {
				t.Errorf("upstream requests = %d, want 2 (error must not be cached)", upstreamRequests)
			}
		})
	}
}

func TestContainerHandler_ReferrersFallsBackToTagSchemaWithoutCache(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{name: "upstream 503", handler: func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
		}},
		{name: "upstream 429", handler: func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "slow down", http.StatusTooManyRequests)
		}},
		{name: "upstream not JSON", handler: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			_, _ = io.WriteString(w, "<html>captive portal</html>")
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			upstreamRequests := 0
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				upstreamRequests++
				tt.handler(w, r)
			}))
			defer upstream.Close()

			h, proxy := newReferrersTestHandler(t, upstream)
			proxy.MetadataTTL = time.Hour
			target := "/library/nginx/referrers/" + testReferrersSubject
			for range 2 {
				assertReferrersFallback(t, serveReferrersRequest(h, http.MethodGet, target))
			}
			if upstreamRequests != 2 {
				t.Errorf("upstream requests = %d, want 2 (failure must not be cached)", upstreamRequests)
			}
		})
	}

	t.Run("upstream unreachable", func(t *testing.T) {
		upstream := httptest.NewServer(http.NotFoundHandler())
		h, _ := newReferrersTestHandler(t, upstream)
		upstream.Close()
		assertReferrersFallback(t, serveReferrersRequest(h, http.MethodGet, "/library/nginx/referrers/"+testReferrersSubject))
	})
}

func assertReferrersFallback(t *testing.T, got *httptest.ResponseRecorder) {
	t.Helper()
	if got.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 so clients use the tag schema: %s", got.Code, got.Body.String())
	}
	var body struct {
		Errors []struct {
			Code string `json:"code"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(got.Body.Bytes(), &body); err != nil || len(body.Errors) != 1 {
		t.Fatalf("body = %q, want one OCI error", got.Body.String())
	}
	if body.Errors[0].Code == "NAME_UNKNOWN" {
		t.Errorf("error code = NAME_UNKNOWN, which stops clients from falling back")
	}
}

func TestContainerHandler_ReferrersNamedRegistryRewritesLink(t *testing.T) {
	upstreamRequests := 0
	var upstream *httptest.Server
	upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamRequests++
		if r.URL.Path != "/v2/owner/img/referrers/"+testReferrersSubject {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", containerReferrersMediaType)
		switch r.URL.Query().Get("last") {
		case "":
			w.Header().Add("Link", `</v2/owner/img/referrers/`+testReferrersSubject+`?last=abc>; rel="next"`)
			w.Header().Add("Link", `<https://elsewhere.example.test/docs>; rel="help"`)
		case "abc":
			w.Header().Set("Link", `<`+upstream.URL+`/v2/owner/img/referrers/`+testReferrersSubject+`?last=def>; rel="next"`)
		}
		_, _ = io.WriteString(w, testReferrersIndex)
	}))
	defer upstream.Close()

	proxy, _, _, _ := setupTestProxy(t)
	proxy.HTTPClient = upstream.Client()
	proxy.MetadataTTL = time.Hour
	// The default registry is the same upstream, so upstream/test/owner/img
	// and owner/img share one cache row.
	h := NewContainerHandlerWithRegistry(proxy, "http://proxy.example.test", upstream.URL, map[string]string{"test": upstream.URL})
	routes := http.StripPrefix("/v2", h.Routes())

	first := httptest.NewRecorder()
	routes.ServeHTTP(first, httptest.NewRequest(http.MethodGet,
		"/v2/upstream/test/owner/img/referrers/"+testReferrersSubject+"?artifactType=application/spdx%2Bjson", nil))
	if first.Code != http.StatusOK {
		t.Fatalf("first page status = %d, want 200: %s", first.Code, first.Body.String())
	}
	wantLink := `<http://proxy.example.test/v2/upstream/test/owner/img/referrers/` + testReferrersSubject +
		`?last=abc>; rel="next", <https://elsewhere.example.test/docs>; rel="help"`
	if got := first.Header().Get("Link"); got != wantLink {
		t.Fatalf("Link = %q, want %q", got, wantLink)
	}

	shared := httptest.NewRecorder()
	routes.ServeHTTP(shared, httptest.NewRequest(http.MethodGet, "/v2/owner/img/referrers/"+testReferrersSubject, nil))
	if shared.Code != http.StatusOK {
		t.Fatalf("shared row status = %d, want 200: %s", shared.Code, shared.Body.String())
	}
	wantSharedLink := `<http://proxy.example.test/v2/owner/img/referrers/` + testReferrersSubject +
		`?last=abc>; rel="next", <https://elsewhere.example.test/docs>; rel="help"`
	if got := shared.Header().Get("Link"); got != wantSharedLink {
		t.Errorf("shared row Link = %q, want it rewritten for the second path %q", got, wantSharedLink)
	}
	if upstreamRequests != 1 {
		t.Fatalf("upstream requests after shared row hit = %d, want 1", upstreamRequests)
	}

	nextURL := strings.TrimPrefix(strings.SplitN(first.Header().Get("Link"), ">", 2)[0], "<")
	next := httptest.NewRecorder()
	routes.ServeHTTP(next, httptest.NewRequest(http.MethodGet, nextURL, nil))
	if next.Code != http.StatusOK {
		t.Fatalf("next page status = %d, want 200: %s", next.Code, next.Body.String())
	}
	wantNextLink := `<http://proxy.example.test/v2/upstream/test/owner/img/referrers/` + testReferrersSubject + `?last=def>; rel="next"`
	if got := next.Header().Get("Link"); got != wantNextLink {
		t.Errorf("next page Link = %q, want absolute upstream link rewritten to %q", got, wantNextLink)
	}
	if upstreamRequests != 2 {
		t.Errorf("upstream requests = %d, want 2 (pages are separate rows)", upstreamRequests)
	}
}

func TestContainerHandler_ReferrersRejectsInvalidRequests(t *testing.T) {
	upstreamRequests := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamRequests++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer upstream.Close()

	proxy, _, _, _ := setupTestProxy(t)
	proxy.HTTPClient = upstream.Client()
	h := NewContainerHandlerWithRegistry(proxy, "http://proxy.example.test", upstream.URL)

	tests := []struct {
		name       string
		method     string
		path       string
		wantStatus int
		wantCode   string
	}{
		{name: "post", method: http.MethodPost, path: "/library/nginx/referrers/" + testReferrersSubject, wantStatus: http.StatusMethodNotAllowed},
		{name: "short digest", method: http.MethodGet, path: "/library/nginx/referrers/sha256:abc", wantStatus: http.StatusBadRequest, wantCode: "DIGEST_INVALID"},
		{name: "trailing characters", method: http.MethodGet, path: "/library/nginx/referrers/" + testReferrersSubject + "x", wantStatus: http.StatusBadRequest, wantCode: "DIGEST_INVALID"},
		{name: "leading characters", method: http.MethodGet, path: "/library/nginx/referrers/x" + testReferrersSubject, wantStatus: http.StatusBadRequest, wantCode: "DIGEST_INVALID"},
		{name: "unknown algorithm", method: http.MethodGet, path: "/library/nginx/referrers/md5:" + strings.Repeat("a", 32), wantStatus: http.StatusBadRequest, wantCode: "DIGEST_INVALID"},
		{name: "unknown named upstream", method: http.MethodGet, path: "/upstream/missing/owner/img/referrers/" + testReferrersSubject, wantStatus: http.StatusNotFound, wantCode: "NAME_UNKNOWN"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := serveReferrersRequest(h, tt.method, tt.path)
			if got.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d: %s", got.Code, tt.wantStatus, got.Body.String())
			}
			if tt.wantCode != "" && !strings.Contains(got.Body.String(), tt.wantCode) {
				t.Errorf("body = %q, want %s", got.Body.String(), tt.wantCode)
			}
		})
	}
	if upstreamRequests != 0 {
		t.Errorf("upstream requests = %d, want 0", upstreamRequests)
	}
}

func TestContainerHandler_ReferrersRouteKeepsManifestPaths(t *testing.T) {
	tagSchema := "sha256-" + strings.TrimPrefix(testReferrersSubject, "sha256:")
	var upstreamPaths []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamPaths = append(upstreamPaths, r.URL.Path)
		// Both handlers build the same upstream path here, so tell them apart
		// by Accept: only the referrers handler asks for the index type alone.
		if r.Header.Get("Accept") == containerReferrersMediaType {
			http.Error(w, "reached the referrers handler", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", containerReferrersMediaType)
		_, _ = io.WriteString(w, testReferrersIndex)
	}))
	defer upstream.Close()

	h, _ := newReferrersTestHandler(t, upstream)
	for _, path := range []string{
		// A repository may contain a "referrers" component; manifest paths
		// must keep reaching the manifest handler.
		"/foo/manifests/referrers/" + testReferrersSubject,
		// Clients without a referrers API read the tag schema as a manifest.
		"/library/nginx/manifests/" + tagSchema,
	} {
		if got := serveReferrersRequest(h, http.MethodGet, path); got.Code != http.StatusOK {
			t.Fatalf("GET %s status = %d, want 200: %s", path, got.Code, got.Body.String())
		}
	}
	want := []string{
		"/v2/foo/manifests/referrers/" + testReferrersSubject,
		"/v2/library/nginx/manifests/" + tagSchema,
	}
	if strings.Join(upstreamPaths, "\n") != strings.Join(want, "\n") {
		t.Errorf("upstream paths = %q, want %q", upstreamPaths, want)
	}
}
