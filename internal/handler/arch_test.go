package handler

import (
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/git-pkgs/proxy/internal/packageurl"
	"github.com/git-pkgs/registries/fetch"
)

func TestParseArchPackage(t *testing.T) {
	assertPathParser(t, "parseArchPackage", parseArchPackage, []pathParseCase{
		{"bash-5.2.037-1-x86_64.pkg.tar.zst", "bash", "5.2.037-1", "x86_64"},
		{"python-pip-24.3.1-1-any.pkg.tar.zst", "python-pip", "24.3.1-1", "any"},
		{"lz4-1:1.10.0-2-x86_64.pkg.tar.zst", "lz4", "1:1.10.0-2", "x86_64"},
		{"wpa_supplicant-2:2.12-1-x86_64.pkg.tar.zst", "wpa_supplicant", "2:2.12-1", "x86_64"},
		{"linux-aarch64-6.12.1-1-aarch64.pkg.tar.xz", "linux-aarch64", "6.12.1-1", "aarch64"},
		{"libsigc++-3.6.0-1.1-x86_64.pkg.tar.lz4", "libsigc++", "3.6.0-1.1", "x86_64"},
		{"plain-1.0-1-x86_64.pkg.tar", "plain", "1.0-1", "x86_64"},
		{"bash-5.2.037-1-x86_64.pkg.tar.zst.sig", "bash", "5.2.037-1", "x86_64"},
		{"bash-x86_64.pkg.tar.zst", "", "", ""},
		{"bash-5.2.037-1-x86_64.tar.zst", "", "", ""},
		{"core.db", "", "", ""},
	})
}

func TestArchFileClassification(t *testing.T) {
	tests := []struct {
		path                               string
		database, databaseSig, pkg, pkgSig bool
	}{
		{path: "core/os/x86_64/core.db", database: true},
		{path: "core/os/x86_64/core.db.tar.gz", database: true},
		{path: "core/os/x86_64/core.files", database: true},
		{path: "core/os/x86_64/core.files.tar.zst", database: true},
		{path: "aarch64/core/core.db", database: true},
		{path: "core/os/x86_64/core.db.sig", databaseSig: true},
		{path: "custom/os/x86_64/custom.db.tar.xz.sig", databaseSig: true},
		{path: "core/os/x86_64/bash-5.2.037-1-x86_64.pkg.tar.zst", pkg: true},
		{path: "core/os/x86_64/bash-5.2.037-1-x86_64.pkg.tar.zst.sig", pkgSig: true},
		{path: "core/os/x86_64/core.db.tar.gz.old"},
		{path: "lastupdate"},
		{path: "iso/latest/archlinux-x86_64.iso"},
	}
	for _, tt := range tests {
		if got := isArchDatabase(tt.path); got != tt.database {
			t.Errorf("isArchDatabase(%q) = %v, want %v", tt.path, got, tt.database)
		}
		if got := isArchDatabaseSignature(tt.path); got != tt.databaseSig {
			t.Errorf("isArchDatabaseSignature(%q) = %v, want %v", tt.path, got, tt.databaseSig)
		}
		if got := isArchPackage(tt.path); got != tt.pkg {
			t.Errorf("isArchPackage(%q) = %v, want %v", tt.path, got, tt.pkg)
		}
		if got := isArchPackageSignature(tt.path); got != tt.pkgSig {
			t.Errorf("isArchPackageSignature(%q) = %v, want %v", tt.path, got, tt.pkgSig)
		}
	}
}

// TestArchPURL pins the package URL type: git-pkgs/purl maps the "arch"
// ecosystem to pkg:alpm with the "arch" namespace.
func TestArchPURL(t *testing.T) {
	pkg, version := packageurl.MakeCacheStrings(archEcosystem, "lz4", "1:1.10.0-2")
	if pkg != "pkg:alpm/arch/lz4" {
		t.Errorf("package PURL = %q, want %q", pkg, "pkg:alpm/arch/lz4")
	}
	if version != "pkg:alpm/arch/lz4@1:1.10.0-2" {
		t.Errorf("version PURL = %q, want %q", version, "pkg:alpm/arch/lz4@1:1.10.0-2")
	}
}

func TestArchHandler_Routes(t *testing.T) {
	h := NewArchHandler(nil, "http://localhost:8080", nil)
	assertRoutesBasics(t, h.Routes(), "/archlinux/core/os/x86_64/core.db", "/archlinux/core/../../../etc/passwd")
}

func TestArchHandler_DefaultsToGeoMirror(t *testing.T) {
	h := NewArchHandler(nil, "http://localhost:8080", nil)
	if got := h.repositories[defaultArchRepositoryName]; got != defaultArchUpstream {
		t.Errorf("default repository = %q, want %q", got, defaultArchUpstream)
	}
	if len(h.repositories) != 1 {
		t.Errorf("repositories = %v, want only the default", h.repositories)
	}
}

func TestArchHandler_UnknownRepositoryReturns404(t *testing.T) {
	proxy, _, _, _ := setupTestProxy(t)
	h := NewArchHandler(proxy, "http://localhost:8080", map[string]string{"archlinux": "https://example.test"})

	for _, target := range []string{"/unknown/core/os/x86_64/core.db", "/archlinux", "/"} {
		if w := serveArchRequest(h, http.MethodGet, target, nil); w.Code != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404", target, w.Code)
		}
	}
}

// archUpstream is a fake pacman mirror. Files can be swapped, single paths
// broken and the mirror taken offline; requests are counted per path.
type archUpstream struct {
	*httptest.Server
	mu        sync.Mutex
	files     map[string][]byte
	modified  map[string]time.Time
	hits      map[string]int
	broken    map[string]bool
	served    func(path string) // called after a file is served, without mu
	available atomic.Bool
	auth      atomic.Value
}

func newArchUpstream(t *testing.T, files map[string][]byte) *archUpstream {
	t.Helper()
	u := &archUpstream{files: files, modified: map[string]time.Time{}, hits: map[string]int{}, broken: map[string]bool{}}
	u.available.Store(true)
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !u.available.Load() {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		u.auth.Store(r.Header.Get("Authorization"))
		u.mu.Lock()
		data, ok := u.files[r.URL.Path]
		modified := u.modified[r.URL.Path]
		broken := u.broken[r.URL.Path]
		served := u.served
		u.hits[r.URL.Path]++
		u.mu.Unlock()
		if broken {
			http.Error(w, "broken", http.StatusInternalServerError)
			return
		}
		if !ok {
			http.NotFound(w, r)
			return
		}
		if !modified.IsZero() {
			w.Header().Set("Last-Modified", modified.UTC().Format(http.TimeFormat))
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(data)
		if served != nil {
			served(r.URL.Path)
		}
	}))
	t.Cleanup(u.Close)
	return u
}

func (u *archUpstream) set(path string, data []byte) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.files[path] = data
}

func (u *archUpstream) hitCount(path string) int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.hits[path]
}

func newArchTestProxy(t *testing.T, upstream *archUpstream) *Proxy {
	t.Helper()
	proxy, _, _, _ := setupTestProxy(t)
	proxy.CacheMetadata = true
	proxy.MetadataTTL = time.Hour
	proxy.HTTPClient = upstream.Client()
	fetcher := fetch.NewFetcher(fetch.WithHTTPClient(upstream.Client()), fetch.WithMaxRetries(0))
	proxy.Fetcher = fetcher
	t.Cleanup(func() { _ = fetcher.Close() })
	return proxy
}

func assertArchBody(t *testing.T, w *httptest.ResponseRecorder, label string, want []byte) {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("%s: status = %d, want 200: %s", label, w.Code, w.Body.String())
	}
	if got := w.Body.String(); got != string(want) {
		t.Errorf("%s: body altered:\ngot  %q\nwant %q", label, got, want)
	}
}

// TestArchHandler_DatabasesServedUnchanged covers database and file-list
// downloads under the names pacman uses (core.db, core.files) and their
// .tar.* forms: bytes are served unchanged so signatures stay valid, cached
// copies are served within the TTL without contacting the upstream, and the
// cache keeps serving them when the upstream is down.
func TestArchHandler_DatabasesServedUnchanged(t *testing.T) {
	files := map[string][]byte{
		"/core/os/x86_64/core.db":            []byte("\x1f\x8b\x08core-db-bytes"),
		"/core/os/x86_64/core.db.tar.gz":     []byte("\x1f\x8b\x08core-db-bytes"),
		"/core/os/x86_64/core.db.sig":        []byte("core-db-signature"),
		"/core/os/x86_64/core.files":         []byte("\x1f\x8b\x08core-files-bytes"),
		"/core/os/x86_64/core.files.tar.zst": []byte("\x28\xb5\x2f\xfdcore-files-zst"),
	}
	upstream := newArchUpstream(t, files)
	proxy := newArchTestProxy(t, upstream)
	authorizeUpstream(proxy, "Bearer arch-token")
	h := NewArchHandler(proxy, "http://proxy.example", map[string]string{"archlinux": upstream.URL})

	// The database goes first, as pacman requests it.
	paths := []string{
		"/core/os/x86_64/core.db", "/core/os/x86_64/core.db.sig", "/core/os/x86_64/core.db.tar.gz",
		"/core/os/x86_64/core.files", "/core/os/x86_64/core.files.tar.zst",
	}
	for _, path := range paths {
		assertArchBody(t, serveArchRequest(h, http.MethodGet, "/archlinux"+path, nil), path, files[path])
	}
	if got, _ := upstream.auth.Load().(string); got != "Bearer arch-token" {
		t.Errorf("Authorization = %q, want %q", got, "Bearer arch-token")
	}

	// core.db is fetched twice on its first request: once more after its
	// signature, to make sure the two belong together.
	wantHits := map[string]int{"/core/os/x86_64/core.db": 2}
	for _, path := range paths {
		assertArchBody(t, serveArchRequest(h, http.MethodGet, "/archlinux"+path, nil), path+" cached", files[path])
		want := max(wantHits[path], 1)
		if got := upstream.hitCount(path); got != want {
			t.Errorf("%s: upstream hits = %d, want %d (second read from cache)", path, got, want)
		}
	}

	upstream.available.Store(false)
	for _, path := range paths {
		assertArchBody(t, serveArchRequest(h, http.MethodGet, "/archlinux"+path, nil), path+" offline", files[path])
	}
}

// TestArchHandler_StaleDatabaseServedWhenUpstreamDown covers the stale
// fallback after the TTL.
func TestArchHandler_StaleDatabaseServedWhenUpstreamDown(t *testing.T) {
	db := []byte("core-db-bytes")
	upstream := newArchUpstream(t, map[string][]byte{"/core/os/x86_64/core.db": db})
	proxy := newArchTestProxy(t, upstream)
	h := NewArchHandler(proxy, "http://proxy.example", map[string]string{"archlinux": upstream.URL})

	assertArchBody(t, serveArchRequest(h, http.MethodGet, "/archlinux/core/os/x86_64/core.db", nil), "seed", db)
	ageMetadata(t, proxy, h, "archlinux", upstream.URL, "core/os/x86_64/core.db")
	upstream.available.Store(false)

	w := serveArchRequest(h, http.MethodGet, "/archlinux/core/os/x86_64/core.db", nil)
	assertArchBody(t, w, "stale", db)
	if got := w.Header().Get("Warning"); got == "" {
		t.Error("stale database served without a Warning header")
	}
}

// TestArchHandler_DatabaseIfModifiedSince covers pacman's database refresh,
// which sends If-Modified-Since with the local copy's time.
func TestArchHandler_DatabaseIfModifiedSince(t *testing.T) {
	modified := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	upstream := newArchUpstream(t, map[string][]byte{"/core/os/x86_64/core.db": []byte("core-db-bytes")})
	upstream.modified["/core/os/x86_64/core.db"] = modified
	proxy := newArchTestProxy(t, upstream)
	h := NewArchHandler(proxy, "http://proxy.example", map[string]string{"archlinux": upstream.URL})

	target := "/archlinux/core/os/x86_64/core.db"
	assertArchBody(t, serveArchRequest(h, http.MethodGet, target, nil), "seed", []byte("core-db-bytes"))

	upToDate := http.Header{"If-Modified-Since": {modified.Format(http.TimeFormat)}}
	if w := serveArchRequest(h, http.MethodGet, target, upToDate); w.Code != http.StatusNotModified {
		t.Errorf("unchanged database: status = %d, want 304", w.Code)
	}
	older := http.Header{"If-Modified-Since": {modified.Add(-time.Hour).Format(http.TimeFormat)}}
	assertArchBody(t, serveArchRequest(h, http.MethodGet, target, older), "older local copy", []byte("core-db-bytes"))
}

// TestArchHandler_MissingDatabaseSignature covers the official repositories,
// which sign no databases: pacman with DatabaseOptional asks for core.db.sig
// and must get a 404. The database refresh found it missing, so the answer
// needs no upstream and holds offline too.
func TestArchHandler_MissingDatabaseSignature(t *testing.T) {
	upstream := newArchUpstream(t, map[string][]byte{"/core/os/x86_64/core.db": []byte("core-db-bytes")})
	proxy := newArchTestProxy(t, upstream)
	h := NewArchHandler(proxy, "http://proxy.example", map[string]string{"archlinux": upstream.URL})

	assertArchBody(t, serveArchRequest(h, http.MethodGet, "/archlinux/core/os/x86_64/core.db", nil), "db", []byte("core-db-bytes"))
	if w := serveArchRequest(h, http.MethodGet, "/archlinux/core/os/x86_64/core.db.sig", nil); w.Code != http.StatusNotFound {
		t.Errorf("missing signature: status = %d, want 404", w.Code)
	}
	if got := upstream.hitCount("/core/os/x86_64/core.db.sig"); got != 1 {
		t.Errorf("signature upstream hits = %d, want 1 (asked once, with the database)", got)
	}

	upstream.available.Store(false)
	assertArchBody(t, serveArchRequest(h, http.MethodGet, "/archlinux/core/os/x86_64/core.db", nil), "db offline", []byte("core-db-bytes"))
	if w := serveArchRequest(h, http.MethodGet, "/archlinux/core/os/x86_64/core.db.sig", nil); w.Code != http.StatusNotFound {
		t.Errorf("missing signature offline: status = %d, want 404", w.Code)
	}
}

// TestArchHandler_DatabaseSignatureFollowsDatabase covers custom repositories
// with signed databases. The database and its signature are separate cache
// entries; when the database is refreshed, the signature must be refreshed
// with it even inside its own TTL, or pacman would get a new database with
// the old signature. While the database is not refreshed, the cached
// signature keeps matching it and is served without asking the upstream.
func TestArchHandler_DatabaseSignatureFollowsDatabase(t *testing.T) {
	const dbPath, sigPath = "/custom/os/x86_64/custom.db", "/custom/os/x86_64/custom.db.sig"
	upstream := newArchUpstream(t, map[string][]byte{dbPath: []byte("db v1"), sigPath: []byte("sig v1")})
	proxy := newArchTestProxy(t, upstream)
	h := NewArchHandler(proxy, "http://proxy.example", map[string]string{"custom": upstream.URL})

	assertArchBody(t, serveArchRequest(h, http.MethodGet, "/custom"+dbPath, nil), "db v1", []byte("db v1"))
	assertArchBody(t, serveArchRequest(h, http.MethodGet, "/custom"+sigPath, nil), "sig v1", []byte("sig v1"))
	assertArchBody(t, serveArchRequest(h, http.MethodGet, "/custom"+sigPath, nil), "sig v1 again", []byte("sig v1"))
	if got := upstream.hitCount(sigPath); got != 1 {
		t.Errorf("signature upstream hits = %d, want 1 while the database is unchanged", got)
	}

	// Upstream publishes a new database. The cached database expires before
	// the signature does.
	upstream.set(dbPath, []byte("db v2"))
	upstream.set(sigPath, []byte("sig v2"))
	ageMetadata(t, proxy, h, "custom", upstream.URL, dbPath[1:])

	assertArchBody(t, serveArchRequest(h, http.MethodGet, "/custom"+dbPath, nil), "db v2", []byte("db v2"))
	assertArchBody(t, serveArchRequest(h, http.MethodGet, "/custom"+sigPath, nil), "sig after db refresh", []byte("sig v2"))
	assertArchBody(t, serveArchRequest(h, http.MethodGet, "/custom"+sigPath, nil), "sig v2 again", []byte("sig v2"))
	if got := upstream.hitCount(sigPath); got != 2 {
		t.Errorf("signature upstream hits = %d, want 2", got)
	}
}

// TestArchHandler_OutdatedSignatureNeverServed covers a signature fetch that
// fails while the database is refreshed. The cached signature belongs to the
// old database, and pacman rejects a present but invalid signature even with
// DatabaseOptional, so the proxy must answer 404 or 502 rather than fall back
// to it.
func TestArchHandler_OutdatedSignatureNeverServed(t *testing.T) {
	const dbPath, sigPath = "/custom/os/x86_64/custom.db", "/custom/os/x86_64/custom.db.sig"

	for _, tt := range []struct {
		name     string
		breakSig func(u *archUpstream)
		want     int
	}{
		{"signature removed upstream", func(u *archUpstream) { delete(u.files, sigPath) }, http.StatusNotFound},
		{"signature fetch fails", func(u *archUpstream) { u.broken[sigPath] = true }, http.StatusBadGateway},
	} {
		t.Run(tt.name, func(t *testing.T) {
			upstream := newArchUpstream(t, map[string][]byte{dbPath: []byte("db v1"), sigPath: []byte("sig v1")})
			proxy := newArchTestProxy(t, upstream)
			h := NewArchHandler(proxy, "http://proxy.example", map[string]string{"custom": upstream.URL})

			assertArchBody(t, serveArchRequest(h, http.MethodGet, "/custom"+dbPath, nil), "db v1", []byte("db v1"))
			assertArchBody(t, serveArchRequest(h, http.MethodGet, "/custom"+sigPath, nil), "sig v1", []byte("sig v1"))

			upstream.mu.Lock()
			upstream.files[dbPath] = []byte("db v2")
			tt.breakSig(upstream)
			upstream.mu.Unlock()
			ageMetadata(t, proxy, h, "custom", upstream.URL, dbPath[1:])
			assertArchBody(t, serveArchRequest(h, http.MethodGet, "/custom"+dbPath, nil), "db v2", []byte("db v2"))

			for i := range 2 {
				w := serveArchRequest(h, http.MethodGet, "/custom"+sigPath, nil)
				if w.Code != tt.want {
					t.Errorf("request %d: status = %d, body = %q, want %d", i, w.Code, w.Body.String(), tt.want)
				}
			}
		})
	}
}

// TestArchHandler_DelayedSignatureMatchesCachedDatabase covers a signature
// first requested after the upstream moved on: it must be the one for the
// database the proxy still serves, not the upstream's new one.
func TestArchHandler_DelayedSignatureMatchesCachedDatabase(t *testing.T) {
	const dbPath, sigPath = "/custom/os/x86_64/custom.db", "/custom/os/x86_64/custom.db.sig"
	upstream := newArchUpstream(t, map[string][]byte{dbPath: []byte("db v1"), sigPath: []byte("sig v1")})
	proxy := newArchTestProxy(t, upstream)
	h := NewArchHandler(proxy, "http://proxy.example", map[string]string{"custom": upstream.URL})

	assertArchBody(t, serveArchRequest(h, http.MethodGet, "/custom"+dbPath, nil), "db v1", []byte("db v1"))

	upstream.set(dbPath, []byte("db v2"))
	upstream.set(sigPath, []byte("sig v2"))

	assertArchBody(t, serveArchRequest(h, http.MethodGet, "/custom"+dbPath, nil), "db within TTL", []byte("db v1"))
	assertArchBody(t, serveArchRequest(h, http.MethodGet, "/custom"+sigPath, nil), "first signature request", []byte("sig v1"))
}

// TestArchHandler_RecoveredSignatureMatchesServedDatabase covers a signature
// whose fetch failed while the database was served. If the upstream has moved
// on by the time pacman asks for the signature, the new one cannot verify the
// database pacman holds, so the proxy must fail rather than serve it.
func TestArchHandler_RecoveredSignatureMatchesServedDatabase(t *testing.T) {
	const dbPath, sigPath = "/custom/os/x86_64/custom.db", "/custom/os/x86_64/custom.db.sig"
	upstream := newArchUpstream(t, map[string][]byte{dbPath: []byte("db v1"), sigPath: []byte("sig v1")})
	upstream.broken[sigPath] = true
	proxy := newArchTestProxy(t, upstream)
	h := NewArchHandler(proxy, "http://proxy.example", map[string]string{"custom": upstream.URL})

	assertArchBody(t, serveArchRequest(h, http.MethodGet, "/custom"+dbPath, nil), "db v1", []byte("db v1"))

	upstream.mu.Lock()
	upstream.files[dbPath] = []byte("db v2")
	upstream.files[sigPath] = []byte("sig v2")
	upstream.broken[sigPath] = false
	upstream.mu.Unlock()

	if w := serveArchRequest(h, http.MethodGet, "/custom"+sigPath, nil); w.Code != http.StatusBadGateway {
		t.Errorf("signature for db v1: status = %d, body = %q, want 502", w.Code, w.Body.String())
	}

	// The next sync gets the new pair.
	assertArchBody(t, serveArchRequest(h, http.MethodGet, "/custom"+dbPath, nil), "db v2", []byte("db v2"))
	assertArchBody(t, serveArchRequest(h, http.MethodGet, "/custom"+sigPath, nil), "sig v2", []byte("sig v2"))
}

// TestArchHandler_RecoveredSignatureForUnchangedDatabase covers the same
// failed signature fetch when the database has not changed: the signature is
// fetched on request and served.
func TestArchHandler_RecoveredSignatureForUnchangedDatabase(t *testing.T) {
	const dbPath, sigPath = "/custom/os/x86_64/custom.db", "/custom/os/x86_64/custom.db.sig"
	upstream := newArchUpstream(t, map[string][]byte{dbPath: []byte("db v1"), sigPath: []byte("sig v1")})
	upstream.broken[sigPath] = true
	proxy := newArchTestProxy(t, upstream)
	h := NewArchHandler(proxy, "http://proxy.example", map[string]string{"custom": upstream.URL})

	assertArchBody(t, serveArchRequest(h, http.MethodGet, "/custom"+dbPath, nil), "db v1", []byte("db v1"))
	upstream.mu.Lock()
	upstream.broken[sigPath] = false
	upstream.mu.Unlock()
	assertArchBody(t, serveArchRequest(h, http.MethodGet, "/custom"+sigPath, nil), "sig v1", []byte("sig v1"))
}

// TestArchHandler_SignatureSurvivesUnchangedRefresh covers a database refresh
// that returns the same bytes: the cached signature still matches and must be
// served offline afterwards.
func TestArchHandler_SignatureSurvivesUnchangedRefresh(t *testing.T) {
	const dbPath, sigPath = "/custom/os/x86_64/custom.db", "/custom/os/x86_64/custom.db.sig"
	upstream := newArchUpstream(t, map[string][]byte{dbPath: []byte("db v1"), sigPath: []byte("sig v1")})
	proxy := newArchTestProxy(t, upstream)
	h := NewArchHandler(proxy, "http://proxy.example", map[string]string{"custom": upstream.URL})

	assertArchBody(t, serveArchRequest(h, http.MethodGet, "/custom"+dbPath, nil), "db", []byte("db v1"))
	assertArchBody(t, serveArchRequest(h, http.MethodGet, "/custom"+sigPath, nil), "sig", []byte("sig v1"))

	ageMetadata(t, proxy, h, "custom", upstream.URL, dbPath[1:])
	assertArchBody(t, serveArchRequest(h, http.MethodGet, "/custom"+dbPath, nil), "db refreshed unchanged", []byte("db v1"))

	upstream.available.Store(false)
	assertArchBody(t, serveArchRequest(h, http.MethodGet, "/custom"+sigPath, nil), "sig offline", []byte("sig v1"))
	if got := upstream.hitCount(sigPath); got != 1 {
		t.Errorf("signature upstream hits = %d, want 1", got)
	}
}

// TestArchHandler_DatabaseReplacedDuringRefresh covers a mirror that swaps
// database and signature between the proxy's two fetches. The proxy must
// notice and store the new pair, not the old database with the new signature.
func TestArchHandler_DatabaseReplacedDuringRefresh(t *testing.T) {
	const dbPath, sigPath = "/custom/os/x86_64/custom.db", "/custom/os/x86_64/custom.db.sig"
	upstream := newArchUpstream(t, map[string][]byte{dbPath: []byte("db v1"), sigPath: []byte("sig v1")})
	var swapped atomic.Bool
	upstream.served = func(path string) {
		if path == sigPath && swapped.CompareAndSwap(false, true) {
			upstream.set(dbPath, []byte("db v2"))
			upstream.set(sigPath, []byte("sig v2"))
		}
	}
	proxy := newArchTestProxy(t, upstream)
	h := NewArchHandler(proxy, "http://proxy.example", map[string]string{"custom": upstream.URL})

	assertArchBody(t, serveArchRequest(h, http.MethodGet, "/custom"+dbPath, nil), "db", []byte("db v2"))
	upstream.available.Store(false)
	assertArchBody(t, serveArchRequest(h, http.MethodGet, "/custom"+sigPath, nil), "sig", []byte("sig v2"))
}

// TestArchHandler_RejectsQueryAndFragmentInPath covers a decoded '?' or '#'
// in the path, which would otherwise fetch one file and cache it under the
// name that follows.
func TestArchHandler_RejectsQueryAndFragmentInPath(t *testing.T) {
	upstream := newArchUpstream(t, map[string][]byte{})
	proxy := newArchTestProxy(t, upstream)
	h := NewArchHandler(proxy, "http://proxy.example", map[string]string{"archlinux": upstream.URL})

	for _, target := range []string{
		"/archlinux/core/os/x86_64/bash-5.2-1-x86_64.pkg.tar.zst%3F/foo-9-9-any.pkg.tar.zst",
		"/archlinux/core/os/x86_64/bash-5.2-1-x86_64.pkg.tar.zst%23/foo-9-9-any.pkg.tar.zst",
	} {
		if w := serveArchRequest(h, http.MethodGet, target, nil); w.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", target, w.Code)
		}
	}
	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	if len(upstream.hits) != 0 {
		t.Errorf("upstream was asked for %v", upstream.hits)
	}
}

// ageMetadata moves a metadata entry's fetch time past the one-hour TTL.
func ageMetadata(t *testing.T, proxy *Proxy, h *ArchHandler, repository, upstreamURL, path string) {
	t.Helper()
	entry, err := proxy.DB.GetMetadataCache(archEcosystem, h.metadataCacheKey(repository, upstreamURL, path))
	if err != nil || entry == nil {
		t.Fatalf("metadata entry for %s: %v", path, err)
	}
	entry.FetchedAt.Time = time.Now().Add(-2 * time.Hour)
	if err := proxy.DB.UpsertMetadataCache(entry); err != nil {
		t.Fatalf("aging metadata entry: %v", err)
	}
}

// TestArchHandler_PackagesAndSignatures covers package and signature
// downloads, cache hits, offline reads and epoch filenames (both as pacman
// sends them and percent-encoded).
func TestArchHandler_PackagesAndSignatures(t *testing.T) {
	files := map[string][]byte{
		"/core/os/x86_64/bash-5.2.037-1-x86_64.pkg.tar.zst":     []byte("bash package"),
		"/core/os/x86_64/bash-5.2.037-1-x86_64.pkg.tar.zst.sig": []byte("bash signature"),
		"/core/os/x86_64/lz4-1:1.10.0-2-x86_64.pkg.tar.zst":     []byte("lz4 package"),
		"/core/os/x86_64/lz4-1:1.10.0-2-x86_64.pkg.tar.zst.sig": []byte("lz4 signature"),
	}
	upstream := newArchUpstream(t, files)
	proxy := newArchTestProxy(t, upstream)
	h := NewArchHandler(proxy, "http://proxy.example", map[string]string{"archlinux": upstream.URL})

	for path, want := range files {
		assertArchBody(t, serveArchRequest(h, http.MethodGet, "/archlinux"+path, nil), path, want)
	}

	upstream.available.Store(false)
	for path, want := range files {
		assertArchBody(t, serveArchRequest(h, http.MethodGet, "/archlinux"+path, nil), path+" offline", want)
		if got := upstream.hitCount(path); got != 1 {
			t.Errorf("%s: upstream hits = %d, want 1", path, got)
		}
	}
	assertArchBody(t, serveArchRequest(h, http.MethodGet,
		"/archlinux/core/os/x86_64/lz4-1%3A1.10.0-2-x86_64.pkg.tar.zst", nil), "percent-encoded epoch", files["/core/os/x86_64/lz4-1:1.10.0-2-x86_64.pkg.tar.zst"])
}

// TestArchHandler_ArchLinuxARMLayout covers the Arch Linux ARM mirror layout
// ($arch/$repo), served through the same route.
func TestArchHandler_ArchLinuxARMLayout(t *testing.T) {
	files := map[string][]byte{
		"/aarch64/core/core.db":                               []byte("alarm core db"),
		"/aarch64/core/bash-5.2.037-1-aarch64.pkg.tar.xz":     []byte("alarm bash"),
		"/aarch64/core/bash-5.2.037-1-aarch64.pkg.tar.xz.sig": []byte("alarm bash signature"),
	}
	upstream := newArchUpstream(t, files)
	proxy := newArchTestProxy(t, upstream)
	h := NewArchHandler(proxy, "http://proxy.example", map[string]string{"archlinuxarm": upstream.URL})

	for path, want := range files {
		assertArchBody(t, serveArchRequest(h, http.MethodGet, "/archlinuxarm"+path, nil), path, want)
	}
	upstream.available.Store(false)
	for path, want := range files {
		assertArchBody(t, serveArchRequest(h, http.MethodGet, "/archlinuxarm"+path, nil), path+" offline", want)
	}
}

// TestArchHandler_RepositoriesDoNotShareCache checks that two repositories
// with the same path keep their own bytes, for databases and packages.
func TestArchHandler_RepositoriesDoNotShareCache(t *testing.T) {
	const dbPath, pkgPath = "/core/os/x86_64/core.db", "/core/os/x86_64/bash-5.2.037-1-x86_64.pkg.tar.zst"
	a := newArchUpstream(t, map[string][]byte{dbPath: []byte("db A"), pkgPath: []byte("pkg A")})
	b := newArchUpstream(t, map[string][]byte{dbPath: []byte("db B"), pkgPath: []byte("pkg B")})
	proxy := newArchTestProxy(t, a)
	proxy.HTTPClient = http.DefaultClient
	fetcher := fetch.NewFetcher(fetch.WithHTTPClient(http.DefaultClient), fetch.WithMaxRetries(0))
	proxy.Fetcher = fetcher
	t.Cleanup(func() { _ = fetcher.Close() })
	h := NewArchHandler(proxy, "http://proxy.example", map[string]string{"a": a.URL, "b": b.URL})

	for _, repo := range []string{"a", "b"} {
		suffix := map[string]string{"a": "A", "b": "B"}[repo]
		assertArchBody(t, serveArchRequest(h, http.MethodGet, "/"+repo+dbPath, nil), repo+" db", []byte("db "+suffix))
		assertArchBody(t, serveArchRequest(h, http.MethodGet, "/"+repo+pkgPath, nil), repo+" pkg", []byte("pkg "+suffix))
	}
}

// TestArchHandler_PackageHead covers HEAD: headers without a body. Range
// requests (pacman resuming a .part download) go through ServeArtifactRequest
// and are covered by its tests on seekable storage.
func TestArchHandler_PackageHead(t *testing.T) {
	const path = "/core/os/x86_64/bash-5.2.037-1-x86_64.pkg.tar.zst"
	pkg := []byte("0123456789 package bytes")
	upstream := newArchUpstream(t, map[string][]byte{path: pkg})
	proxy := newArchTestProxy(t, upstream)
	h := NewArchHandler(proxy, "http://proxy.example", map[string]string{"archlinux": upstream.URL})

	assertArchBody(t, serveArchRequest(h, http.MethodGet, "/archlinux"+path, nil), "seed", pkg)

	w := serveArchRequest(h, http.MethodHead, "/archlinux"+path, nil)
	if w.Code != http.StatusOK || w.Body.Len() != 0 {
		t.Errorf("HEAD: status = %d, body length = %d, want 200 and 0", w.Code, w.Body.Len())
	}
	if got := w.Header().Get("Content-Length"); got != fmt.Sprint(len(pkg)) {
		t.Errorf("HEAD Content-Length = %q, want %d", got, len(pkg))
	}
}

// TestArchHandler_PackageDownloadSendsUpstreamAuth covers upstream.auth for
// package downloads; databases are covered in
// TestArchHandler_DatabasesServedUnchanged.
func TestArchHandler_PackageDownloadSendsUpstreamAuth(t *testing.T) {
	const path = "/private/os/x86_64/tool-1.0-1-x86_64.pkg.tar.zst"
	upstream := newArchUpstream(t, map[string][]byte{path: []byte("private package")})
	proxy := newArchTestProxy(t, upstream)
	client := upstream.Client()
	client.Transport = &authRoundTripper{base: client.Transport, header: "Authorization", value: "Bearer arch-token"}
	fetcher := fetch.NewFetcher(fetch.WithHTTPClient(client), fetch.WithMaxRetries(0))
	proxy.Fetcher = fetcher
	t.Cleanup(func() { _ = fetcher.Close() })
	h := NewArchHandler(proxy, "http://proxy.example", map[string]string{"private": upstream.URL})

	assertArchBody(t, serveArchRequest(h, http.MethodGet, "/private"+path, nil), path, []byte("private package"))
	if got, _ := upstream.auth.Load().(string); got != "Bearer arch-token" {
		t.Errorf("Authorization = %q, want %q", got, "Bearer arch-token")
	}
}

// TestArchHandler_OtherFilesProxiedUncached covers files that are neither
// databases nor packages, such as lastupdate.
func TestArchHandler_OtherFilesProxiedUncached(t *testing.T) {
	upstream := newArchUpstream(t, map[string][]byte{"/lastupdate": []byte("1760000000")})
	proxy := newArchTestProxy(t, upstream)
	h := NewArchHandler(proxy, "http://proxy.example", map[string]string{"archlinux": upstream.URL})

	for i := range 2 {
		assertArchBody(t, serveArchRequest(h, http.MethodGet, "/archlinux/lastupdate", nil), fmt.Sprint("lastupdate ", i), []byte("1760000000"))
	}
	if got := upstream.hitCount("/lastupdate"); got != 2 {
		t.Errorf("upstream hits = %d, want 2 (not cached)", got)
	}
}

func serveArchRequest(h *ArchHandler, method, target string, header http.Header) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	for k, v := range header {
		req.Header[k] = v
	}
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, req)
	return w
}

// TestArchHandler_DatabaseValidatorsDescribeBody checks a fetched database
// goes out with its own Last-Modified, not that of the cache row, which
// another instance may have rewritten while the signature was fetched.
func TestArchHandler_DatabaseValidatorsDescribeBody(t *testing.T) {
	const dbPath, sigPath = "/core/os/x86_64/core.db", "/core/os/x86_64/core.db.sig"
	fetched := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	rewritten := fetched.Add(time.Hour)
	upstream := newArchUpstream(t, map[string][]byte{dbPath: []byte("core-db-v1"), sigPath: []byte("sig")})
	upstream.modified[dbPath] = fetched
	proxy := newArchTestProxy(t, upstream)
	h := NewArchHandler(proxy, "http://proxy.example", map[string]string{"archlinux": upstream.URL})
	key := h.metadataCacheKey("archlinux", upstream.URL, "core/os/x86_64/core.db")
	upstream.served = func(path string) {
		if path != sigPath {
			return
		}
		entry, err := proxy.DB.GetMetadataCache(archEcosystem, key)
		if err != nil || entry == nil {
			t.Errorf("database row while fetching the signature: %v", err)
			return
		}
		entry.LastModified = sql.NullTime{Time: rewritten, Valid: true}
		if err := proxy.DB.UpsertMetadataCache(entry); err != nil {
			t.Error(err)
		}
	}

	w := serveArchRequest(h, http.MethodGet, "/archlinux"+dbPath, nil)
	assertArchBody(t, w, "database", []byte("core-db-v1"))
	if got := w.Header().Get("Last-Modified"); got != fetched.Format(http.TimeFormat) {
		t.Errorf("Last-Modified = %q, want the fetched database's %q", got, fetched.Format(http.TimeFormat))
	}
}

// TestArchHandler_StaleDatabaseMarkedWithoutTTL checks the stale fallback
// carries the Warning header with metadata_ttl 0.
func TestArchHandler_StaleDatabaseMarkedWithoutTTL(t *testing.T) {
	db := []byte("core-db-bytes")
	upstream := newArchUpstream(t, map[string][]byte{"/core/os/x86_64/core.db": db})
	proxy := newArchTestProxy(t, upstream)
	proxy.MetadataTTL = 0
	h := NewArchHandler(proxy, "http://proxy.example", map[string]string{"archlinux": upstream.URL})

	assertArchBody(t, serveArchRequest(h, http.MethodGet, "/archlinux/core/os/x86_64/core.db", nil), "seed", db)
	upstream.available.Store(false)
	w := serveArchRequest(h, http.MethodGet, "/archlinux/core/os/x86_64/core.db", nil)
	assertArchBody(t, w, "stale", db)
	if got := w.Header().Get("Warning"); got != staleWarning {
		t.Errorf("Warning = %q, want %q", got, staleWarning)
	}
}
