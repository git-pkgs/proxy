package handler

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// TestHandlersRejectQueryCharsInPath checks that handlers which build upstream
// URLs from the decoded path refuse an encoded '?' or '#' themselves, without
// relying on the server middleware, and never contact the upstream or fetch
// an artifact.
func TestHandlersRejectQueryCharsInPath(t *testing.T) {
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte("upstream"))
	}))
	defer upstream.Close()

	proxy, _, _, fetcher := setupTestProxy(t)
	proxy.HTTPClient = upstream.Client()
	fetcher.fetchErr = errors.New("artifact fetched")

	handlers := map[string]struct {
		routes http.Handler
		paths  []string
	}{
		"debian": {NewDebianHandler(proxy, testProxyURL, upstream.URL, nil).Routes(), []string{
			"/pool/main/h/hello/hello_2.10-3_amd64.deb%3F/foo_9.9_amd64.deb",
			"/dists/trixie/InRelease%23x",
		}},
		"rpm": {NewRPMHandlerWithUpstream(proxy, testProxyURL, upstream.URL).Routes(), []string{
			"/Packages/n/nginx-1.24.0-2.fc39.x86_64.rpm%3F/foo-9.9-1.fc39.x86_64.rpm",
			"/repodata/repomd.xml%3Fx",
		}},
		"apk": {NewAPKHandler(proxy, testProxyURL, map[string]string{"alpine": upstream.URL}).Routes(), []string{
			"/alpine/v3.20/main/x86_64/busybox-1.36.1-r29.apk%3F/foo-9.9-r0.apk",
			"/alpine/v3.20/main/x86_64/APKINDEX.tar.gz%23x",
		}},
		"pypi": {NewPyPIHandlerWithUpstreams(proxy, testProxyURL, upstream.URL, upstream.URL).Routes(), []string{
			"/packages/aa/bb/requests-2.31.0.tar.gz%3F/foo-9.9.tar.gz",
			"/simple/requests%3Fx/",
		}},
		"cran": {NewCRANHandlerWithUpstream(proxy, testProxyURL, upstream.URL).Routes(), []string{
			"/src/contrib/ggplot2_3.4.4.tar.gz%3Ffoo_9.9.tar.gz",
			"/bin/macosx%23x/contrib/4.3/PACKAGES",
		}},
		"maven": {NewMavenHandler(proxy, testProxyURL, upstream.URL, upstream.URL).Routes(), []string{
			"/com/example/lib/1.0/lib-1.0.jar%3F/com/example/foo/9.9/foo-9.9.jar",
			"/com/example/lib/maven-metadata.xml%23x",
		}},
	}
	for name, tc := range handlers {
		for _, target := range tc.paths {
			w := httptest.NewRecorder()
			tc.routes.ServeHTTP(w, httptest.NewRequest(http.MethodGet, target, nil))
			if w.Code != http.StatusBadRequest {
				t.Errorf("%s: GET %s = %d, want %d", name, target, w.Code, http.StatusBadRequest)
			}
		}
	}
	if got := requests.Load(); got != 0 {
		t.Errorf("upstream requests = %d, want 0", got)
	}
	if fetcher.fetchCalled {
		t.Errorf("artifact fetched from %s", fetcher.fetchedURL)
	}
}
