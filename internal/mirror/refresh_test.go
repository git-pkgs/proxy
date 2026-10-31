package mirror

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/git-pkgs/cooldown"
	"github.com/git-pkgs/registries"
	"github.com/git-pkgs/registries/fetch"
)

// servingFetcher answers every fetch with body and records the URLs.
type servingFetcher struct {
	body string
	mu   sync.Mutex
	urls []string
}

func (f *servingFetcher) Fetch(ctx context.Context, url string) (*fetch.Artifact, error) {
	return f.FetchWithHeaders(ctx, url, nil)
}

func (f *servingFetcher) FetchWithHeaders(_ context.Context, url string, _ http.Header) (*fetch.Artifact, error) {
	f.mu.Lock()
	f.urls = append(f.urls, url)
	f.mu.Unlock()
	return &fetch.Artifact{Body: io.NopCloser(strings.NewReader(f.body)), Size: int64(len(f.body)), ContentType: "application/octet-stream"}, nil
}

func (f *servingFetcher) Head(context.Context, string) (int64, string, error) {
	return int64(len(f.body)), "application/octet-stream", nil
}

func (f *servingFetcher) fetched() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.urls...)
}

func (f *servingFetcher) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.urls = nil
}

// stubReleases lists releases by "ecosystem/name".
type stubReleases map[string][]registries.Version

func (s stubReleases) ListReleases(_ context.Context, ecosystem, name string) ([]registries.Version, error) {
	r, ok := s[ecosystem+"/"+name]
	if !ok {
		return nil, fmt.Errorf("%w: %s/%s", registries.ErrNotFound, ecosystem, name)
	}
	return r, nil
}

func (s stubReleases) ListVersions(ctx context.Context, ecosystem, name string) ([]string, error) {
	r, err := s.ListReleases(ctx, ecosystem, name)
	numbers := make([]string, len(r))
	for i, v := range r {
		numbers[i] = v.Number
	}
	return numbers, err
}

func (s stubReleases) FindVersion(ctx context.Context, ecosystem, name, version string) (registries.Version, error) {
	r, err := s.ListReleases(ctx, ecosystem, name)
	if err != nil {
		return registries.Version{}, err
	}
	for _, v := range r {
		if v.Number == version {
			return v, nil
		}
	}
	return registries.Version{}, registries.ErrNotFound
}

func oldReleases(numbers ...string) []registries.Version {
	r := releases(numbers...)
	for i := range r {
		r[i].PublishedAt = time.Now().Add(-time.Duration(len(r)-i) * 30 * 24 * time.Hour)
	}
	return r
}

func setupRefreshMirror(t *testing.T, body string) (*Mirror, *servingFetcher) {
	t.Helper()
	m := setupTestMirror(t, 1)
	fetcher := &servingFetcher{body: body}
	m.proxy.Fetcher = fetcher
	return m, fetcher
}

// cacheNPM downloads a version the way a client would, so it counts as used.
func cacheNPM(t *testing.T, m *Mirror, name, version string) {
	t.Helper()
	result, err := m.proxy.GetOrFetchArtifact(context.Background(), "npm", name, version, "")
	if err != nil {
		t.Fatalf("caching %s@%s: %v", name, version, err)
	}
	_ = result.Reader.Close()
}

func fetchedMatching(urls []string, part string) []string {
	var out []string
	for _, u := range urls {
		if strings.Contains(u, part) {
			out = append(out, u)
		}
	}
	return out
}

func TestRefreshMirrorsNewerVersionOnCachedLine(t *testing.T) {
	m, fetcher := setupRefreshMirror(t, "tarball")
	cacheNPM(t, m, "lodash", "4.17.20")
	m.Versions = stubReleases{"npm/lodash": oldReleases("4.17.20", "4.17.21", "5.0.0")}
	fetcher.reset()

	progress, err := m.Refresh(context.Background(), Refresh{Top: 10, Since: 24 * time.Hour})
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if progress.Completed != 1 || progress.Failed != 0 {
		t.Errorf("progress = %+v, want one mirrored", progress)
	}
	urls := fetcher.fetched()
	if len(fetchedMatching(urls, "lodash-4.17.21.tgz")) != 1 || len(fetchedMatching(urls, "5.0.0")) != 0 {
		t.Errorf("fetched %v, want lodash 4.17.21 only", urls)
	}

	// The new version is cached now, so the next run has nothing to do.
	progress, err = m.Refresh(context.Background(), Refresh{Top: 10, Since: 24 * time.Hour})
	if err != nil {
		t.Fatalf("second Refresh: %v", err)
	}
	if progress.Completed != 0 || progress.Failed != 0 {
		t.Errorf("second run progress = %+v, want nothing to do", progress)
	}
}

func TestRefreshKeepsCooldown(t *testing.T) {
	m, fetcher := setupRefreshMirror(t, "tarball")
	cacheNPM(t, m, "lodash", "4.17.20")
	r := oldReleases("4.17.20", "4.17.21")
	r[1].PublishedAt = time.Now().Add(-time.Hour)
	m.Versions = stubReleases{"npm/lodash": r}
	m.proxy.Cooldown = &cooldown.Config{Default: "3d"}
	fetcher.reset()

	progress, err := m.Refresh(context.Background(), Refresh{Top: 10, Since: 24 * time.Hour})
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if progress.Completed != 0 || len(fetcher.fetched()) != 0 {
		t.Errorf("progress = %+v, fetched %v; want a version within cooldown left alone", progress, fetcher.fetched())
	}
}

func TestRefreshOnlyTopPackagesRecentlyUsed(t *testing.T) {
	m, fetcher := setupRefreshMirror(t, "tarball")
	cacheNPM(t, m, "lodash", "4.17.20")
	cacheNPM(t, m, "react", "18.3.0")
	cacheNPM(t, m, "react", "18.3.0") // a second download: react is used most
	m.Versions = stubReleases{
		"npm/lodash": oldReleases("4.17.20", "4.17.21"),
		"npm/react":  oldReleases("18.3.0", "18.3.1"),
	}
	if err := m.db.RecordArtifactHit("pkg:npm/react@18.3.0", "react-18.3.0.tgz"); err != nil {
		t.Fatal(err)
	}
	fetcher.reset()

	progress, err := m.Refresh(context.Background(), Refresh{Top: 1, Since: 24 * time.Hour})
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	urls := fetcher.fetched()
	if progress.Completed != 1 || len(fetchedMatching(urls, "react-18.3.1.tgz")) != 1 || len(fetchedMatching(urls, "lodash")) != 0 {
		t.Errorf("progress = %+v, fetched %v; want only react refreshed", progress, urls)
	}
}

func TestRefreshStopsAtMaxVersions(t *testing.T) {
	m, fetcher := setupRefreshMirror(t, "tarball")
	cacheNPM(t, m, "lodash", "4.17.20")
	cacheNPM(t, m, "react", "18.3.0")
	m.Versions = stubReleases{
		"npm/lodash": oldReleases("4.17.20", "4.17.21"),
		"npm/react":  oldReleases("18.3.0", "18.3.1"),
	}
	fetcher.reset()

	progress, err := m.Refresh(context.Background(), Refresh{Top: 10, Since: 24 * time.Hour, MaxVersions: 1})
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if progress.Completed != 1 || len(fetcher.fetched()) != 1 {
		t.Errorf("progress = %+v, fetched %v; want one version", progress, fetcher.fetched())
	}
}

func TestRefreshStopsAtMaxBytes(t *testing.T) {
	m, fetcher := setupRefreshMirror(t, "0123456789")
	cacheNPM(t, m, "lodash", "4.17.20")
	cacheNPM(t, m, "react", "18.3.0")
	m.Versions = stubReleases{
		"npm/lodash": oldReleases("4.17.20", "4.17.21"),
		"npm/react":  oldReleases("18.3.0", "18.3.1"),
	}
	fetcher.reset()

	// The cap is checked before each download, so the first one goes over it.
	progress, err := m.Refresh(context.Background(), Refresh{Top: 10, Since: 24 * time.Hour, MaxBytes: 5})
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if progress.Completed != 1 || progress.Bytes != 10 {
		t.Errorf("progress = %+v, want one version of 10 bytes", progress)
	}
}

func TestRefreshCountsListingFailure(t *testing.T) {
	m, fetcher := setupRefreshMirror(t, "tarball")
	cacheNPM(t, m, "lodash", "4.17.20")
	m.Versions = stubReleases{}
	fetcher.reset()

	progress, err := m.Refresh(context.Background(), Refresh{Top: 10, Since: 24 * time.Hour})
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if progress.Failed != 1 || len(progress.Errors) != 1 || progress.Errors[0].Name != "lodash" {
		t.Errorf("progress = %+v, want lodash failed", progress)
	}
}

func TestRefreshMirrorsTheFileVariantsClientsFetched(t *testing.T) {
	m, fetcher := setupRefreshMirror(t, "wheel")
	ctx := context.Background()
	for _, f := range []string{"demo-1.0.0-py3-none-any.whl", "demo-1.0.0-cp312-cp312-manylinux_2_17_x86_64.whl"} {
		result, err := m.proxy.GetOrFetchArtifactFromURLWithDigest(ctx, "pypi", "demo", "1.0.0", f, "https://files.example/"+f, "")
		if err != nil {
			t.Fatal(err)
		}
		_ = result.Reader.Close()
	}
	files := func(version string, names ...string) registries.Version {
		v := registries.Version{Number: version, PublishedAt: time.Now().Add(-30 * 24 * time.Hour)}
		for _, n := range names {
			v.Artifacts = append(v.Artifacts, registries.Artifact{Filename: n, URL: "https://files.example/" + n})
		}
		return v
	}
	m.Versions = stubReleases{"pypi/demo": {
		files("1.0.0", "demo-1.0.0-py3-none-any.whl", "demo-1.0.0-cp312-cp312-manylinux_2_17_x86_64.whl"),
		files("1.1.0",
			"demo-1.1.0.tar.gz",
			"demo-1.1.0-py3-none-any.whl",
			"demo-1.1.0-cp312-cp312-manylinux_2_17_x86_64.whl",
			"demo-1.1.0-cp313-cp313-manylinux_2_17_x86_64.whl",
			"demo-1.1.0-cp312-cp312-win_amd64.whl"),
	}}
	fetcher.reset()

	progress, err := m.Refresh(ctx, Refresh{Top: 10, Since: 24 * time.Hour})
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	got := fetcher.fetched()
	slices.Sort(got)
	want := []string{
		"https://files.example/demo-1.1.0-cp312-cp312-manylinux_2_17_x86_64.whl",
		"https://files.example/demo-1.1.0-py3-none-any.whl",
	}
	if progress.Completed != 1 || !slices.Equal(got, want) {
		t.Errorf("progress = %+v, fetched %v; want %v", progress, got, want)
	}
}

func TestFileVariants(t *testing.T) {
	tests := []struct {
		ecosystem, cacheFilename, want string
	}{
		{"pypi", "demo-1.0.0-py3-none-any.whl", "py3-none-any"},
		{"pypi", "demo-1.0.0-1-cp312-cp312-manylinux_2_17_x86_64.manylinux2014_x86_64.whl", "cp312-cp312-manylinux_2_17_x86_64.manylinux2014_x86_64"},
		{"pypi", "demo-1.0.0.tar.gz", "sdist"},
		{"pypi", "demo-1.0.0.zip", "sdist"},
		{"conda", "linux-64/numpy-1.26.4-py312h8753938_0.conda", "linux-64/py312/conda"},
		{"conda", "noarch/requests-2.32.3-pyhd8ed1ab_0.conda", "noarch/py/conda"},
		{"conda", "osx-arm64/zlib-1.3.1-h8359307_2.tar.bz2", "osx-arm64//tar.bz2"},
	}
	for _, tt := range tests {
		if got := fileEcosystems[tt.ecosystem].cachedVariant(tt.cacheFilename); got != tt.want {
			t.Errorf("%s variant of %q = %q, want %q", tt.ecosystem, tt.cacheFilename, got, tt.want)
		}
	}
}
