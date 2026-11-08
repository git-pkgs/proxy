package handler

import (
	"crypto/sha1" //nolint:gosec // Maven publishes SHA-1 checksums.
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/git-pkgs/registries/fetch"
)

// mavenGenerations serves every path with the current generation's bytes,
// and a matching .sha1 for maven-metadata.xml, counting requests per path.
type mavenGenerations struct {
	gen      atomic.Int32
	requests map[string]*atomic.Int32
	// down makes every request fail with 502.
	down atomic.Bool
}

func (m *mavenGenerations) body(path string) string {
	return path + " generation " + string(rune('0'+m.gen.Load()))
}

func newMavenGenerations(t *testing.T, paths ...string) (*mavenGenerations, http.Handler) {
	t.Helper()
	m := &mavenGenerations{requests: map[string]*atomic.Int32{}}
	for _, p := range paths {
		m.requests[p] = &atomic.Int32{}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n := m.requests[r.URL.Path]; n != nil {
			n.Add(1)
		}
		if m.down.Load() {
			http.Error(w, "unavailable", http.StatusBadGateway)
			return
		}
		const metadata = "/com/example/lib/maven-metadata.xml"
		if r.URL.Path == metadata+".sha1" {
			sum := sha1.Sum([]byte(m.body(metadata))) //nolint:gosec // as above
			_, _ = io.WriteString(w, hex.EncodeToString(sum[:]))
			return
		}
		_, _ = io.WriteString(w, m.body(r.URL.Path))
	}))
	t.Cleanup(srv.Close)

	proxy, _, store, _ := setupTestProxy(t)
	store.seekable = true
	proxy.CacheMetadata = true
	proxy.MetadataTTL = time.Hour
	proxy.HTTPClient = srv.Client()
	fetcher := fetch.NewFetcher(fetch.WithHTTPClient(srv.Client()), fetch.WithMaxRetries(0))
	proxy.Fetcher = fetcher
	t.Cleanup(func() { _ = fetcher.Close() })
	return m, NewMavenHandler(proxy, "http://localhost", srv.URL, srv.URL).Routes()
}

// TestMavenMetadataChecksumMatchesServedMetadata checks that the checksum
// files of maven-metadata.xml describe the metadata the proxy serves, even
// when upstream has deployed since it was cached.
func TestMavenMetadataChecksumMatchesServedMetadata(t *testing.T) {
	m, h := newMavenGenerations(t)
	const metadata = "/com/example/lib/maven-metadata.xml"

	served := getMaven(h, metadata).Body.String()
	m.gen.Store(1)
	w := getMaven(h, metadata+".sha1")
	sum := sha1.Sum([]byte(served)) //nolint:gosec // as above
	if w.Code != http.StatusOK || w.Body.String() != hex.EncodeToString(sum[:]) {
		t.Errorf(".sha1 = %d %q, want the sha1 of the served metadata %x", w.Code, w.Body, sum)
	}
}

// TestMavenNonUniqueSnapshotNotKept checks that a SNAPSHOT file without a
// timestamp, which each build replaces, is not served from cache for good,
// while a timestamped one is.
func TestMavenNonUniqueSnapshotNotKept(t *testing.T) {
	const (
		snapshot    = "/com/example/lib/1.0-SNAPSHOT/lib-1.0-SNAPSHOT.jar"
		timestamped = "/com/example/lib/1.0-SNAPSHOT/lib-1.0-20240101.120000-1.jar"
	)
	m, h := newMavenGenerations(t, timestamped)

	getMaven(h, snapshot)
	getMaven(h, snapshot+".sha1")
	getMaven(h, timestamped)
	m.gen.Store(1)
	if w := getMaven(h, snapshot); w.Body.String() != m.body(snapshot) {
		t.Errorf("SNAPSHOT jar = %q, want the new build %q", w.Body, m.body(snapshot))
	}
	if w := getMaven(h, snapshot+".sha1"); w.Body.String() != m.body(snapshot+".sha1") {
		t.Errorf("SNAPSHOT jar checksum = %q, want the new build's", w.Body)
	}
	if w := getMaven(h, timestamped); w.Code != http.StatusOK || m.requests[timestamped].Load() != 1 {
		t.Errorf("timestamped SNAPSHOT jar fetched %d times, want once", m.requests[timestamped].Load())
	}
}

// TestMavenMutableSnapshotKeepsPolicyAndFallback checks that a SNAPSHOT file
// fetched again for each request still goes through the denylist, and falls
// back to the Gradle Plugin Portal when the primary upstream lacks it.
func TestMavenMutableSnapshotKeepsPolicyAndFallback(t *testing.T) {
	const snapshot = "/com/example/lib/1.0-SNAPSHOT/lib-1.0-SNAPSHOT.jar"
	primary := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(primary.Close)
	portal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "portal build")
	}))
	t.Cleanup(portal.Close)

	proxy, _, store, _ := setupTestProxy(t)
	store.seekable = true
	proxy.HTTPClient = primary.Client()
	fetcher := fetch.NewFetcher(fetch.WithHTTPClient(primary.Client()), fetch.WithMaxRetries(0))
	proxy.Fetcher = fetcher
	t.Cleanup(func() { _ = fetcher.Close() })
	h := NewMavenHandler(proxy, "http://localhost", primary.URL, portal.URL).Routes()

	if w := getMaven(h, snapshot); w.Code != http.StatusOK || w.Body.String() != "portal build" {
		t.Errorf("SNAPSHOT from the fallback = %d %q, want 200 portal build", w.Code, w.Body)
	}
	setTestDenylist(t, proxy, "pkg:maven/com.example/lib@1.0-SNAPSHOT")
	if w := getMaven(h, snapshot); w.Code != http.StatusForbidden {
		t.Errorf("denied SNAPSHOT = %d, want 403", w.Code)
	}
}

// TestMavenMutableSnapshotServesCacheWhenUpstreamFails checks that a
// SNAPSHOT file fetched again for each request is still served from cache
// while upstream is down, and that HEAD does not download it again.
func TestMavenMutableSnapshotServesCacheWhenUpstreamFails(t *testing.T) {
	const snapshot = "/com/example/lib/1.0-SNAPSHOT/lib-1.0-SNAPSHOT.jar"
	m, h := newMavenGenerations(t, snapshot)

	built := getMaven(h, snapshot).Body.String()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodHead, snapshot, nil))
	if w.Code != http.StatusOK || m.requests[snapshot].Load() != 1 {
		t.Errorf("HEAD = %d after %d upstream requests, want 200 from cache", w.Code, m.requests[snapshot].Load())
	}

	m.down.Store(true)
	if w := getMaven(h, snapshot); w.Code != http.StatusOK || w.Body.String() != built {
		t.Errorf("SNAPSHOT while upstream is down = %d %q, want the cached build %q", w.Code, w.Body, built)
	}
	m.down.Store(false)
	m.gen.Store(1)
	if w := getMaven(h, snapshot); w.Body.String() != m.body(snapshot) {
		t.Errorf("SNAPSHOT once upstream is back = %q, want the new build", w.Body)
	}
}
