package handler

import (
	"compress/gzip"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/git-pkgs/cooldown"
	"github.com/git-pkgs/proxy/internal/metadata/maven"
	registryclient "github.com/git-pkgs/registries/client"
	"github.com/git-pkgs/registries/fetch"
)

const mavenTestDir = "/com/example/lib/"

// mavenCooldownUpstream serves com.example:lib. Each version's .pom has the
// Last-Modified in times; pomStatus overrides the status of a version's
// .pom. Requests for .pom files are counted.
type mavenCooldownUpstream struct {
	server      *httptest.Server
	versions    []string
	latest      string
	release     string
	lastUpdated time.Time
	times       map[string]time.Time
	pomStatus   map[string]int

	mu         sync.Mutex
	poms       map[string]int
	heads      int
	userAgents []string
}

func newMavenCooldownUpstream(t *testing.T) *mavenCooldownUpstream {
	t.Helper()
	now := time.Now()
	u := &mavenCooldownUpstream{
		versions:    []string{"1.0", "1.1", "2.0", "2.1", "3.0-SNAPSHOT"},
		latest:      "3.0-SNAPSHOT",
		release:     "2.1",
		lastUpdated: now.Add(-time.Hour),
		times: map[string]time.Time{
			"1.0":          now.Add(-30 * 24 * time.Hour),
			"1.1":          now.Add(-10 * 24 * time.Hour),
			"2.0":          now.Add(-2 * time.Hour),
			"2.1":          now.Add(-time.Hour),
			"3.0-SNAPSHOT": now.Add(-time.Minute),
		},
		pomStatus: map[string]int{},
		poms:      map[string]int{},
	}
	u.server = httptest.NewServer(http.HandlerFunc(u.serve))
	t.Cleanup(u.server.Close)
	return u
}

// serve answers for the default upstream and, under /portal, for the
// Gradle Plugin Portal fallback alike.
func (u *mavenCooldownUpstream) serve(w http.ResponseWriter, r *http.Request) {
	rest, ok := strings.CutPrefix(strings.TrimPrefix(r.URL.Path, "/portal"), mavenTestDir)
	if !ok {
		http.NotFound(w, r)
		return
	}
	switch rest {
	case "maven-metadata.xml":
		_, _ = w.Write([]byte(u.metadata()))
		return
	case "maven-metadata.xml.asc":
		_, _ = w.Write([]byte("signature"))
		return
	}
	version, file, _ := strings.Cut(rest, "/")
	published, known := u.times[version]
	if !known {
		http.NotFound(w, r)
		return
	}
	if strings.HasSuffix(file, ".module") {
		w.Header().Set(headerLastModified, published.UTC().Format(http.TimeFormat))
	}
	if strings.HasSuffix(file, ".pom") {
		u.mu.Lock()
		u.poms[version]++
		if r.Method == http.MethodHead {
			u.heads++
			u.userAgents = append(u.userAgents, r.UserAgent())
		}
		u.mu.Unlock()
		w.Header().Set(headerLastModified, published.UTC().Format(http.TimeFormat))
		if status := u.pomStatus[version]; status != 0 {
			// An error page dated long ago must not count as an old version.
			w.Header().Set(headerLastModified, time.Unix(0, 0).UTC().Format(http.TimeFormat))
			if status == http.StatusTooManyRequests {
				w.Header().Set("Retry-After", "60")
			}
			w.WriteHeader(status)
			return
		}
	}
	_, _ = w.Write([]byte(file + " of " + version))
}

func (u *mavenCooldownUpstream) metadata() string {
	var b strings.Builder
	b.WriteString("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<metadata>\n  <groupId>com.example</groupId>\n  <artifactId>lib</artifactId>\n  <versioning>\n")
	b.WriteString("    <latest>" + u.latest + "</latest>\n    <release>" + u.release + "</release>\n    <versions>\n")
	for _, v := range u.versions {
		b.WriteString("      <version>" + v + "</version>\n")
	}
	b.WriteString("    </versions>\n    <lastUpdated>" + u.lastUpdated.UTC().Format(maven.LastUpdatedLayout) + "</lastUpdated>\n  </versioning>\n</metadata>\n")
	return b.String()
}

// pomRequests returns how many requests each version's .pom got.
func (u *mavenCooldownUpstream) pomRequests() map[string]int {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := make(map[string]int, len(u.poms))
	for v, n := range u.poms {
		out[v] = n
	}
	return out
}

func (u *mavenCooldownUpstream) totalPOMRequests() int {
	total := 0
	for _, n := range u.pomRequests() {
		total += n
	}
	return total
}

func newMavenCooldownHandler(t *testing.T, u *mavenCooldownUpstream) (http.Handler, *Proxy) {
	t.Helper()
	proxy, _, store, _ := setupTestProxy(t)
	store.seekable = true
	proxy.HTTPClient = u.server.Client()
	fetcher := fetch.NewFetcher(fetch.WithHTTPClient(u.server.Client()), fetch.WithMaxRetries(0))
	proxy.Fetcher = fetcher
	t.Cleanup(func() { _ = fetcher.Close() })
	proxy.Cooldown = &cooldown.Config{Default: "3d"}
	return NewMavenHandler(proxy, "http://localhost", u.server.URL, u.server.URL+"/portal").Routes(), proxy
}

func mavenMetadataVersions(t *testing.T, body string) []string {
	t.Helper()
	doc, err := maven.ParseMetadata([]byte(body))
	if err != nil {
		t.Fatalf("served metadata does not parse: %v\n%s", err, body)
	}
	var versions []string
	for _, v := range doc.Versions {
		versions = append(versions, v.Value)
	}
	return versions
}

func TestMavenCooldownFiltersMetadata(t *testing.T) {
	u := newMavenCooldownUpstream(t)
	h, proxy := newMavenCooldownHandler(t, u)

	w := getMaven(h, mavenTestDir+"maven-metadata.xml")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	body := w.Body.String()
	if got := strings.Join(mavenMetadataVersions(t, body), " "); got != "1.0 1.1 3.0-SNAPSHOT" {
		t.Errorf("versions = %s, want 1.0 1.1 3.0-SNAPSHOT", got)
	}
	if !strings.Contains(body, "<release>1.1</release>") || !strings.Contains(body, "<latest>3.0-SNAPSHOT</latest>") {
		t.Errorf("release or latest not recomputed:\n%s", body)
	}
	if strings.Contains(body, "\n\n") {
		t.Errorf("removed versions left blank lines:\n%s", body)
	}

	// The walk stops at 1.1, the first version old enough, and skips the
	// SNAPSHOT.
	want := map[string]int{"2.1": 1, "2.0": 1, "1.1": 1}
	if got := u.pomRequests(); len(got) != len(want) || got["2.1"] != 1 || got["2.0"] != 1 || got["1.1"] != 1 {
		t.Errorf("pom requests = %v, want %v", got, want)
	}

	// Times are stored, so the next request makes no .pom requests.
	ver, err := proxy.DB.GetVersionByPURL(canonicalVersionPURL("maven", "com.example:lib", "2.1"))
	if err != nil || ver == nil || !ver.PublishedAt.Valid {
		t.Fatalf("time of 2.1 not stored: %v %v", ver, err)
	}
	if w := getMaven(h, mavenTestDir+"maven-metadata.xml"); w.Body.String() != body {
		t.Errorf("second response differs:\n%s", w.Body)
	}
	if n := u.totalPOMRequests(); n != 3 {
		t.Errorf("pom requests after second metadata request = %d, want 3", n)
	}
}

func TestMavenCooldownRecomputesLatest(t *testing.T) {
	u := newMavenCooldownUpstream(t)
	u.versions = []string{"1.0", "1.1", "2.0", "2.1"}
	u.latest = "2.1"
	h, _ := newMavenCooldownHandler(t, u)

	body := getMaven(h, mavenTestDir+"maven-metadata.xml").Body.String()
	if !strings.Contains(body, "<latest>1.1</latest>") || !strings.Contains(body, "<release>1.1</release>") {
		t.Errorf("latest and release should be 1.1:\n%s", body)
	}
}

func TestMavenCooldownDropsLatestWhenNothingIsLeft(t *testing.T) {
	u := newMavenCooldownUpstream(t)
	u.versions = []string{"2.0", "2.1"}
	u.latest = "2.1"
	h, _ := newMavenCooldownHandler(t, u)

	body := getMaven(h, mavenTestDir+"maven-metadata.xml").Body.String()
	if strings.Contains(body, "<latest>") || strings.Contains(body, "<release>") || strings.Contains(body, "<version>") {
		t.Errorf("no version is out of cooldown:\n%s", body)
	}
	if !strings.Contains(body, "<versions>\n    </versions>") {
		t.Errorf("version list not emptied in place:\n%s", body)
	}
}

func TestMavenCooldownChecksumsMatchMetadata(t *testing.T) {
	u := newMavenCooldownUpstream(t)
	h, _ := newMavenCooldownHandler(t, u)

	body := getMaven(h, mavenTestDir+"maven-metadata.xml").Body.Bytes()
	for suffix, newHash := range map[string]func() hash.Hash{
		".md5": md5.New, ".sha1": sha1.New, ".sha256": sha256.New, ".sha512": sha512.New,
	} {
		sum := newHash()
		_, _ = sum.Write(body)
		w := getMaven(h, mavenTestDir+"maven-metadata.xml"+suffix)
		if w.Code != http.StatusOK || w.Body.String() != hex.EncodeToString(sum.Sum(nil)) {
			t.Errorf("%s: status = %d body %q, want checksum of served metadata", suffix, w.Code, w.Body)
		}
	}
	// A signature of the upstream metadata would not match.
	if w := getMaven(h, mavenTestDir+"maven-metadata.xml.asc"); w.Code != http.StatusNotFound {
		t.Errorf(".asc status = %d, want 404", w.Code)
	}

	// Unchanged metadata keeps its signature.
	u.lastUpdated = time.Now().Add(-4 * 24 * time.Hour)
	if w := getMaven(h, mavenTestDir+"maven-metadata.xml.asc"); w.Code != http.StatusOK || w.Body.String() != "signature" {
		t.Errorf("unchanged .asc: status = %d body %q, want upstream's", w.Code, w.Body)
	}
}

func TestMavenCooldownRefusesFiles(t *testing.T) {
	u := newMavenCooldownUpstream(t)
	h, _ := newMavenCooldownHandler(t, u)

	for _, file := range []string{"2.1/lib-2.1.pom", "2.1/lib-2.1.jar", "2.1/lib-2.1.jar.sha1", "2.0/lib-2.0-sources.jar"} {
		w := getMaven(h, mavenTestDir+file)
		if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), errMavenCooldown) {
			t.Errorf("%s: status = %d body %q, want 403 cooldown", file, w.Code, w.Body)
		}
	}
	for _, file := range []string{"1.1/lib-1.1.jar", "3.0-SNAPSHOT/lib-3.0-SNAPSHOT.jar"} {
		if w := getMaven(h, mavenTestDir+file); w.Code != http.StatusOK {
			t.Errorf("%s: status = %d, want 200", file, w.Code)
		}
	}
	// One .pom request per version, none for the SNAPSHOT.
	if got := u.pomRequests(); got["2.1"] != 1 || got["2.0"] != 1 || got["1.1"] != 1 || got["3.0-SNAPSHOT"] != 0 {
		t.Errorf("pom requests = %v", got)
	}
}

// TestMavenCooldownPOMDownloadCarriesItsTime checks that a build fetching
// pinned versions, which never reads maven-metadata.xml, costs no requests
// beyond its downloads: the .pom's own Last-Modified gives the version's
// time, and Maven fetches the .pom before the version's other files.
func TestMavenCooldownPOMDownloadCarriesItsTime(t *testing.T) {
	u := newMavenCooldownUpstream(t)
	h, proxy := newMavenCooldownHandler(t, u)

	for _, c := range []struct {
		file string
		want int
	}{
		{"2.1/lib-2.1.pom", http.StatusForbidden},
		{"2.1/lib-2.1.jar", http.StatusForbidden},
		{"1.0/lib-1.0.pom", http.StatusOK},
		{"1.0/lib-1.0.jar", http.StatusOK},
	} {
		if w := getMaven(h, mavenTestDir+c.file); w.Code != c.want {
			t.Errorf("%s: status = %d, want %d", c.file, w.Code, c.want)
		}
	}
	u.mu.Lock()
	heads := u.heads
	u.mu.Unlock()
	if heads != 0 {
		t.Errorf("HEAD requests = %d, want 0", heads)
	}
	if got := u.pomRequests(); got["2.1"] != 1 || got["1.0"] != 1 {
		t.Errorf("pom requests = %v, want one GET each", got)
	}
	ver, _ := proxy.DB.GetVersionByPURL(canonicalVersionPURL("maven", "com.example:lib", "2.1"))
	if ver == nil || !ver.PublishedAt.Valid || !ver.PublishedAt.Time.Equal(u.times["2.1"].Truncate(time.Second)) {
		t.Errorf("stored time of 2.1 = %v, want %v", ver, u.times["2.1"])
	}
}

// TestMavenCooldownModuleDownloadCarriesItsTime checks that a Gradle
// .module fetched before the .pom gives the version's time as the .pom
// would.
func TestMavenCooldownModuleDownloadCarriesItsTime(t *testing.T) {
	u := newMavenCooldownUpstream(t)
	h, _ := newMavenCooldownHandler(t, u)

	if w := getMaven(h, mavenTestDir+"2.1/lib-2.1.module"); w.Code != http.StatusForbidden {
		t.Errorf("2.1 .module: status = %d, want 403", w.Code)
	}
	if w := getMaven(h, mavenTestDir+"1.0/lib-1.0.module"); w.Code != http.StatusOK {
		t.Errorf("1.0 .module: status = %d, want 200", w.Code)
	}
	if w := getMaven(h, mavenTestDir+"1.0/lib-1.0.jar"); w.Code != http.StatusOK {
		t.Errorf("1.0 .jar: status = %d, want 200", w.Code)
	}
	if n := u.totalPOMRequests(); n != 0 {
		t.Errorf("pom requests = %d, want 0", n)
	}
}

// TestMavenCooldownTimeRequestsNameTheProxy checks the User-Agent of the
// HEAD requests for publish times: Maven Central asks tools to name
// themselves rather than send an HTTP library's default.
func TestMavenCooldownTimeRequestsNameTheProxy(t *testing.T) {
	u := newMavenCooldownUpstream(t)
	h, _ := newMavenCooldownHandler(t, u)

	getMaven(h, mavenTestDir+"maven-metadata.xml")
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.userAgents) == 0 {
		t.Fatal("no HEAD requests")
	}
	for _, ua := range u.userAgents {
		if ua != mavenTimeUserAgent {
			t.Errorf("User-Agent = %q, want %q", ua, mavenTimeUserAgent)
		}
	}
}

// TestMavenCooldownMetadataServedLikeOtherRewrites checks that filtered
// metadata is gzip-compressed for clients that accept it, as the other
// ecosystems' rewritten metadata is.
func TestMavenCooldownMetadataServedLikeOtherRewrites(t *testing.T) {
	u := newMavenCooldownUpstream(t)
	// Enough versions that the document compresses.
	for i := range 50 {
		v := fmt.Sprintf("0.%d", i)
		u.versions = append([]string{v}, u.versions...)
		u.times[v] = time.Now().Add(-100 * 24 * time.Hour)
	}
	h, proxy := newMavenCooldownHandler(t, u)
	// The server always sets up the rewrite cache, which keeps the gzip copy.
	proxy.SetMetadataRewriteCacheSize(1 << 20)

	req := httptest.NewRequest(http.MethodGet, mavenTestDir+"maven-metadata.xml", nil)
	req.Header.Set(headerAcceptEncoding, "gzip")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK || w.Header().Get(headerContentEncoding) != "gzip" {
		t.Fatalf("status = %d, Content-Encoding %q; want gzip", w.Code, w.Header().Get(headerContentEncoding))
	}
	if ct := w.Header().Get(headerContentType); !strings.Contains(ct, "xml") {
		t.Errorf("Content-Type = %q, want XML", ct)
	}
	zr, err := gzip.NewReader(w.Body)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(zr)
	if versions := mavenMetadataVersions(t, string(body)); slices.Contains(versions, "2.1") {
		t.Errorf("2.1 listed in the gzip body: %v", versions)
	}
}

func TestMavenCooldownOldLastUpdatedSkipsPOMs(t *testing.T) {
	u := newMavenCooldownUpstream(t)
	u.lastUpdated = time.Now().Add(-4 * 24 * time.Hour)
	// The times would filter 2.0 and 2.1 if they were read.
	h, _ := newMavenCooldownHandler(t, u)

	w := getMaven(h, mavenTestDir+"maven-metadata.xml")
	if w.Body.String() != u.metadata() {
		t.Errorf("metadata changed:\n%s", w.Body)
	}
	if n := u.totalPOMRequests(); n != 0 {
		t.Errorf("pom requests = %d, want 0", n)
	}
}

func TestMavenCooldownIgnoresNon2xxPOMResponses(t *testing.T) {
	u := newMavenCooldownUpstream(t)
	u.pomStatus["2.1"] = http.StatusNotFound
	h, proxy := newMavenCooldownHandler(t, u)

	// A missing .pom gives no time: 2.1 stays listed, and the walk goes on
	// to 2.0 rather than stopping at the error page's old date.
	versions := mavenMetadataVersions(t, getMaven(h, mavenTestDir+"maven-metadata.xml").Body.String())
	if got := strings.Join(versions, " "); got != "1.0 1.1 2.1 3.0-SNAPSHOT" {
		t.Errorf("versions = %s, want 1.0 1.1 2.1 3.0-SNAPSHOT", got)
	}
	if ver, _ := proxy.DB.GetVersionByPURL(canonicalVersionPURL("maven", "com.example:lib", "2.1")); ver != nil && ver.PublishedAt.Valid {
		t.Errorf("time of a 404 stored: %v", ver.PublishedAt.Time)
	}

	// Any other error fails closed.
	u.pomStatus["2.1"] = http.StatusTooManyRequests
	for _, p := range []string{"maven-metadata.xml", "maven-metadata.xml.sha1", "2.1/lib-2.1.jar"} {
		if w := getMaven(h, mavenTestDir+p); w.Code != http.StatusBadGateway {
			t.Errorf("%s: status = %d, want 502", p, w.Code)
		}
	}
	if ver, _ := proxy.DB.GetVersionByPURL(canonicalVersionPURL("maven", "com.example:lib", "2.1")); ver != nil && ver.PublishedAt.Valid {
		t.Errorf("time of a 429 stored: %v", ver.PublishedAt.Time)
	}
}

// TestMavenCooldownRateLimitedPOMIsHeld checks that a 429 on a .pom time
// request is answered like any held upstream request, with 503 and the
// upstream's Retry-After, so clients wait instead of failing the build.
func TestMavenCooldownRateLimitedPOMIsHeld(t *testing.T) {
	u := newMavenCooldownUpstream(t)
	u.pomStatus["2.0"] = http.StatusTooManyRequests
	h, proxy := newMavenCooldownHandler(t, u)
	gate := registryclient.NewRateLimitGate(u.server.Client().Transport)
	proxy.HTTPClient = &http.Client{Transport: gate}
	proxy.RateLimits = gate

	// Not the .pom, so its time is read with a HEAD.
	w := getMaven(h, mavenTestDir+"2.0/lib-2.0.jar")
	if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") == "" {
		t.Errorf("status = %d, Retry-After %q; want 503 with Retry-After", w.Code, w.Header().Get("Retry-After"))
	}
}

func TestMavenCooldownDisabledPassesThrough(t *testing.T) {
	u := newMavenCooldownUpstream(t)
	h, proxy := newMavenCooldownHandler(t, u)
	proxy.Cooldown = nil

	if w := getMaven(h, mavenTestDir+"maven-metadata.xml"); w.Body.String() != u.metadata() {
		t.Errorf("metadata changed:\n%s", w.Body)
	}
	if w := getMaven(h, mavenTestDir+"2.1/lib-2.1.jar"); w.Code != http.StatusOK {
		t.Errorf("jar status = %d, want 200", w.Code)
	}
	if n := u.totalPOMRequests(); n != 0 {
		t.Errorf("pom requests = %d, want 0", n)
	}
}

func TestMavenCooldownPackageOverride(t *testing.T) {
	u := newMavenCooldownUpstream(t)
	h, proxy := newMavenCooldownHandler(t, u)
	proxy.Cooldown = &cooldown.Config{Default: "3d", Packages: map[string]string{"pkg:maven/com.example/lib": "0"}}

	if w := getMaven(h, mavenTestDir+"maven-metadata.xml"); w.Body.String() != u.metadata() {
		t.Errorf("metadata changed:\n%s", w.Body)
	}
	if w := getMaven(h, mavenTestDir+"2.1/lib-2.1.jar"); w.Code != http.StatusOK {
		t.Errorf("jar status = %d, want 200", w.Code)
	}
	if n := u.totalPOMRequests(); n != 0 {
		t.Errorf("pom requests = %d, want 0", n)
	}
}

func TestMavenCooldownNamedRepositoryStoresItsOwnTimes(t *testing.T) {
	u := newMavenCooldownUpstream(t)
	proxy, _, _, _ := setupTestProxy(t)
	proxy.HTTPClient = u.server.Client()
	proxy.Cooldown = &cooldown.Config{Default: "3d"}
	// The named repository is the same server under /portal, which serves
	// the same files.
	h := NewMavenHandler(proxy, "http://localhost", u.server.URL, u.server.URL+"/portal").
		WithRepositories(map[string]string{"mirror": u.server.URL + "/portal"}).Routes()

	for range 2 {
		versions := mavenMetadataVersions(t, getMaven(h, "/mirror"+mavenTestDir+"maven-metadata.xml").Body.String())
		if got := strings.Join(versions, " "); got != "1.0 1.1 3.0-SNAPSHOT" {
			t.Fatalf("versions = %s, want 1.0 1.1 3.0-SNAPSHOT", got)
		}
	}
	if w := getMaven(h, "/mirror"+mavenTestDir+"2.1/lib-2.1.jar"); w.Code != http.StatusForbidden {
		t.Errorf("jar status = %d, want 403", w.Code)
	}
	if n := u.totalPOMRequests(); n != 3 {
		t.Errorf("pom requests = %d, want 3", n)
	}

	// The default repository doesn't use the named one's times.
	_ = getMaven(h, mavenTestDir+"maven-metadata.xml")
	if n := u.totalPOMRequests(); n != 6 {
		t.Errorf("pom requests after default metadata = %d, want 6", n)
	}
}
