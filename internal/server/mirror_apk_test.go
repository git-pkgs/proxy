package server

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/git-pkgs/proxy/internal/config"
	"github.com/git-pkgs/proxy/internal/handler"
)

const (
	apkBusyboxPath = "/v3.20/main/x86_64/busybox-1.36.1-r29.apk"
	apkHtopPath    = "/v3.20/community/x86_64/htop-3.3.0-r0.apk"
	apkMainIndex   = "/v3.20/main/x86_64/APKINDEX.tar.gz"
	apkCommIndex   = "/v3.20/community/x86_64/APKINDEX.tar.gz"
)

// apkUpstream serves an Alpine mirror with busybox in v3.20/main and htop in
// v3.20/community, for x86_64. It counts requests by path.
type apkUpstream struct {
	*httptest.Server
	mu    sync.Mutex
	files map[string]string
	hits  map[string]int
}

func apkIndexArchive(t *testing.T, index string) string {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	if err := tw.WriteHeader(&tar.Header{Name: "APKINDEX", Mode: 0o644, Size: int64(len(index))}); err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(tw, index)
	_ = tw.Close()
	_ = zw.Close()
	return buf.String()
}

func newAPKUpstream(t *testing.T) *apkUpstream {
	t.Helper()
	u := &apkUpstream{
		files: map[string]string{
			apkMainIndex:                           apkIndexArchive(t, "P:busybox\nV:1.36.1-r29\nA:x86_64\nS:18\n\nP:musl\nV:1.2.5-r0\nA:x86_64\nS:15\n\n"),
			apkCommIndex:                           apkIndexArchive(t, "P:htop\nV:3.3.0-r0\nA:x86_64\nS:15\n\n"),
			apkBusyboxPath:                         "busybox 1.36.1-r29",
			apkHtopPath:                            "htop 3.3.0-r0 x86",
			"/v3.20/main/x86_64/musl-1.2.5-r0.apk": "musl 1.2.5-r0",
		},
		hits: map[string]int{},
	}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		body, ok := u.files[r.URL.Path]
		u.hits[r.URL.Path]++
		u.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(u.Close)
	return u
}

func (u *apkUpstream) hitsFor(path string) int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.hits[path]
}

func apkMirrorConfig(upstream *apkUpstream) *config.Config {
	cfg := config.Default()
	cfg.Upstream.APK = map[string]string{"alpine": upstream.URL}
	return cfg
}

func getAPK(proxy *handler.Proxy, upstream *apkUpstream, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h := handler.NewAPKHandler(proxy, "http://proxy.test", map[string]string{"alpine": upstream.URL})
	h.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/alpine"+path, nil))
	return rec
}

func TestMirrorAPKPackagesAreServedFromCache(t *testing.T) {
	upstream := newAPKUpstream(t)

	progress, proxy := mirrorWithConfig(t, apkMirrorConfig(upstream),
		"pkg:apk/alpine/busybox@1.36.1-r29?arch=x86_64&distro=alpine-3.20.3",
		"pkg:apk/alpine/htop@3.3.0-r0?arch=x86_64&distro=alpine-3.20.3",
		"pkg:apk/alpine/musl@1.2.5-r0?arch=x86_64&distro=alpine-3.20.3")

	if progress.Completed != 3 || progress.Failed != 0 {
		t.Fatalf("completed = %d, failed = %d, errors = %v", progress.Completed, progress.Failed, progress.Errors)
	}
	for _, path := range []string{apkBusyboxPath, apkHtopPath, apkMainIndex, apkCommIndex} {
		if got := upstream.hitsFor(path); got != 1 {
			t.Errorf("%s fetched %d times, want 1", path, got)
		}
	}

	// apk gets the packages from the cache once the upstream is down.
	upstream.Close()
	for path, want := range map[string]string{
		apkBusyboxPath: "busybox 1.36.1-r29",
		apkHtopPath:    "htop 3.3.0-r0 x86",
	} {
		if rec := getAPK(proxy, upstream, path); rec.Code != http.StatusOK || rec.Body.String() != want {
			t.Errorf("%s from cache: status = %d, body = %q, want 200 and %q", path, rec.Code, rec.Body, want)
		}
	}
}

func TestMirrorAPKRefusesIncompletePURLs(t *testing.T) {
	upstream := newAPKUpstream(t)

	tests := map[string]string{
		"pkg:apk/alpine/busybox@1.36.1-r29?arch=x86_64":                         "distro",
		"pkg:apk/alpine/busybox@1.36.1-r29?distro=alpine-3.20.3":                "arch",
		"pkg:apk/alpine/busybox@1.36.1-r29?arch=x86_64&distro=debian-12":        "distro",
		"pkg:apk/other/busybox@1.36.1-r29?arch=x86_64&distro=alpine-3.20.3":     "upstream.apk",
		"pkg:apk/alpine/busybox@9.9.9-r0?arch=x86_64&distro=alpine-3.20.3":      "9.9.9-r0",
		"pkg:apk/alpine/busybox@1.36.1-r29?arch=../../etc&distro=alpine-3.20.3": "arch",
	}
	for p, want := range tests {
		t.Run(p, func(t *testing.T) {
			progress, _ := mirrorWithConfig(t, apkMirrorConfig(upstream), p)
			if progress.Failed != 1 || len(progress.Errors) != 1 || !strings.Contains(progress.Errors[0].Error, want) {
				t.Errorf("failed = %d, errors = %v; want one error mentioning %q", progress.Failed, progress.Errors, want)
			}
		})
	}
}
