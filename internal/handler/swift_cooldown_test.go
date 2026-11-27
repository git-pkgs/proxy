package handler

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/git-pkgs/cooldown"
	"github.com/git-pkgs/proxy/internal/packageurl"
	"github.com/git-pkgs/registries/fetch"
)

const swiftCooldownArchive = "swift source archive"

// swiftCooldownUpstream serves apple/example with an old 1.0.0 and a recent
// 1.1.0. Without times, release documents leave publishedAt out, as the
// default upstream does.
type swiftCooldownUpstream struct {
	server *httptest.Server
	times  map[string]time.Time
	mu     sync.Mutex
	hits   map[string]int
	fail   bool
}

func (u *swiftCooldownUpstream) count(path string) int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.hits[path]
}

func newSwiftCooldownUpstream(t *testing.T, withTimes bool) *swiftCooldownUpstream {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Second)
	u := &swiftCooldownUpstream{hits: map[string]int{}, times: map[string]time.Time{}}
	if withTimes {
		u.times["1.0.0"] = now.Add(-10 * 24 * time.Hour)
		u.times["1.1.0"] = now.Add(-time.Hour)
	}
	digest := sha256.Sum256([]byte(swiftCooldownArchive))
	checksum := hex.EncodeToString(digest[:])
	u.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		u.hits[r.URL.Path]++
		fail := u.fail
		u.mu.Unlock()
		if fail {
			http.Error(w, "down", http.StatusInternalServerError)
			return
		}
		rest, ok := strings.CutPrefix(r.URL.Path, "/apple/example")
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch rest {
		case "":
			w.Header().Set("Link", `<`+u.server.URL+`/apple/example/1.1.0>; rel="latest-version"`)
			_, _ = fmt.Fprintf(w, `{"releases":{"1.0.0":{"url":"%[1]s/apple/example/1.0.0"},"1.1.0":{"url":"%[1]s/apple/example/1.1.0"}}}`, u.server.URL)
		case "/1.0.0", "/1.1.0":
			version := rest[1:]
			release := map[string]any{
				"id": "apple.example", "version": version,
				"resources": []any{map[string]any{"name": "source-archive", "type": "application/zip", "checksum": checksum}},
			}
			if published, ok := u.times[version]; ok {
				release["publishedAt"] = published.Format(time.RFC3339)
			}
			_ = json.NewEncoder(w).Encode(release)
		case "/1.0.0/Package.swift", "/1.1.0/Package.swift":
			w.Header().Set("Content-Type", "text/x-swift")
			_, _ = w.Write([]byte("// swift-tools-version:5.9\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(u.server.Close)
	return u
}

func swiftCooldownHandler(t *testing.T, u *swiftCooldownUpstream) (http.Handler, *Proxy, *mockFetcher) {
	t.Helper()
	proxy, _, _, fetcher := setupTestProxy(t)
	proxy.Cooldown = &cooldown.Config{Default: "3d"}
	fetcher.artifact = &fetch.Artifact{
		Body:        io.NopCloser(strings.NewReader(swiftCooldownArchive)),
		Size:        int64(len(swiftCooldownArchive)),
		ContentType: "application/zip",
	}
	return NewSwiftHandler(proxy, "https://proxy.example", u.server.URL).Routes(), proxy, fetcher
}

func swiftGet(h http.Handler, path, accept string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Accept", accept)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func swiftListed(t *testing.T, w *httptest.ResponseRecorder) []string {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("release list status = %d: %s", w.Code, w.Body.String())
	}
	var doc struct {
		Releases map[string]json.RawMessage `json:"releases"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	var versions []string
	for v := range doc.Releases {
		versions = append(versions, v)
	}
	slices.Sort(versions)
	return versions
}

func TestSwiftCooldownFiltersReleaseList(t *testing.T) {
	u := newSwiftCooldownUpstream(t, true)
	h, proxy, _ := swiftCooldownHandler(t, u)

	w := swiftGet(h, "/apple/example", swiftAcceptJSON)
	if got, want := swiftListed(t, w), []string{"1.0.0"}; !slices.Equal(got, want) {
		t.Errorf("listed = %v, want %v", got, want)
	}
	if link := w.Header().Get("Link"); strings.Contains(link, "latest-version") {
		t.Errorf("Link = %q, still points latest-version at a release in cooldown", link)
	}
	assertSwiftWithheldRecorded(t, proxy, "1.1.0", u.times["1.1.0"])

	// The times are stored, so the next list doesn't read the release again.
	before := u.count("/apple/example/1.1.0")
	swiftGet(h, "/apple/example", swiftAcceptJSON)
	if got := u.count("/apple/example/1.1.0"); got != before {
		t.Errorf("release 1.1.0 fetched %d more times for a second list", got-before)
	}
}

func assertSwiftWithheldRecorded(t *testing.T, proxy *Proxy, version string, published time.Time) {
	t.Helper()
	packagePURL, versionPURL := packageurl.MakeCacheStrings("swift", "apple/example", version)
	pkg, err := proxy.DB.GetPackageByEcosystemName("swift", "apple/example")
	if err != nil || pkg == nil || pkg.PURL != packagePURL {
		t.Fatalf("package = %+v, %v; want a row under %s", pkg, err, packagePURL)
	}
	ver, err := proxy.DB.GetVersionByPURL(versionPURL)
	if err != nil || ver == nil {
		t.Fatalf("version %s = %v, %v", versionPURL, ver, err)
	}
	if ver.PackagePURL != packagePURL || !ver.PublishedAt.Valid || !ver.PublishedAt.Time.Equal(published) {
		t.Errorf("version = %+v, want package %s published %v", ver, packagePURL, published)
	}
}

func TestSwiftCooldownRefusesReleaseInCooldown(t *testing.T) {
	u := newSwiftCooldownUpstream(t, true)
	h, _, fetcher := swiftCooldownHandler(t, u)

	for _, tc := range []struct{ path, accept string }{
		{"/apple/example/1.1.0", swiftAcceptJSON},
		{"/apple/example/1.1.0.json", swiftAcceptJSON},
		{"/apple/example/1.1.0/Package.swift", swiftAcceptManifest},
		{"/apple/example/1.1.0.zip", swiftAcceptArchive},
		{"/Apple/Example/1.1.0.zip", swiftAcceptArchive},
	} {
		w := swiftGet(h, tc.path, tc.accept)
		if w.Code != http.StatusForbidden {
			t.Errorf("%s: status = %d, want 403; body %s", tc.path, w.Code, w.Body.String())
			continue
		}
		if ct := w.Header().Get("Content-Type"); ct != "application/problem+json" {
			t.Errorf("%s: Content-Type = %q", tc.path, ct)
		}
		if !strings.Contains(w.Body.String(), "cooldown: pkg:swift/apple/example@1.1.0") {
			t.Errorf("%s: body = %s, want it to name the release in cooldown", tc.path, w.Body.String())
		}
	}
	if fetcher.fetchCalled {
		t.Error("archive of a release in cooldown was fetched")
	}

	for _, tc := range []struct{ path, accept string }{
		{"/apple/example/1.0.0", swiftAcceptJSON},
		{"/apple/example/1.0.0/Package.swift", swiftAcceptManifest},
		{"/apple/example/1.0.0.zip", swiftAcceptArchive},
	} {
		if w := swiftGet(h, tc.path, tc.accept); w.Code != http.StatusOK {
			t.Errorf("%s: status = %d, want 200; body %s", tc.path, w.Code, w.Body.String())
		}
	}
}

func TestSwiftCooldownLetsReleasesWithoutTimeThrough(t *testing.T) {
	u := newSwiftCooldownUpstream(t, false)
	h, _, _ := swiftCooldownHandler(t, u)

	w := swiftGet(h, "/apple/example", swiftAcceptJSON)
	if got, want := swiftListed(t, w), []string{"1.0.0", "1.1.0"}; !slices.Equal(got, want) {
		t.Errorf("listed = %v, want %v", got, want)
	}
	if !strings.Contains(w.Header().Get("Link"), "latest-version") {
		t.Errorf("Link = %q, lost latest-version", w.Header().Get("Link"))
	}
	if got := u.count("/apple/example/1.0.0"); got != 0 {
		t.Errorf("release 1.0.0 fetched %d times: the newest release had no time, so none is checked", got)
	}
	if w := swiftGet(h, "/apple/example/1.1.0.zip", swiftAcceptArchive); w.Code != http.StatusOK {
		t.Errorf("archive status = %d, want 200; body %s", w.Code, w.Body.String())
	}
}

func TestSwiftCooldownFailsClosed(t *testing.T) {
	u := newSwiftCooldownUpstream(t, true)
	h, _, fetcher := swiftCooldownHandler(t, u)
	u.fail = true

	if w := swiftGet(h, "/apple/example/1.1.0/Package.swift", swiftAcceptManifest); w.Code != http.StatusBadGateway {
		t.Errorf("manifest status = %d, want 502", w.Code)
	}
	if w := swiftGet(h, "/apple/example/1.1.0.zip", swiftAcceptArchive); w.Code != http.StatusBadGateway {
		t.Errorf("archive status = %d, want 502", w.Code)
	}
	if fetcher.fetchCalled {
		t.Error("archive fetched although its time couldn't be read")
	}
}

func TestSwiftCooldownExemptPackageIsServedAsBefore(t *testing.T) {
	u := newSwiftCooldownUpstream(t, true)
	h, proxy, _ := swiftCooldownHandler(t, u)
	proxy.Cooldown = &cooldown.Config{Default: "3d", Packages: map[string]string{"pkg:swift/apple/example": "0"}}

	if got, want := swiftListed(t, swiftGet(h, "/apple/example", swiftAcceptJSON)), []string{"1.0.0", "1.1.0"}; !slices.Equal(got, want) {
		t.Errorf("listed = %v, want %v", got, want)
	}
	if got := u.count("/apple/example/1.1.0"); got != 0 {
		t.Errorf("release fetched %d times for an exempt package", got)
	}
	if w := swiftGet(h, "/apple/example/1.1.0/Package.swift", swiftAcceptManifest); w.Code != http.StatusOK {
		t.Errorf("manifest status = %d, want 200", w.Code)
	}
}
