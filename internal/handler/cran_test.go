package handler

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/git-pkgs/registries/fetch"
)

func TestCRANParseSourceFilename(t *testing.T) {
	h := &CRANHandler{proxy: &Proxy{Logger: slog.Default()}}

	tests := []struct {
		filename    string
		wantName    string
		wantVersion string
	}{
		{"ggplot2_3.4.4.tar.gz", "ggplot2", "3.4.4"},
		{"data.table_1.14.8.tar.gz", "data.table", "1.14.8"},
		{"Rcpp_1.0.11.tar.gz", "Rcpp", "1.0.11"},
		{"invalid.tar.gz", "", ""},
	}

	for _, tt := range tests {
		name, version := h.parseSourceFilename(tt.filename)
		if name != tt.wantName || version != tt.wantVersion {
			t.Errorf("parseSourceFilename(%q) = (%q, %q), want (%q, %q)",
				tt.filename, name, version, tt.wantName, tt.wantVersion)
		}
	}
}

func TestCRANParseBinaryFilename(t *testing.T) {
	h := &CRANHandler{proxy: &Proxy{Logger: slog.Default()}}

	tests := []struct {
		filename    string
		wantName    string
		wantVersion string
	}{
		{"ggplot2_3.4.4.zip", "ggplot2", "3.4.4"},
		{"ggplot2_3.4.4.tgz", "ggplot2", "3.4.4"},
		{"data.table_1.14.8.zip", "data.table", "1.14.8"},
		{"invalid.zip", "", ""},
	}

	for _, tt := range tests {
		name, version := h.parseBinaryFilename(tt.filename)
		if name != tt.wantName || version != tt.wantVersion {
			t.Errorf("parseBinaryFilename(%q) = (%q, %q), want (%q, %q)",
				tt.filename, name, version, tt.wantName, tt.wantVersion)
		}
	}
}

func TestCRANHandler_MacOSTwoSegmentIndexesAndMeta(t *testing.T) {
	for _, path := range []string{
		"/bin/macosx/big-sur-arm64/contrib/4.4/PACKAGES",
		"/bin/macosx/big-sur-x86_64/contrib/4.4/PACKAGES.gz",
		"/bin/macosx/sonoma-arm64/contrib/4.5/PACKAGES.rds",
		"/src/contrib/Meta/archive.rds",
		"/src/contrib/Meta/current.rds",
	} {
		t.Run(path, func(t *testing.T) {
			assertUpstreamProxied(t, "index", path,
				func(proxy *Proxy, upstreamURL string) http.Handler {
					h := &CRANHandler{proxy: proxy, upstreamURL: upstreamURL, proxyURL: "http://localhost"}
					return h.Routes()
				},
			)
		})
	}
}

func TestCRANHandler_MacOSTwoSegmentBinaryDownload(t *testing.T) {
	proxy, _, _, fetcher := setupTestProxy(t)
	fetcher.artifact = &fetch.Artifact{
		Body:        io.NopCloser(strings.NewReader("fetched binary")),
		ContentType: "application/x-gzip",
	}

	h := NewCRANHandler(proxy, "http://localhost")
	h.upstreamURL = "https://cran.r-project.org"
	srv := httptest.NewServer(h.Routes())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/bin/macosx/big-sur-arm64/contrib/4.4/dplyr_1.1.4.tgz")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	want := "https://cran.r-project.org/bin/macosx/big-sur-arm64/contrib/4.4/dplyr_1.1.4.tgz"
	if fetcher.fetchedURL != want {
		t.Errorf("upstream URL = %q, want %q", fetcher.fetchedURL, want)
	}
}

func TestCRANHandler_MacOSTwoSegmentBinaryCacheIdentity(t *testing.T) {
	proxy, db, store, _ := setupTestProxy(t)
	seedPackageWithPURL(t, db, store, "cran", "dplyr", "1.1.4_macosx-big-sur-arm64_4.4", "dplyr_1.1.4.tgz", "arm64 binary")

	h := NewCRANHandler(proxy, "http://localhost")
	srv := httptest.NewServer(h.Routes())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/bin/macosx/big-sur-arm64/contrib/4.4/dplyr_1.1.4.tgz")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "arm64 binary" {
		t.Errorf("body = %q, want %q", body, "arm64 binary")
	}
}
