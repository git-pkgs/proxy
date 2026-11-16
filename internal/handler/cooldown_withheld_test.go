package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/git-pkgs/cooldown"
	"github.com/git-pkgs/proxy/internal/database"
)

// withheldTestProxy returns a proxy with a database and a three day cooldown,
// and the times of a version old enough to pass it and one it holds back.
func withheldTestProxy(t *testing.T) (proxy *Proxy, db *database.DB, old, recent time.Time) {
	t.Helper()
	proxy, db, _, _ = setupTestProxy(t)
	proxy.Cooldown = &cooldown.Config{Default: "3d"}
	now := time.Now().UTC().Truncate(time.Second)
	return proxy, db, now.Add(-10 * 24 * time.Hour), now.Add(-time.Hour)
}

// assertWithheldRecorded checks that a version a filter held back is stored
// with its publish time, under a package row the package page finds by
// ecosystem and name.
func assertWithheldRecorded(t *testing.T, db *database.DB, ecosystem, name, version string, published time.Time) {
	t.Helper()
	pkg, err := db.GetPackageByEcosystemName(ecosystem, name)
	if err != nil || pkg == nil {
		t.Fatalf("package %s/%s = %v, %v; want a row", ecosystem, name, pkg, err)
	}
	ver, err := db.GetVersionByPURL(canonicalVersionPURL(ecosystem, name, version))
	if err != nil || ver == nil {
		t.Fatalf("version %s = %v, %v; want a row", canonicalVersionPURL(ecosystem, name, version), ver, err)
	}
	if ver.PackagePURL != pkg.PURL {
		t.Errorf("version's package = %q, want %q", ver.PackagePURL, pkg.PURL)
	}
	if !ver.PublishedAt.Valid || !ver.PublishedAt.Time.Equal(published) {
		t.Errorf("version %s published %v, want %v", version, ver.PublishedAt, published)
	}
}

// assertNotRecorded checks that a version the filter let through got no row:
// only held back versions are recorded.
func assertNotRecorded(t *testing.T, db *database.DB, ecosystem, name, version string) {
	t.Helper()
	ver, err := db.GetVersionByPURL(canonicalVersionPURL(ecosystem, name, version))
	if err != nil {
		t.Fatalf("GetVersionByPURL: %v", err)
	}
	if ver != nil {
		t.Errorf("version %s recorded although cooldown let it through", version)
	}
}

func TestRecordWithheldNeedsADatabase(t *testing.T) {
	proxy := testProxy()
	proxy.Cooldown = &cooldown.Config{Default: "3d"}
	proxy.recordWithheld("npm", "leftpad", "1.0.0", time.Now())
}

func TestRecordWithheldSkipsUnknownTimes(t *testing.T) {
	proxy, db, _, _ := withheldTestProxy(t)
	proxy.recordWithheld("npm", "leftpad", "1.0.0", time.Time{})
	if pkg, err := db.GetPackageByEcosystemName("npm", "leftpad"); err != nil || pkg != nil {
		t.Errorf("package = %v, %v; want nothing recorded without a time", pkg, err)
	}
}

// Without a version the version PURL would be the package's own.
func TestRecordWithheldSkipsVersionsWithoutAName(t *testing.T) {
	proxy, db, _, recent := withheldTestProxy(t)
	proxy.recordWithheld("npm", "leftpad", "", recent)
	if pkg, err := db.GetPackageByEcosystemName("npm", "leftpad"); err != nil || pkg != nil {
		t.Errorf("package = %v, %v; want nothing recorded without a version", pkg, err)
	}
}

func TestNPMMetadataRecordsWithheldVersions(t *testing.T) {
	proxy, db, old, recent := withheldTestProxy(t)
	h := &NPMHandler{proxy: proxy, proxyURL: "http://localhost:8080"}
	input := `{"name":"testpkg","dist-tags":{"latest":"2.0.0"},
		"time":{"1.0.0":"` + old.Format(time.RFC3339) + `","2.0.0":"` + recent.Format(time.RFC3339) + `"},
		"versions":{
			"1.0.0":{"name":"testpkg","version":"1.0.0","dist":{"tarball":"https://registry.npmjs.org/testpkg/-/testpkg-1.0.0.tgz"}},
			"2.0.0":{"name":"testpkg","version":"2.0.0","dist":{"tarball":"https://registry.npmjs.org/testpkg/-/testpkg-2.0.0.tgz"}}}}`
	if _, _, err := h.rewriteMetadataKeeping("testpkg", []byte(input)); err != nil {
		t.Fatalf("rewriteMetadataKeeping: %v", err)
	}
	assertWithheldRecorded(t, db, "npm", "testpkg", "2.0.0", recent)
	assertNotRecorded(t, db, "npm", "testpkg", "1.0.0")
}

func TestCargoIndexRecordsWithheldVersions(t *testing.T) {
	proxy, db, old, recent := withheldTestProxy(t)
	h := &CargoHandler{proxy: proxy, proxyURL: "http://localhost:8080"}
	line := func(version string, at time.Time) string {
		return `{"name":"serde","vers":"` + version + `","cksum":"abcd","features":{},"yanked":false,"pubtime":"` + at.Format(time.RFC3339) + `"}` + "\n"
	}
	h.applyCooldownFiltering(httptest.NewRecorder(), []byte(line("1.0.0", old)+line("1.0.1", recent)))
	assertWithheldRecorded(t, db, "cargo", "serde", "1.0.1", recent)
	assertNotRecorded(t, db, "cargo", "serde", "1.0.0")
}

func TestComposerMetadataRecordsWithheldVersions(t *testing.T) {
	proxy, db, old, recent := withheldTestProxy(t)
	h := &ComposerHandler{proxy: proxy, proxyURL: "http://localhost:8080"}
	input := `{"packages":{"symfony/console":[
		{"version":"5.0.0","time":"` + old.Format(time.RFC3339) + `","dist":{"url":"https://repo.packagist.org/5.0.0.zip","type":"zip"}},
		{"version":"6.0.0","time":"` + recent.Format(time.RFC3339) + `","dist":{"url":"https://repo.packagist.org/6.0.0.zip","type":"zip"}}]}}`
	if _, _, err := h.rewriteMetadataKeeping([]byte(input)); err != nil {
		t.Fatalf("rewriteMetadataKeeping: %v", err)
	}
	assertWithheldRecorded(t, db, "composer", "symfony/console", "6.0.0", recent)
	assertNotRecorded(t, db, "composer", "symfony/console", "5.0.0")
}

func TestPubMetadataRecordsWithheldVersions(t *testing.T) {
	proxy, db, old, recent := withheldTestProxy(t)
	h := &PubHandler{proxy: proxy, proxyURL: "http://localhost:8080"}
	input := `{"name":"flutter_bloc","latest":{"version":"2.0.0"},"versions":[
		{"version":"1.0.0","published":"` + old.Format(time.RFC3339) + `","archive_url":"https://pub.dev/1.0.0.tar.gz"},
		{"version":"2.0.0","published":"` + recent.Format(time.RFC3339) + `","archive_url":"https://pub.dev/2.0.0.tar.gz"}]}`
	if _, err := h.rewriteMetadata("flutter_bloc", []byte(input)); err != nil {
		t.Fatalf("rewriteMetadata: %v", err)
	}
	assertWithheldRecorded(t, db, "pub", "flutter_bloc", "2.0.0", recent)
	assertNotRecorded(t, db, "pub", "flutter_bloc", "1.0.0")
}

func TestTimedDocumentsRecordWithheldVersions(t *testing.T) {
	proxy, db, old, recent := withheldTestProxy(t)
	docs := timedDocuments{published: map[string]time.Time{"1.0.0": old, "2.0.0": recent, "3.0.0": {}}}
	for _, v := range []string{"1.0.0", "2.0.0", "3.0.0", "4.0.0"} {
		docs.withheld(proxy, "hex", "testpkg", v)
	}
	assertWithheldRecorded(t, db, "hex", "testpkg", "2.0.0", recent)
	for _, v := range []string{"1.0.0", "3.0.0", "4.0.0"} {
		assertNotRecorded(t, db, "hex", "testpkg", v)
	}
}

func TestGemIndexRecordsWithheldVersions(t *testing.T) {
	proxy, db, old, recent := withheldTestProxy(t)
	versionsJSON, _ := json.Marshal([]gemVersion{
		{Number: "1.0.0", Platform: "ruby", CreatedAt: old.Format(time.RFC3339)},
		{Number: "2.0.0", Platform: "ruby", CreatedAt: recent.Format(time.RFC3339)},
	})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/info/"):
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte("---\n1.0.0 dep1:>= 1.0|checksum:abc123\n2.0.0 dep1:>= 1.0|checksum:def456\n"))
		case strings.HasPrefix(r.URL.Path, "/api/v1/versions/"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(versionsJSON)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()
	h := &GemHandler{proxy: proxy, upstreamURL: upstream.URL, proxyURL: "http://proxy.local"}

	req := httptest.NewRequest(http.MethodGet, "/info/testgem", nil)
	req.SetPathValue("name", "testgem")
	w := httptest.NewRecorder()
	h.handleCompactIndex(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	assertWithheldRecorded(t, db, "gem", "testgem", "2.0.0", recent)
	assertNotRecorded(t, db, "gem", "testgem", "1.0.0")
}

// Go stores the publish time of every version it reads, so what the filter
// adds is the package row.
func TestGoListRecordsWithheldVersions(t *testing.T) {
	u := newGoCooldownUpstream(t, "v1.1.0")
	h, _, proxy := goCooldownHandler(t, u)
	if w := goGet(h, "/example.com/mod/@v/list"); w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	assertPackageRow(t, proxy.DB, "golang", "example.com/mod")
}

// A .info request for a version in cooldown is refused, and the version is
// recorded with the publish time the refusal was based on.
func TestGoRefusedInfoRecordsWithheldVersion(t *testing.T) {
	u := newGoCooldownUpstream(t, "v1.1.0")
	h, _, proxy := goCooldownHandler(t, u)
	if w := goGet(h, "/example.com/mod/@v/v1.1.0.info"); w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
	ver, err := proxy.DB.GetVersionByPURL(canonicalVersionPURL("golang", "example.com/mod", "v1.1.0"))
	if err != nil || ver == nil || !ver.PublishedAt.Valid {
		t.Fatalf("version = %v, %v; want it stored with its publish time", ver, err)
	}
	assertPackageRow(t, proxy.DB, "golang", "example.com/mod")
}

func assertPackageRow(t *testing.T, db *database.DB, ecosystem, name string) {
	t.Helper()
	if pkg, err := db.GetPackageByEcosystemName(ecosystem, name); err != nil || pkg == nil {
		t.Fatalf("package %s/%s = %v, %v; want a row", ecosystem, name, pkg, err)
	}
}

func TestMavenMetadataRecordsWithheldVersions(t *testing.T) {
	u := newMavenCooldownUpstream(t)
	h, proxy := newMavenCooldownHandler(t, u)
	if w := getMaven(h, mavenTestDir+"maven-metadata.xml"); w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	for _, v := range []string{"2.0", "2.1"} {
		assertWithheldRecorded(t, proxy.DB, "maven", "com.example:lib", v, u.times[v].UTC().Truncate(time.Second))
	}
}

func TestMavenNamedRepositoryRecordsNoWithheldVersions(t *testing.T) {
	u := newMavenCooldownUpstream(t)
	proxy, db, _, _ := setupTestProxy(t)
	proxy.HTTPClient = u.server.Client()
	proxy.Cooldown = &cooldown.Config{Default: "3d"}
	h := NewMavenHandler(proxy, "http://localhost", u.server.URL, u.server.URL+"/portal").
		WithRepositories(map[string]string{"mirror": u.server.URL + "/portal"}).Routes()
	if w := getMaven(h, "/mirror"+mavenTestDir+"maven-metadata.xml"); w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	if pkg, err := db.GetPackageByEcosystemName("maven", "com.example:lib"); err != nil || pkg != nil {
		t.Errorf("package = %v, %v; want no row from a named repository", pkg, err)
	}
}

func TestPyPISimpleRecordsWithheldVersions(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	old, recent := now.Add(-30*24*time.Hour), now.Add(-time.Hour)
	h, proxy := setupPyPIHandler(t, func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/simple/ruff/":
			body := `{"meta":{"api-version":"1.4"},"name":"ruff","files":[` +
				`{"filename":"ruff-1.0.0.tar.gz","url":"https://files.pythonhosted.org/packages/ab/ruff-1.0.0.tar.gz","upload-time":"` + old.Format(time.RFC3339) + `"},` +
				`{"filename":"ruff-2.0.0.tar.gz","url":"https://files.pythonhosted.org/packages/cd/ruff-2.0.0.tar.gz","upload-time":"` + recent.Format(time.RFC3339) + `"}]}`
			return pypiHTTPResponse(r, pypiSimpleJSON, body), nil
		case "/pypi/ruff/json":
			body := `{"releases":{"1.0.0":[{"upload_time_iso_8601":"` + old.Format(time.RFC3339) + `"}],` +
				`"2.0.0":[{"upload_time_iso_8601":"` + recent.Format(time.RFC3339) + `"}]}}`
			return pypiHTTPResponse(r, "application/json", body), nil
		default:
			t.Fatalf("unexpected upstream path: %s", r.URL.Path)
			return nil, nil
		}
	})
	proxy.Cooldown = &cooldown.Config{Default: "7d"}

	req := httptest.NewRequest(http.MethodGet, "/simple/ruff/", nil)
	req.Header.Set("Accept", uvPyPIAccept)
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	assertWithheldRecorded(t, proxy.DB, "pypi", "ruff", "2.0.0", recent)
	assertNotRecorded(t, proxy.DB, "pypi", "ruff", "1.0.0")
}

// The JSON API filters releases on its own path, so a client that reads it
// rather than the simple index gets the held back version recorded too.
func TestPyPIJSONRecordsWithheldVersions(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	old, recent := now.Add(-30*24*time.Hour), now.Add(-time.Hour)
	h, proxy := setupPyPIHandler(t, func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/pypi/ruff/json" {
			t.Fatalf("unexpected upstream path: %s", r.URL.Path)
		}
		body := `{"info":{"name":"ruff","version":"1.0.0"},"releases":{` +
			`"1.0.0":[{"filename":"ruff-1.0.0.tar.gz","url":"https://files.pythonhosted.org/packages/ab/ruff-1.0.0.tar.gz","upload_time_iso_8601":"` + old.Format(time.RFC3339) + `"}],` +
			`"2.0.0":[{"filename":"ruff-2.0.0.tar.gz","url":"https://files.pythonhosted.org/packages/cd/ruff-2.0.0.tar.gz","upload_time_iso_8601":"` + recent.Add(-time.Minute).Format(time.RFC3339) + `"},` +
			`{"filename":"ruff-2.0.0-py3-none-any.whl","url":"https://files.pythonhosted.org/packages/ef/ruff-2.0.0-py3-none-any.whl","upload_time_iso_8601":"` + recent.Format(time.RFC3339) + `"}]}}`
		return pypiHTTPResponse(r, "application/json", body), nil
	})
	proxy.Cooldown = &cooldown.Config{Default: "7d"}

	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/pypi/ruff/json", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), `"2.0.0"`) {
		t.Fatalf("2.0.0 was served although it is in cooldown")
	}
	// A release's time is its newest file's.
	assertWithheldRecorded(t, proxy.DB, "pypi", "ruff", "2.0.0", recent)
	assertNotRecorded(t, proxy.DB, "pypi", "ruff", "1.0.0")
}

// A release's time is its newest file's, and files can be added to a release
// after it was first held back, so the newer time replaces the stored one.
func TestPyPIWithheldReleaseKeepsItsNewestUploadTime(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	first, added := now.Add(-2*time.Hour), now.Add(-time.Hour)
	files := []string{`{"filename":"ruff-2.0.0.tar.gz","url":"https://files.pythonhosted.org/packages/cd/ruff-2.0.0.tar.gz","upload_time_iso_8601":"` + first.Format(time.RFC3339) + `"}`}
	h, proxy := setupPyPIHandler(t, func(r *http.Request) (*http.Response, error) {
		body := `{"info":{"name":"ruff","version":"2.0.0"},"releases":{"2.0.0":[` + strings.Join(files, ",") + `]}}`
		return pypiHTTPResponse(r, "application/json", body), nil
	})
	proxy.Cooldown = &cooldown.Config{Default: "7d"}
	get := func() {
		t.Helper()
		w := httptest.NewRecorder()
		h.Routes().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/pypi/ruff/json", nil))
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", w.Code, w.Body)
		}
	}

	get()
	assertWithheldRecorded(t, proxy.DB, "pypi", "ruff", "2.0.0", first)
	files = append(files, `{"filename":"ruff-2.0.0-py3-none-any.whl","url":"https://files.pythonhosted.org/packages/ef/ruff-2.0.0-py3-none-any.whl","upload_time_iso_8601":"`+added.Format(time.RFC3339)+`"}`)
	get()
	assertWithheldRecorded(t, proxy.DB, "pypi", "ruff", "2.0.0", added)
}

// The simple index and the version JSON also keep the newest upload time.
func TestPyPIWithheldReleaseKeepsItsNewestUploadTimeOnEveryPath(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	first, added := now.Add(-2*time.Hour), now.Add(-time.Hour)
	for name, path := range map[string]string{"simple": "/simple/ruff/", "version json": "/pypi/ruff/2.0.0/json"} {
		t.Run(name, func(t *testing.T) {
			times := []time.Time{first}
			files := func(key string) string {
				var out []string
				for i, at := range times {
					out = append(out, fmt.Sprintf(`{"filename":"ruff-2.0.0-%d.tar.gz","url":"https://files.pythonhosted.org/packages/cd/ruff-2.0.0-%d.tar.gz",%q:%q}`, i, i, key, at.Format(time.RFC3339)))
				}
				return strings.Join(out, ",")
			}
			h, proxy := setupPyPIHandler(t, func(r *http.Request) (*http.Response, error) {
				switch r.URL.Path {
				case "/simple/ruff/":
					return pypiHTTPResponse(r, pypiSimpleJSON, `{"meta":{"api-version":"1.4"},"name":"ruff","files":[`+files("upload-time")+`]}`), nil
				case "/pypi/ruff/json":
					return pypiHTTPResponse(r, "application/json", `{"releases":{"2.0.0":[`+files("upload_time_iso_8601")+`]}}`), nil
				default:
					return pypiHTTPResponse(r, "application/json", `{"info":{"name":"ruff","version":"2.0.0"},"urls":[`+files("upload_time_iso_8601")+`]}`), nil
				}
			})
			proxy.Cooldown = &cooldown.Config{Default: "7d"}
			get := func() {
				t.Helper()
				req := httptest.NewRequest(http.MethodGet, path, nil)
				req.Header.Set("Accept", uvPyPIAccept)
				w := httptest.NewRecorder()
				h.Routes().ServeHTTP(w, req)
				if w.Code != http.StatusOK {
					t.Fatalf("status = %d: %s", w.Code, w.Body)
				}
			}
			get()
			assertWithheldRecorded(t, proxy.DB, "pypi", "ruff", "2.0.0", first)
			times = append(times, added)
			get()
			assertWithheldRecorded(t, proxy.DB, "pypi", "ruff", "2.0.0", added)
		})
	}
}

// A download that read an older document than one already recorded leaves
// the newer upload time, so cooldown doesn't end early.
func TestPyPIDownloadKeepsTheNewerStoredUploadTime(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	older, newer := now.Add(-2*time.Hour), now.Add(-time.Hour)
	h, proxy := setupPyPIHandler(t, func(r *http.Request) (*http.Response, error) {
		return pypiHTTPResponse(r, "application/json",
			`{"releases":{"2.0.0":[{"filename":"ruff-2.0.0.tar.gz","upload_time_iso_8601":"`+older.Format(time.RFC3339)+`"}]}}`), nil
	})
	proxy.Cooldown = &cooldown.Config{Default: "7d"}
	proxy.recordWithheldPyPI("ruff", "2.0.0", newer)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if inCooldown, err := h.versionInCooldown(req, "ruff", "2.0.0"); err != nil || !inCooldown {
		t.Fatalf("versionInCooldown = %v, %v; want true", inCooldown, err)
	}
	assertWithheldRecorded(t, proxy.DB, "pypi", "ruff", "2.0.0", newer)
}

// A download that read an upload time out of cooldown is still refused when
// a newer time, inside it, was stored meanwhile.
func TestPyPIDownloadDecidesWithTheNewerStoredUploadTime(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	older, newer := now.Add(-30*24*time.Hour), now.Add(-time.Hour)
	h, proxy := setupPyPIHandler(t, func(r *http.Request) (*http.Response, error) {
		return pypiHTTPResponse(r, "application/json",
			`{"releases":{"2.0.0":[{"filename":"ruff-2.0.0.tar.gz","upload_time_iso_8601":"`+older.Format(time.RFC3339)+`"}]}}`), nil
	})
	proxy.Cooldown = &cooldown.Config{Default: "7d"}
	proxy.recordWithheldPyPI("ruff", "2.0.0", newer)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if inCooldown, err := h.versionInCooldown(req, "ruff", "2.0.0"); err != nil || !inCooldown {
		t.Errorf("versionInCooldown = %v, %v; want true from the newer stored time", inCooldown, err)
	}
}

// The version JSON names one release, in urls, and records it when it is
// held back.
func TestPyPIVersionJSONRecordsWithheldVersion(t *testing.T) {
	recent := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	h, proxy := setupPyPIHandler(t, func(r *http.Request) (*http.Response, error) {
		body := `{"info":{"name":"ruff","version":"2.0.0"},"urls":[` +
			`{"filename":"ruff-2.0.0.tar.gz","url":"https://files.pythonhosted.org/packages/cd/ruff-2.0.0.tar.gz","upload_time_iso_8601":"` + recent.Format(time.RFC3339) + `"}]}`
		return pypiHTTPResponse(r, "application/json", body), nil
	})
	proxy.Cooldown = &cooldown.Config{Default: "7d"}

	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/pypi/ruff/2.0.0/json", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), "ruff-2.0.0.tar.gz") {
		t.Fatalf("the files of 2.0.0 were served although it is in cooldown")
	}
	assertWithheldRecorded(t, proxy.DB, "pypi", "ruff", "2.0.0", recent)
}

// The NuGet listing and the registration each find the held back version.
func TestNuGetMetadataRecordsWithheldVersions(t *testing.T) {
	for _, path := range []string{
		"/nuget/v3-flatcontainer/testpkg/index.json",
		"/nuget" + nugetRegistrationPath + "testpkg/index.json",
	} {
		t.Run(path, func(t *testing.T) {
			proxy, db, _, _ := setupTestProxy(t)
			proxy.Cooldown = &cooldown.Config{Default: "14d"}
			metadataRequests := 0
			upstream := newNuGetCooldownUpstream(t, &metadataRequests)
			defer upstream.Close()
			h := NewNuGetHandlerWithUpstreams(proxy, "http://proxy.test", upstream.URL, upstream.URL)
			nugetGet(t, http.StripPrefix("/nuget", h.Routes()), path, http.StatusOK)

			assertPackageRow(t, db, "nuget", "testpkg")
			ver, err := db.GetVersionByPURL(canonicalVersionPURL("nuget", "testpkg", "2.0.0"))
			if err != nil || ver == nil || !ver.PublishedAt.Valid {
				t.Fatalf("2.0.0 = %+v, %v; want a row with its publish time", ver, err)
			}
			if age := time.Since(ver.PublishedAt.Time); age < 47*time.Hour || age > 49*time.Hour {
				t.Errorf("2.0.0 published %v ago, want two days", age)
			}
			assertNotRecorded(t, db, "nuget", "testpkg", "1.0.0")
		})
	}
}

// Without a name the PURLs would name no package, such as pkg:cargo/.
func TestRecordWithheldSkipsPackagesWithoutAName(t *testing.T) {
	proxy, db, _, recent := withheldTestProxy(t)
	proxy.recordWithheld("cargo", "", "1.0.0", recent)
	if pkg, err := db.GetPackageByEcosystemName("cargo", ""); err != nil || pkg != nil {
		t.Errorf("package = %v, %v; want nothing recorded without a name", pkg, err)
	}
}

// A routed package's times come from its route and are never stored under
// the default registry's PURLs, so the filter doesn't record them either.
func TestNPMMetadataOfRoutedPackageRecordsNothing(t *testing.T) {
	proxy, db, old, recent := withheldTestProxy(t)
	h := &NPMHandler{proxy: proxy, proxyURL: "http://localhost:8080",
		routes: newPackageRoutes(map[string]string{"@corp/*": "https://npm.corp.example"})}
	input := `{"name":"@corp/lib","dist-tags":{"latest":"2.0.0"},
		"time":{"1.0.0":"` + old.Format(time.RFC3339) + `","2.0.0":"` + recent.Format(time.RFC3339) + `"},
		"versions":{
			"1.0.0":{"name":"@corp/lib","version":"1.0.0","dist":{"tarball":"https://npm.corp.example/@corp/lib/-/lib-1.0.0.tgz"}},
			"2.0.0":{"name":"@corp/lib","version":"2.0.0","dist":{"tarball":"https://npm.corp.example/@corp/lib/-/lib-2.0.0.tgz"}}}}`
	if _, _, err := h.rewriteMetadataKeeping("@corp/lib", []byte(input)); err != nil {
		t.Fatalf("rewriteMetadataKeeping: %v", err)
	}
	assertNotRecorded(t, db, "npm", "@corp/lib", "2.0.0")
}

func TestComposerMetadataOfRoutedPackageRecordsNothing(t *testing.T) {
	proxy, db, old, recent := withheldTestProxy(t)
	h := &ComposerHandler{proxy: proxy, proxyURL: "http://localhost:8080",
		routes: newPackageRoutes(map[string]string{"corp/*": "https://composer.corp.example"})}
	input := `{"packages":{"corp/lib":[
		{"version":"5.0.0","time":"` + old.Format(time.RFC3339) + `","dist":{"url":"https://composer.corp.example/5.0.0.zip","type":"zip"}},
		{"version":"6.0.0","time":"` + recent.Format(time.RFC3339) + `","dist":{"url":"https://composer.corp.example/6.0.0.zip","type":"zip"}}]}}`
	if _, _, err := h.rewriteMetadataKeeping([]byte(input)); err != nil {
		t.Fatalf("rewriteMetadataKeeping: %v", err)
	}
	assertNotRecorded(t, db, "composer", "corp/lib", "6.0.0")
}

// Go keeps publish times under the escaped module path, which differs from
// the module path downloads are stored under when it has capitals. Recording
// such a module would give one release two version rows, so it is skipped.
func TestGoListOfModuleWithCapitalsRecordsNoSecondRow(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/example.com/!mod/@v/list":
			_, _ = w.Write([]byte("v1.1.0\n"))
		case "/example.com/!mod/@v/v1.1.0.info":
			_ = json.NewEncoder(w).Encode(goInfo{Version: "v1.1.0", Time: now.Add(-time.Hour)})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(upstream.Close)
	proxy, db, _, _ := setupTestProxy(t)
	proxy.Cooldown = &cooldown.Config{Default: "3d"}
	h := NewGoHandlerWithUpstream(proxy, "http://localhost:8080", upstream.URL).Routes()
	if w := goGet(h, "/example.com/!mod/@v/list"); w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	if ver, err := db.GetVersionByPURL(canonicalVersionPURL("golang", "example.com/!mod", "v1.1.0")); err != nil || ver == nil {
		t.Fatalf("time under the escaped path = %v, %v; want it stored", ver, err)
	}
	assertNotRecorded(t, db, "golang", "example.com/Mod", "v1.1.0")
}

// A registration leaf carries no version of its own; it is the one its path
// names.
func TestNuGetRegistrationLeafRecordsWithheldVersion(t *testing.T) {
	proxy, db, _, _ := setupTestProxy(t)
	proxy.Cooldown = &cooldown.Config{Default: "14d"}
	metadataRequests := 0
	upstream := newNuGetCooldownUpstream(t, &metadataRequests)
	defer upstream.Close()
	h := NewNuGetHandlerWithUpstreams(proxy, "http://proxy.test", upstream.URL, upstream.URL)
	nugetGet(t, http.StripPrefix("/nuget", h.Routes()), "/nuget"+nugetRegistrationPath+"testpkg/2.0.0.json", http.StatusNotFound)

	ver, err := db.GetVersionByPURL(canonicalVersionPURL("nuget", "testpkg", "2.0.0"))
	if err != nil || ver == nil || !ver.PublishedAt.Valid {
		t.Fatalf("2.0.0 = %+v, %v; want a row with its publish time", ver, err)
	}
}

func TestNuGetRegistrationLeafVersion(t *testing.T) {
	for path, want := range map[string]string{
		nugetRegistrationPath + "testpkg/2.0.0.json":            "2.0.0",
		nugetRegistrationPath + "testpkg/index.json":            "",
		nugetRegistrationPath + "testpkg/page/1.0.0/2.0.0.json": "",
		nugetRegistrationPath + "testpkg/2.0.0":                 "",
		"/other/testpkg/2.0.0.json":                             "",
	} {
		if got := nugetRegistrationLeafVersion(path); got != want {
			t.Errorf("nugetRegistrationLeafVersion(%q) = %q, want %q", path, got, want)
		}
	}
}
