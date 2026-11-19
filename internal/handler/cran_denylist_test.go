package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestCRANDenylistBlocksBinaries checks a denied version's binary packages
// are refused like its source package, cached or not, although they are
// cached under a version qualified by platform and R version.
func TestCRANDenylistBlocksBinaries(t *testing.T) {
	proxy, db, store, fetcher := setupTestProxy(t)
	seedPackageWithPURL(t, db, store, "cran", "dplyr", "1.1.0_windows_4.3", "dplyr_1.1.0.zip", "cran binary")
	seedPackageWithPURL(t, db, store, "cran", "dplyr", "1.0.0_windows_4.3", "dplyr_1.0.0.zip", "older binary")
	setTestDenylist(t, proxy, "pkg:cran/dplyr@1.1.0")
	srv := httptest.NewServer(NewCRANHandler(proxy, "http://localhost").Routes())
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
	if fetcher.fetchCalled {
		t.Errorf("fetched %s from upstream, want no fetch", fetcher.fetchedURL)
	}
}
