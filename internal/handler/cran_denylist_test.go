package handler

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// TestCRANDenylistBlocksBinaries checks a denied version's binary packages
// are refused like its source package, cached or not, although they are
// cached under a version qualified by platform and R version.
func TestCRANDenylistBlocksBinaries(t *testing.T) {
	proxy, db, store, _ := setupTestProxy(t)
	seedPackageWithPURL(t, db, store, "cran", "dplyr", "1.1.0_windows_4.3", "dplyr_1.1.0.zip", "cran binary")
	seedPackageWithPURL(t, db, store, "cran", "dplyr", "1.0.0_windows_4.3", "dplyr_1.0.0.zip", "older binary")
	setTestDenylist(t, proxy, "pkg:cran/dplyr@1.1.0")
	var upstreamRequests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamRequests.Add(1)
		_, _ = w.Write([]byte("from upstream"))
	}))
	defer upstream.Close()
	srv := httptest.NewServer(NewCRANHandler(proxy, upstream.URL).Routes())
	defer srv.Close()

	for path, want := range map[string]int{
		"/bin/windows/contrib/4.3/dplyr_1.1.0.zip":              http.StatusForbidden,
		"/bin/windows/contrib/4.4/dplyr_1.1.0.zip":              http.StatusForbidden,
		"/bin/macosx/big-sur-arm64/contrib/4.3/dplyr_1.1.0.tgz": http.StatusForbidden,
		"/src/contrib/dplyr_1.1.0.tar.gz":                       http.StatusForbidden,
		"/bin/windows/contrib/4.3/dplyr_1.0.0.zip":              http.StatusOK,
	} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("GET %s: status %d, want %d", path, resp.StatusCode, want)
		}
	}
	if got := upstreamRequests.Load(); got != 0 {
		t.Errorf("upstream got %d requests, want none", got)
	}
}
