package handler

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/git-pkgs/purl"
	"github.com/git-pkgs/registries/fetch"
)

func TestRPMHandler_parseRPMPath(t *testing.T) {
	h := &RPMHandler{}

	assertPathParser(t, "parseRPMPath", h.parseRPMPath, []pathParseCase{
		{"releases/39/Everything/x86_64/os/Packages/n/nginx-1.24.0-1.fc39.x86_64.rpm", "nginx", "1.24.0-1.fc39", "x86_64"},
		{"Packages/kernel-core-6.5.5-200.fc38.x86_64.rpm", "kernel-core", "6.5.5-200.fc38", "x86_64"},
		{"updates/39/Everything/aarch64/Packages/g/git-2.42.0-1.fc39.aarch64.rpm", "git", "2.42.0-1.fc39", "aarch64"},
		{"vim-enhanced-9.0.1000-1.fc38.noarch.rpm", "vim-enhanced", "9.0.1000-1.fc38", "noarch"},
		{"invalid.rpm", "", "", ""},
		{"not-an-rpm-file", "", "", ""},
	})
}

func TestRPMHandler_Routes(t *testing.T) {
	h := NewRPMHandler(nil, "http://localhost:8080")
	assertRoutesBasics(t, h.Routes(), "/repodata/repomd.xml", "/releases/../../../etc/passwd")
}

func TestRPMRetentionKey(t *testing.T) {
	tests := []struct {
		key  string
		want string
		ok   bool
	}{
		{"pkg:rpm/curl", "pkg:rpm/curl", true},
		{"pkg:rpm/fedora/curl", "pkg:rpm/curl", true},
		{"pkg:rpm/Fedora/NetworkManager", "pkg:rpm/NetworkManager", true},
		{"pkg:rpm/fedora/libstdc++", "pkg:rpm/libstdc%2B%2B", true},
		{"pkg:rpm/fedora/40/curl", "", false},
		{"pkg:rpm/fedora/Packages%2Fcurl", "", false},
		{"pkg:deb/fedora/curl", "", false},
	}
	for _, tt := range tests {
		p, err := purl.Parse(tt.key)
		if err != nil {
			t.Fatalf("Parse(%s): %v", tt.key, err)
		}
		got, ok := rpmRetentionKey(p)
		if ok != tt.ok || (ok && got != tt.want) {
			t.Errorf("rpmRetentionKey(%s) = %q, %v, want %q, %v", tt.key, got, ok, tt.want, tt.ok)
		}
	}
	if _, ok := rpmRetentionKey(nil); ok {
		t.Error("rpmRetentionKey(nil) accepted")
	}
}

// TestRPMHandler_PackageDownloadCachesPerRepository checks that two
// repositories under one upstream that ship the same filename with different
// bytes are cached apart, as on a mirror root serving several distributions.
func TestRPMHandler_PackageDownloadCachesPerRepository(t *testing.T) {
	packages := map[string]string{
		"/rocky/9/BaseOS/x86_64/os/Packages/b/bash-5.1.8-9.el9.x86_64.rpm":     "rocky bash",
		"/almalinux/9/BaseOS/x86_64/os/Packages/b/bash-5.1.8-9.el9.x86_64.rpm": "alma bash",
	}

	var available atomic.Bool
	available.Store(true)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, ok := packages[r.URL.Path]
		if !available.Load() || !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(data))
	}))
	defer upstream.Close()

	proxy, _, _, _ := setupTestProxy(t)
	fetcher := fetch.NewFetcher(fetch.WithHTTPClient(upstream.Client()), fetch.WithMaxRetries(0))
	proxy.Fetcher = fetcher
	t.Cleanup(func() { _ = fetcher.Close() })
	srv := httptest.NewServer(NewRPMHandlerWithUpstream(proxy, testProxyURL, upstream.URL).Routes())
	defer srv.Close()

	for round := range 2 {
		for path, want := range packages {
			resp, err := http.Get(srv.URL + path)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK || string(body) != want {
				t.Errorf("round %d %s: %d %q, want 200 %q", round, path, resp.StatusCode, body, want)
			}
		}
		// The second round must come from the cache.
		available.Store(false)
	}
}

// TestRPMHandler_PackageDownloadCleansPath checks that spellings of one
// path with empty or "." segments share one cache entry.
func TestRPMHandler_PackageDownloadCleansPath(t *testing.T) {
	const want = "bash"
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rocky/9/BaseOS/x86_64/os/Packages/b/bash-5.1.8-9.el9.x86_64.rpm" {
			http.NotFound(w, r)
			return
		}
		hits.Add(1)
		_, _ = w.Write([]byte(want))
	}))
	defer upstream.Close()

	proxy, _, _, _ := setupTestProxy(t)
	fetcher := fetch.NewFetcher(fetch.WithHTTPClient(upstream.Client()), fetch.WithMaxRetries(0))
	proxy.Fetcher = fetcher
	t.Cleanup(func() { _ = fetcher.Close() })
	srv := httptest.NewServer(NewRPMHandlerWithUpstream(proxy, testProxyURL, upstream.URL).Routes())
	defer srv.Close()

	for _, path := range []string{
		"/rocky/9/BaseOS/x86_64/os/Packages/b/bash-5.1.8-9.el9.x86_64.rpm",
		"/rocky//9/BaseOS/x86_64/os/Packages/b/bash-5.1.8-9.el9.x86_64.rpm",
		"/rocky/./9/BaseOS/x86_64/os/Packages/b/bash-5.1.8-9.el9.x86_64.rpm",
	} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK || string(body) != want {
			t.Errorf("%s: %d %q, want 200 %q", path, resp.StatusCode, body, want)
		}
	}
	if n := hits.Load(); n != 1 {
		t.Errorf("upstream fetched %d times, want 1", n)
	}
}
