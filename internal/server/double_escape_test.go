package server

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// TestProtocolHandlersRejectDoubleEscapedPath sends a path whose escapes were
// escaped again. Handlers check the path decoded once (rails-7.0.%30.gem)
// and append the same string to the upstream URL, where the upstream decodes
// it once more and serves rails-7.0.0.gem: a file other than the one the
// denylist and cooldown checked.
func TestProtocolHandlersRejectDoubleEscapedPath(t *testing.T) {
	transport := &countingTransport{}
	r := newProtocolRouter(t, &http.Client{Transport: transport}, nil)

	for _, target := range []string{
		"/gem/gems/rails-7.0.%2530.gem",
		"/hex/tarballs/phoenix-1.7.1%2530.tar",
		"/pypi/packages/ab/cd/ef/requests-2.31.0.tar.g%257a",
		"/maven/com/google/guava/guav%2561/32.1.3-jre/guava-32.1.3-jre.jar",
		"/go/golang.org/x/text/@v/v0.3.%2530.zip",
		"/cran/src/contrib/ggplot2_3.4.0.tar.g%257a",
		"/conda/conda-forge/linux-64/numpy-1.26.0-py312_0.cond%2561",
		"/nuget/v3-flatcontainer/newtonsoft.json/13.0.%2531/newtonsoft.json.13.0.1.nupkg",
		"/conan/v2/files/zlib/1.%2533/_/_/rev/recipe/conanfile.py",
		"/debian/pool/main/c/curl/curl_8.5.%2530-2_amd64.deb",
		"/rpm/Packages/n/nginx-1.24.%2530-1.fc39.x86_64.rpm",
		"/apk/alpine/v3.22/main/x86_64/busybox-1.37.%2530-r12.apk",
		"/npm/lodash/-/lodash-4.17.2%2531.tgz",
		"/cargo/crates/serde/1.0.%2530/download",
		"/helm/charts/nginx-1.0.%2530.tgz",
		"/v2/library/alpine/manifests/3.1%2539",
		"/gem/gems/rails-7.0.%252E.gem",
	} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, target, nil))
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", target, w.Code)
		}
	}
	if n := transport.hits.Load(); n != 0 {
		t.Errorf("upstream was contacted %d times", n)
	}
}

// TestDoubleEscapeGuardKeepsSingleEscapes keeps paths escaped once working,
// and a '%' that doesn't start an escape.
func TestDoubleEscapeGuardKeepsSingleEscapes(t *testing.T) {
	for _, target := range []string{
		"/npm/%40types%2Fnode",
		"/debian/pool/main/g/gcc/libstdc%2B%2B6_14.2.0-1_amd64.deb",
		"/maven/com/example/x/1.0%2Bbuild/x-1.0%2Bbuild.jar",
		"/pypi/simple/zope.interface/",
		"/rpm/Packages/a%20b.rpm",
		"/gem/gems/x-100%25.gem",
		"/gem/gems/x-1%25zz.gem",
		// Composer archives are resolved from metadata, not from the path.
		"/composer/files/acme/lib/dev-fix%252F42/abc123.zip",
	} {
		var reached atomic.Bool
		next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached.Store(true) })
		w := httptest.NewRecorder()
		rejectDoubleEscapedPath(next).ServeHTTP(w, httptest.NewRequest(http.MethodGet, target, nil))
		if !reached.Load() {
			t.Errorf("%s: status = %d, want the request passed on", target, w.Code)
		}
	}
}
