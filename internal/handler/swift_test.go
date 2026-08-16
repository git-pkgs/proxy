package handler

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/git-pkgs/proxy/internal/packageurl"
	"github.com/git-pkgs/registries/fetch"
)

func TestSwiftPackageReleasesRewritesRegistryURLs(t *testing.T) {
	var gotAccept string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/registry/apple/swift-argument-parser" {
			t.Errorf("upstream path = %q", r.URL.Path)
		}
		gotAccept = r.Header.Get("Accept")
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Content-Version", "1")
		w.Header().Add("Link", `</registry/apple/swift-argument-parser?page=2>; rel="next"`)
		_, _ = io.WriteString(w, `{"releases":{"1.2.0":{"url":"/registry/apple/swift-argument-parser/1.2.0"},"1.1.0":{}}}`)
	}))
	defer upstream.Close()

	proxy, _, _, _ := setupTestProxy(t)
	handler := NewSwiftHandler(proxy, "https://proxy.example", upstream.URL+"/registry").Routes()
	req := httptest.NewRequest(http.MethodGet, "/APPLE/SWIFT-ARGUMENT-PARSER", nil)
	req.Header.Set("Accept", swiftAcceptJSON)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	if gotAccept != swiftAcceptJSON {
		t.Errorf("upstream Accept = %q, want %q", gotAccept, swiftAcceptJSON)
	}
	if got := w.Header().Get("Content-Version"); got != "1" {
		t.Errorf("Content-Version = %q, want 1", got)
	}
	if got := w.Header().Get("Link"); got != `<https://proxy.example/swift/apple/swift-argument-parser?page=2>; rel="next"` {
		t.Errorf("Link = %q", got)
	}

	var body struct {
		Releases map[string]struct {
			URL string `json:"url"`
		} `json:"releases"`
	}
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if got := body.Releases["1.2.0"].URL; got != "https://proxy.example/swift/apple/swift-argument-parser/1.2.0" {
		t.Errorf("release URL = %q", got)
	}
	if got := body.Releases["1.1.0"].URL; got != "" {
		t.Errorf("release without upstream URL gained URL %q", got)
	}
}

func TestSwiftReleaseMetadataSupportsJSONExtensionAndHead(t *testing.T) {
	var requestMethods []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestMethods = append(requestMethods, r.Method)
		if r.URL.Path != "/registry/apple/example/1.2.3" {
			t.Errorf("upstream path = %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"apple.example","version":"1.2.3","resources":[]}`)
	}))
	defer upstream.Close()

	proxy, _, _, _ := setupTestProxy(t)
	handler := NewSwiftHandler(proxy, "https://proxy.example", upstream.URL+"/registry").Routes()

	for _, method := range []string{http.MethodGet, http.MethodHead} {
		req := httptest.NewRequest(method, "/APPLE/EXAMPLE/1.2.3.json", nil)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("%s status = %d, want 200", method, w.Code)
		}
		if method == http.MethodHead && w.Body.Len() != 0 {
			t.Errorf("HEAD response body length = %d, want 0", w.Body.Len())
		}
	}
	if len(requestMethods) != 2 || requestMethods[0] != http.MethodGet || requestMethods[1] != http.MethodGet {
		t.Errorf("upstream methods = %v, want metadata GETs", requestMethods)
	}
}

func TestSwiftManifestProxiesQueryAndRewritesLinks(t *testing.T) {
	var upstream *httptest.Server
	var gotAccept string
	upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("upstream method = %s, want GET", r.Method)
		}
		if r.URL.Path != "/registry/apple/example/1.2.3/Package.swift" {
			t.Errorf("upstream path = %q", r.URL.Path)
		}
		if got := r.URL.Query().Get("swift-version"); got != "5.9" {
			t.Errorf("swift-version = %q, want 5.9", got)
		}
		gotAccept = r.Header.Get("Accept")
		w.Header().Set("Content-Type", "text/x-swift")
		w.Header().Add("Link", fmt.Sprintf(`<%s/registry/apple/example/1.2.3/Package.swift?swift-version=5.8>; rel="alternate"; filename="Package@swift-5.8.swift"`, upstream.URL))
		w.Header().Add("Link", `<https://github.com/apple/example>; rel="canonical"`)
		_, _ = io.WriteString(w, "// swift-tools-version: 5.9\n")
	}))
	defer upstream.Close()

	proxy, _, _, _ := setupTestProxy(t)
	handler := NewSwiftHandler(proxy, "https://proxy.example", upstream.URL+"/registry").Routes()
	req := httptest.NewRequest(http.MethodGet, "/APPLE/EXAMPLE/1.2.3/Package.swift?swift-version=5.9", nil)
	req.Header.Set("Accept", swiftAcceptManifest)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if gotAccept != swiftAcceptManifest {
		t.Errorf("upstream Accept = %q, want %q", gotAccept, swiftAcceptManifest)
	}
	links := strings.Join(w.Header().Values("Link"), ",")
	if !strings.Contains(links, "https://proxy.example/swift/apple/example/1.2.3/Package.swift?swift-version=5.8") {
		t.Errorf("internal manifest Link was not rewritten: %q", links)
	}
	if !strings.Contains(links, "https://github.com/apple/example") {
		t.Errorf("external canonical Link was changed: %q", links)
	}
	if got := w.Header().Get("Content-Version"); got != "1" {
		t.Errorf("Content-Version = %q, want 1", got)
	}
}

func TestSwiftSourceArchiveCachesAndPreservesSecurityMetadata(t *testing.T) {
	archive := []byte("swift source archive")
	checksumBytes := sha256.Sum256(archive)
	checksum := hex.EncodeToString(checksumBytes[:])
	signature := base64.StdEncoding.EncodeToString([]byte("signature"))

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/registry/apple/example/1.2.3" {
			t.Errorf("metadata path = %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":"apple.example","version":"1.2.3","resources":[{"name":"source-archive","type":"application/zip","checksum":%q,"signing":{"signatureBase64Encoded":%q,"signatureFormat":"cms-1.0.0"}}]}`, checksum, signature)
	}))
	defer upstream.Close()

	proxy, db, _, fetcher := setupTestProxy(t)
	fetcher.artifact = &fetch.Artifact{
		Body:        io.NopCloser(strings.NewReader(string(archive))),
		Size:        int64(len(archive)),
		ContentType: "application/zip",
	}
	handler := NewSwiftHandler(proxy, "https://proxy.example", upstream.URL+"/registry").Routes()

	requestArchive := func(method string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "/apple/example/1.2.3.zip", nil)
		req.Header.Set("Accept", swiftAcceptArchive)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		return w
	}

	w := requestArchive(http.MethodGet)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	if got := w.Body.Bytes(); string(got) != string(archive) {
		t.Errorf("archive body = %q", got)
	}
	if !fetcher.fetchCalled {
		t.Fatal("archive fetcher was not called")
	}
	if got := fetcher.fetchedURL; got != upstream.URL+"/registry/apple/example/1.2.3.zip" {
		t.Errorf("fetched URL = %q", got)
	}
	if got := fetcher.fetchedHeader.Get("Accept"); got != swiftAcceptArchive {
		t.Errorf("archive Accept = %q, want %q", got, swiftAcceptArchive)
	}
	if got := w.Header().Get("Digest"); got != "sha-256="+base64.StdEncoding.EncodeToString(checksumBytes[:]) {
		t.Errorf("Digest = %q", got)
	}
	if got := w.Header().Get("X-Swift-Package-Signature"); got != signature {
		t.Errorf("signature = %q", got)
	}
	if got := w.Header().Get("X-Swift-Package-Signature-Format"); got != "cms-1.0.0" {
		t.Errorf("signature format = %q", got)
	}
	if got := w.Header().Get("Content-Disposition"); got != `attachment; filename="example-1.2.3.zip"` {
		t.Errorf("Content-Disposition = %q", got)
	}

	packagePURL, versionPURL := packageurl.MakeCacheStrings(
		"swift", "apple/example", "1.2.3", upstream.URL+"/registry",
	)
	if strings.HasPrefix(packagePURL, "pkg:swift/") {
		t.Fatalf("registry identity produced source PURL %q", packagePURL)
	}
	versionRecord, err := db.GetVersionByPURL(versionPURL)
	if err != nil {
		t.Fatalf("cached Swift version %q not found: %v", versionPURL, err)
	}
	if versionRecord == nil {
		t.Fatalf("cached Swift version %q not found", versionPURL)
	}
	if versionRecord.PackagePURL != packagePURL {
		t.Errorf("cached package PURL = %q, want %q", versionRecord.PackagePURL, packagePURL)
	}

	fetcher.fetchCalled = false
	w = requestArchive(http.MethodHead)
	if w.Code != http.StatusOK {
		t.Fatalf("HEAD status = %d, want 200", w.Code)
	}
	if w.Body.Len() != 0 {
		t.Errorf("HEAD body length = %d, want 0", w.Body.Len())
	}
	if got := w.Header().Get("Content-Length"); got != fmt.Sprint(len(archive)) {
		t.Errorf("HEAD Content-Length = %q", got)
	}

	w = requestArchive(http.MethodGet)
	if w.Code != http.StatusOK || w.Body.String() != string(archive) {
		t.Fatalf("cached response = %d %q", w.Code, w.Body.Bytes())
	}
	if fetcher.fetchCalled {
		t.Error("cached archive contacted artifact upstream")
	}
}

func TestSwiftSourceArchiveRejectsChecksumMismatch(t *testing.T) {
	archive := []byte("unexpected archive")
	expectedChecksum := sha256.Sum256([]byte("expected archive"))

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":"apple.example","version":"1.2.3","resources":[{"name":"source-archive","type":"application/zip","checksum":%q}]}`, hex.EncodeToString(expectedChecksum[:]))
	}))
	defer upstream.Close()

	proxy, db, store, fetcher := setupTestProxy(t)
	fetcher.artifact = &fetch.Artifact{
		Body:        io.NopCloser(strings.NewReader(string(archive))),
		Size:        int64(len(archive)),
		ContentType: "application/zip",
	}
	handler := NewSwiftHandler(proxy, "https://proxy.example", upstream.URL).Routes()

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/apple/example/1.2.3.zip", nil))

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body: %s", w.Code, w.Body.String())
	}
	if len(store.files) != 0 {
		t.Errorf("mismatched archive remained in storage: %v", store.files)
	}
	packagePURL, versionPURL := packageurl.MakeCacheStrings("swift", "apple/example", "1.2.3", upstream.URL)
	cached, err := db.GetCachedArtifact(packagePURL, versionPURL, "example-1.2.3.zip")
	if err != nil {
		t.Fatalf("checking cache: %v", err)
	}
	if cached != nil {
		t.Error("mismatched archive gained a cache record")
	}
}

func TestSwiftSourceArchiveCanonicalizesPackageIdentity(t *testing.T) {
	archive := []byte("swift source archive")
	checksumBytes := sha256.Sum256(archive)
	checksum := hex.EncodeToString(checksumBytes[:])
	var metadataPaths []string

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		metadataPaths = append(metadataPaths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":"apple.example","version":"1.2.3","resources":[{"name":"source-archive","type":"application/zip","checksum":%q}]}`, checksum)
	}))
	defer upstream.Close()

	proxy, db, store, fetcher := setupTestProxy(t)
	handler := NewSwiftHandler(proxy, "https://proxy.example", upstream.URL).Routes()
	requestArchive := func(path string) {
		fetcher.artifact = &fetch.Artifact{
			Body:        io.NopCloser(strings.NewReader(string(archive))),
			Size:        int64(len(archive)),
			ContentType: "application/zip",
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s status = %d, want 200; body: %s", path, w.Code, w.Body.String())
		}
	}

	requestArchive("/apple/example/1.2.3.zip")
	requestArchive("/APPLE/EXAMPLE/1.2.3.zip")

	if len(store.files) != 1 {
		t.Errorf("cached files = %d, want 1", len(store.files))
	}
	for _, path := range metadataPaths {
		if path != "/apple/example/1.2.3" {
			t.Errorf("metadata path = %q, want canonical lowercase path", path)
		}
	}

	canonicalPURL, _ := packageurl.MakeCacheStrings("swift", "apple/example", "1.2.3", upstream.URL)
	canonical, err := db.GetPackageByPURL(canonicalPURL)
	if err != nil {
		t.Fatalf("getting canonical package: %v", err)
	}
	if canonical == nil {
		t.Fatalf("canonical package %q not found", canonicalPURL)
	}

	nonCanonicalPURL, _ := packageurl.MakeCacheStrings("swift", "APPLE/EXAMPLE", "1.2.3", upstream.URL)
	if nonCanonicalPURL != canonicalPURL {
		t.Errorf("uppercase cache PURL = %q, want %q", nonCanonicalPURL, canonicalPURL)
	}
}

func TestSwiftSourceArchiveHeadRejectsCachedChecksumMismatch(t *testing.T) {
	archive := []byte("cached archive")
	expectedChecksum := sha256.Sum256([]byte("expected archive"))

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":"apple.example","version":"1.2.3","resources":[{"name":"source-archive","type":"application/zip","checksum":%q}]}`, hex.EncodeToString(expectedChecksum[:]))
	}))
	defer upstream.Close()

	proxy, _, store, fetcher := setupTestProxy(t)
	fetcher.artifact = &fetch.Artifact{
		Body:        io.NopCloser(strings.NewReader(string(archive))),
		Size:        int64(len(archive)),
		ContentType: "application/zip",
	}
	packagePURL, versionPURL := packageurl.MakeCacheStrings("swift", "apple/example", "1.2.3", upstream.URL)
	cached, err := proxy.getOrFetchArtifactFromURLWithCachePURLs(
		context.Background(), "swift", "apple/example", "1.2.3", "example-1.2.3.zip",
		packagePURL, versionPURL, upstream.URL+"/apple/example/1.2.3.zip", nil, "",
	)
	if err != nil {
		t.Fatalf("seeding cache: %v", err)
	}
	_ = cached.Reader.Close()

	handler := NewSwiftHandler(proxy, "https://proxy.example", upstream.URL).Routes()
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodHead, "/apple/example/1.2.3.zip", nil))

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body: %s", w.Code, w.Body.String())
	}
	if len(store.files) != 0 {
		t.Errorf("cached mismatched archive remained in storage: %v", store.files)
	}
}

func TestSwiftSourceArchiveRequiresReleaseMetadata(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer upstream.Close()

	proxy, _, store, fetcher := setupTestProxy(t)
	fetcher.artifact = &fetch.Artifact{
		Body:        io.NopCloser(strings.NewReader("signed archive")),
		ContentType: "application/zip",
	}
	handler := NewSwiftHandler(proxy, "https://proxy.example", upstream.URL).Routes()

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/apple/example/1.2.3.zip", nil))

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body: %s", w.Code, w.Body.String())
	}
	if fetcher.fetchCalled {
		t.Error("archive was fetched without release security metadata")
	}
	if len(store.files) != 0 {
		t.Errorf("archive was cached without release security metadata: %v", store.files)
	}
}

func TestSwiftSourceArchiveColdHeadUsesRangeGetAcrossRedirect(t *testing.T) {
	checksum := strings.Repeat("a", sha256.Size*2)
	var archiveAccept string
	var archiveMethod string
	var archiveRange string
	download := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		archiveMethod = r.Method
		archiveRange = r.Header.Get("Range")
		w.Header().Set("Content-Range", "bytes 0-0/123")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte("x"))
	}))
	defer download.Close()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/apple/example/1.2.3":
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"id":"apple.example","version":"1.2.3","resources":[{"name":"source-archive","type":"application/zip","checksum":%q}]}`, checksum)
		case "/apple/example/1.2.3.zip":
			archiveAccept = r.Header.Get("Accept")
			http.Redirect(w, r, download.URL, http.StatusSeeOther)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	proxy, _, _, _ := setupTestProxy(t)
	proxy.HTTPClient = upstream.Client()
	handler := NewSwiftHandler(proxy, "https://proxy.example", upstream.URL).Routes()

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodHead, "/apple/example/1.2.3.zip", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	if archiveMethod != http.MethodGet {
		t.Errorf("download method = %q, want GET", archiveMethod)
	}
	if archiveRange != "bytes=0-0" {
		t.Errorf("download Range = %q, want bytes=0-0", archiveRange)
	}
	if archiveAccept != swiftAcceptArchive {
		t.Errorf("upstream Accept = %q, want %q", archiveAccept, swiftAcceptArchive)
	}
	if got := w.Header().Get("Content-Length"); got != "123" {
		t.Errorf("Content-Length = %q, want 123", got)
	}
}

func TestSwiftIdentifiersAndPublishingUnsupported(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/registry/identifiers" {
			t.Errorf("upstream path = %q", r.URL.Path)
		}
		if got := r.URL.Query().Get("url"); got != "https://github.com/apple/example" {
			t.Errorf("lookup URL = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"identifiers":["apple.example"]}`)
	}))
	defer upstream.Close()

	proxy, _, _, _ := setupTestProxy(t)
	handler := NewSwiftHandler(proxy, "https://proxy.example", upstream.URL+"/registry").Routes()

	req := httptest.NewRequest(http.MethodGet, "/identifiers?url=https%3A%2F%2Fgithub.com%2Fapple%2Fexample", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "apple.example") {
		t.Fatalf("identifier response = %d %q", w.Code, w.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/identifiers", nil)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("missing URL status = %d, want 400", w.Code)
	}

	req = httptest.NewRequest(http.MethodPut, "/apple/example/1.2.3", strings.NewReader("ignored"))
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("publish status = %d, want 405", w.Code)
	}
	if got := w.Header().Get("Allow"); got != "GET, HEAD" {
		t.Errorf("Allow = %q", got)
	}
}

func TestSwiftIdentifierValidation(t *testing.T) {
	tests := []struct {
		name  string
		value string
		valid func(string) bool
		want  bool
	}{
		{"scope", "apple", validSwiftScope, true},
		{"scope hyphen", "swift-server", validSwiftScope, true},
		{"scope underscore", "swift_server", validSwiftScope, false},
		{"scope repeated separator", "swift--server", validSwiftScope, false},
		{"package", "swift-argument_parser", validSwiftPackageName, true},
		{"package repeated separators", "swift-_argument", validSwiftPackageName, false},
		{"package trailing separator", "example-", validSwiftPackageName, false},
		{"package non-ASCII", "café", validSwiftPackageName, false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := test.valid(test.value); got != test.want {
				t.Errorf("validation of %q = %v, want %v", test.value, got, test.want)
			}
		})
	}
}
