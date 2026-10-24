package handler

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/git-pkgs/cooldown"
)

func TestGemParseFilename(t *testing.T) {
	h := &GemHandler{proxy: &Proxy{Logger: slog.Default()}}

	tests := []struct {
		filename    string
		wantName    string
		wantVersion string
	}{
		{"rails-7.1.0.gem", "rails", "7.1.0"},
		{"aws-sdk-s3-1.142.0.gem", "aws-sdk-s3", "1.142.0"},
		{"nokogiri-1.15.4-x86_64-linux.gem", "nokogiri", "1.15.4-x86_64-linux"},
		{"activerecord-7.0.8.gem", "activerecord", "7.0.8"},
		{"sorbet-static-0.5.11002-universal-darwin-22.gem", "sorbet-static", "0.5.11002-universal-darwin-22"},
		{"jruby-openssl-0.14.2-universal-java-17.gem", "jruby-openssl", "0.14.2-universal-java-17"},
		{"google-protobuf-3.25.3-arm64-darwin-23.gem", "google-protobuf", "3.25.3-arm64-darwin-23"},
		{"nokogiri-1.16.0-x64-mingw-ucrt.gem", "nokogiri", "1.16.0-x64-mingw-ucrt"},
		{"rake-13.1.0.pre.1.gem", "rake", "13.1.0.pre.1"},
		{"devise-2fa-1.0.0.gem", "devise-2fa", "1.0.0"},
		{"devise-2fa-1.0.0-java.gem", "devise-2fa", "1.0.0-java"},
		{"foo-2-1.0.0.gem", "foo-2", "1.0.0"},
		{"foo-2-1.gem", "foo-2", "1"},
		{"foo-2-x86_64-linux.gem", "foo", "2-x86_64-linux"},
		{"odd-1_0.gem", "odd", "1_0"},
		{"no-version.gem", "", ""},
		{"invalid", "", ""},
	}

	for _, tt := range tests {
		name, version := h.parseGemFilename(tt.filename)
		if name != tt.wantName || version != tt.wantVersion {
			t.Errorf("parseGemFilename(%q) = (%q, %q), want (%q, %q)",
				tt.filename, name, version, tt.wantName, tt.wantVersion)
		}
	}
}

func TestGemDownloadRefusesLongFilename(t *testing.T) {
	// Every "-1-" could start a version, and each try reads to the "!" at
	// the end before failing, so parsing this used to take minutes.
	filename := strings.Repeat("-1-aaaaaa", 100_000) + "!.gem"

	h := &GemHandler{proxy: testProxy(), upstreamURL: "http://upstream.invalid"}
	req := httptest.NewRequest(http.MethodGet, "/gems/x", nil)
	req.SetPathValue("filename", "x"+filename)
	w := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		h.handleDownload(w, req)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("parsing a long gem filename did not finish within 5s")
	}
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestGemCompactIndexCooldown(t *testing.T) {
	now := time.Now()
	oldTime := now.Add(-7 * 24 * time.Hour).Format(time.RFC3339)
	recentTime := now.Add(-1 * time.Hour).Format(time.RFC3339)

	compactIndex := "---\n1.0.0 dep1:>= 1.0|checksum:abc123\n2.0.0 dep1:>= 1.0|checksum:def456\n"

	versionsJSON, _ := json.Marshal([]gemVersion{
		{Number: "1.0.0", Platform: "ruby", CreatedAt: oldTime},
		{Number: "2.0.0", Platform: "ruby", CreatedAt: recentTime},
	})

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/info/"):
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte(compactIndex))
		case strings.HasPrefix(r.URL.Path, "/api/v1/versions/"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(versionsJSON)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()

	proxy := testProxy()
	proxy.Cooldown = &cooldown.Config{
		Default: "3d",
	}

	h := &GemHandler{
		proxy:       proxy,
		upstreamURL: upstream.URL,
		proxyURL:    "http://proxy.local",
	}

	req := httptest.NewRequest(http.MethodGet, "/info/testgem", nil)
	req.SetPathValue("name", "testgem")
	w := httptest.NewRecorder()
	h.handleCompactIndex(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	body := w.Body.String()
	if !strings.Contains(body, "1.0.0") {
		t.Error("expected version 1.0.0 to survive filtering")
	}
	if strings.Contains(body, "2.0.0") {
		t.Error("expected version 2.0.0 to be filtered out")
	}
	if !strings.HasPrefix(body, "---\n") {
		t.Error("expected compact index header to be preserved")
	}
}

// gemCooldownUpstream serves index as /info/testgem, gzipped when the
// request accepts gzip as rubygems.org does, with 1.0.0 published a week ago
// and 2.0.0 an hour ago.
func gemCooldownUpstream(t *testing.T, index string, headers http.Header) *httptest.Server {
	t.Helper()
	now := time.Now()
	versionsJSON, _ := json.Marshal([]gemVersion{
		{Number: "1.0.0", Platform: "ruby", CreatedAt: now.Add(-7 * 24 * time.Hour).Format(time.RFC3339)},
		{Number: "2.0.0", Platform: "ruby", CreatedAt: now.Add(-time.Hour).Format(time.RFC3339)},
	})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/info/testgem":
			for k, vv := range headers {
				w.Header()[k] = vv
			}
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
				_, _ = w.Write([]byte(index))
				return
			}
			w.Header().Set("Content-Encoding", "gzip")
			gz := gzip.NewWriter(w)
			_, _ = gz.Write([]byte(index))
			_ = gz.Close()
		case "/api/v1/versions/testgem.json":
			_, _ = w.Write(versionsJSON)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(upstream.Close)
	return upstream
}

func TestGemCompactIndexCooldownClientAcceptsGzip(t *testing.T) {
	index := "---\n1.0.0 dep1:>= 1.0|checksum:abc123\n2.0.0 dep1:>= 1.0|checksum:def456\n"
	upstream := gemCooldownUpstream(t, index, http.Header{
		"Etag":        {`"upstream"`},
		"Repr-Digest": {"sha-256=:upstream:"},
	})

	proxy := testProxy()
	proxy.Cooldown = &cooldown.Config{Default: "3d"}
	h := &GemHandler{proxy: proxy, upstreamURL: upstream.URL}

	// Bundler sends Accept-Encoding: gzip by default.
	req := httptest.NewRequest(http.MethodGet, "/info/testgem", nil)
	req.Header.Set("Accept-Encoding", "gzip;q=1.0,deflate;q=0.6,identity;q=0.3")
	req.SetPathValue("name", "testgem")
	w := httptest.NewRecorder()
	h.handleCompactIndex(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	if ce := w.Header().Get("Content-Encoding"); ce != "" {
		t.Errorf("Content-Encoding = %q, want none", ce)
	}
	want := "---\n1.0.0 dep1:>= 1.0|checksum:abc123\n"
	if got := w.Body.String(); got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
	for _, hdr := range []string{"ETag", "Repr-Digest"} {
		if v := w.Header().Get(hdr); v != "" {
			t.Errorf("%s = %q, upstream's value must not describe the filtered index", hdr, v)
		}
	}
}

func TestGemCompactIndexCooldownLongLine(t *testing.T) {
	// A version line longer than bufio.Scanner's 64 KB default used to end
	// the response early with a 200.
	long := "1.0.0 " + strings.Repeat("dep:>= 1.0,", 10_000) + "|checksum:abc"
	index := "---\n" + long + "\n2.0.0 |checksum:def\n3.0.0 |checksum:ghi\n"
	upstream := gemCooldownUpstream(t, index, nil)

	proxy := testProxy()
	proxy.Cooldown = &cooldown.Config{Default: "3d"}
	h := &GemHandler{proxy: proxy, upstreamURL: upstream.URL}

	req := httptest.NewRequest(http.MethodGet, "/info/testgem", nil)
	req.SetPathValue("name", "testgem")
	w := httptest.NewRecorder()
	h.handleCompactIndex(w, req)

	want := "---\n" + long + "\n3.0.0 |checksum:ghi\n"
	if w.Code != http.StatusOK || w.Body.String() != want {
		t.Fatalf("status = %d, body of %d bytes; want 200 with %d bytes", w.Code, w.Body.Len(), len(want))
	}
}

func TestGemCompactIndexCooldownOversizedIndex(t *testing.T) {
	index := "---\n1.0.0 |checksum:abc\n" + strings.Repeat("9.9.9 |checksum:zzz\n", 100)
	upstream := gemCooldownUpstream(t, index, nil)

	proxy := testProxy()
	proxy.Cooldown = &cooldown.Config{Default: "3d"}
	proxy.MetadataMaxSize = 256
	h := &GemHandler{proxy: proxy, upstreamURL: upstream.URL}

	req := httptest.NewRequest(http.MethodGet, "/info/testgem", nil)
	req.SetPathValue("name", "testgem")
	w := httptest.NewRecorder()
	h.handleCompactIndex(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusBadGateway)
	}
}

func TestGemCompactIndexCooldownWithPlatformVersion(t *testing.T) {
	now := time.Now()
	recentTime := now.Add(-1 * time.Hour).Format(time.RFC3339)

	compactIndex := "---\n1.0.0 dep:>= 1.0|checksum:abc\n1.0.0-java dep:>= 1.0|checksum:def\n"

	versionsJSON, _ := json.Marshal([]gemVersion{
		{Number: "1.0.0", Platform: "ruby", CreatedAt: recentTime},
		{Number: "1.0.0", Platform: "java", CreatedAt: recentTime},
	})

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/info/"):
			_, _ = w.Write([]byte(compactIndex))
		case strings.HasPrefix(r.URL.Path, "/api/v1/versions/"):
			_, _ = w.Write(versionsJSON)
		}
	}))
	defer upstream.Close()

	proxy := testProxy()
	proxy.Cooldown = &cooldown.Config{
		Default: "3d",
	}

	h := &GemHandler{
		proxy:       proxy,
		upstreamURL: upstream.URL,
		proxyURL:    "http://proxy.local",
	}

	req := httptest.NewRequest(http.MethodGet, "/info/testgem", nil)
	req.SetPathValue("name", "testgem")
	w := httptest.NewRecorder()
	h.handleCompactIndex(w, req)

	body := w.Body.String()
	// Both ruby and java platform versions should be filtered
	lines := strings.Split(strings.TrimSpace(body), "\n")
	if len(lines) != 1 { // only "---"
		t.Errorf("expected only header line, got %d lines: %v", len(lines), lines)
	}
}

func TestGemCompactIndexNoCooldown(t *testing.T) {
	compactIndex := "---\n1.0.0 dep:>= 1.0|checksum:abc\n"

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(compactIndex))
	}))
	defer upstream.Close()

	h := &GemHandler{
		proxy:       testProxy(), // no cooldown
		upstreamURL: upstream.URL,
		proxyURL:    "http://proxy.local",
	}

	req := httptest.NewRequest(http.MethodGet, "/info/testgem", nil)
	req.SetPathValue("name", "testgem")
	w := httptest.NewRecorder()
	h.handleCompactIndex(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
}

func TestGemCompactIndexVersionsAPIFails(t *testing.T) {
	compactIndex := "---\n1.0.0 dep:>= 1.0|checksum:abc\n"

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/info/"):
			_, _ = w.Write([]byte(compactIndex))
		case strings.HasPrefix(r.URL.Path, "/api/v1/versions/"):
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer upstream.Close()

	proxy := testProxy()
	proxy.Cooldown = &cooldown.Config{
		Default: "3d",
	}

	h := &GemHandler{
		proxy:       proxy,
		upstreamURL: upstream.URL,
		proxyURL:    "http://proxy.local",
	}

	req := httptest.NewRequest(http.MethodGet, "/info/testgem", nil)
	req.SetPathValue("name", "testgem")
	w := httptest.NewRecorder()
	h.handleCompactIndex(w, req)

	// Without publish times the index can't be filtered, so it isn't served.
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusBadGateway)
	}
	if strings.Contains(w.Body.String(), "1.0.0") {
		t.Error("served the unfiltered index when the versions API failed")
	}
}

func TestGemFetchFilteredVersions(t *testing.T) {
	now := time.Now()
	oldTime := now.Add(-7 * 24 * time.Hour).Format(time.RFC3339)
	recentTime := now.Add(-1 * time.Hour).Format(time.RFC3339)

	versionsJSON, _ := json.Marshal([]gemVersion{
		{Number: "1.0.0", Platform: "ruby", CreatedAt: oldTime},
		{Number: "2.0.0", Platform: "ruby", CreatedAt: recentTime},
		{Number: "2.0.0", Platform: "java", CreatedAt: recentTime},
	})

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(versionsJSON)
	}))
	defer upstream.Close()

	proxy := testProxy()
	proxy.Cooldown = &cooldown.Config{
		Default: "3d",
	}

	h := &GemHandler{
		proxy:       proxy,
		upstreamURL: upstream.URL,
		proxyURL:    "http://proxy.local",
	}

	req := httptest.NewRequest(http.MethodGet, "/info/testgem", nil)
	filtered, err := h.fetchFilteredVersions(req, "testgem")
	if err != nil {
		t.Fatal(err)
	}

	if filtered["1.0.0"] {
		t.Error("version 1.0.0 should not be filtered (old enough)")
	}
	if !filtered["2.0.0"] {
		t.Error("version 2.0.0 (ruby) should be filtered")
	}
	if !filtered["2.0.0-java"] {
		t.Error("version 2.0.0-java should be filtered")
	}

	_ = fmt.Sprintf // silence unused import
}

func TestGemCooldownVersionsSizeLimit(t *testing.T) {
	versionsJSON, _ := json.Marshal([]gemVersion{
		{Number: "2.0.0", Platform: "ruby", CreatedAt: time.Now().Add(-time.Hour).Format(time.RFC3339)},
	})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/v1/versions/") {
			_, _ = w.Write(versionsJSON)
			_, _ = w.Write([]byte(strings.Repeat(" ", 4096)))
			return
		}
		_, _ = w.Write([]byte("---\n2.0.0 |checksum:abc\n"))
	}))
	defer upstream.Close()

	proxy := testProxy()
	proxy.Cooldown = &cooldown.Config{Default: "3d"}
	proxy.MetadataMaxSize = 1024
	h := &GemHandler{proxy: proxy, upstreamURL: upstream.URL}

	req := httptest.NewRequest(http.MethodGet, "/info/testgem", nil)
	req.SetPathValue("name", "testgem")
	w := httptest.NewRecorder()
	h.handleCompactIndex(w, req)

	if w.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadGateway)
	}
}
