package mirror

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/git-pkgs/registries"
)

// countingReleases counts the listings it answers.
type countingReleases struct {
	stubReleases
	calls atomic.Int32
}

func (c *countingReleases) ListReleases(ctx context.Context, ecosystem, name string) ([]registries.Version, error) {
	c.calls.Add(1)
	return c.stubReleases.ListReleases(ctx, ecosystem, name)
}

func (c *countingReleases) ListVersions(ctx context.Context, ecosystem, name string) ([]string, error) {
	c.calls.Add(1)
	return c.stubReleases.ListVersions(ctx, ecosystem, name)
}

// countingTransport counts the requests it passes on.
type countingTransport struct {
	next  http.RoundTripper
	calls atomic.Int32
}

func (c *countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	c.calls.Add(1)
	return c.next.RoundTrip(r)
}

func turnedOffFailures(p *Progress, ecosystem string) int {
	n := 0
	for _, e := range p.Errors {
		if e.Ecosystem == ecosystem && strings.Contains(e.Error, "turned off") {
			n++
		}
	}
	return n
}

func TestRefreshDoesNotListReleasesOfDisabledEcosystems(t *testing.T) {
	m, fetcher := setupRefreshMirror(t, "tarball")
	cacheNPM(t, m, "lodash", "4.17.20")
	lister := &countingReleases{stubReleases: stubReleases{"npm/lodash": oldReleases("4.17.20", "4.17.21")}}
	m.Versions = lister
	m.Disabled = func(ecosystem string) bool { return ecosystem == "npm" }
	fetcher.reset()

	progress, err := m.Refresh(context.Background(), Refresh{Top: 10, Since: 24 * time.Hour})
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if n := lister.calls.Load(); n != 0 {
		t.Errorf("listed releases %d times for a turned-off ecosystem", n)
	}
	if len(fetcher.fetched()) != 0 {
		t.Errorf("fetched %v for a turned-off ecosystem", fetcher.fetched())
	}
	if progress.Failed != 1 || turnedOffFailures(progress, "npm") != 1 {
		t.Errorf("progress = %+v, want lodash failed as turned off", progress)
	}
}

func TestRefreshDoesNotReadIndexesOfDisabledEcosystems(t *testing.T) {
	m, fetcher, idx := setupIndexRefresh(t)
	dir := "/alpine/v3.20/main/aarch64"
	idx.set(dir+"/APKINDEX.tar.gz", apkIndexArchive(t, "P:curl\nV:8.9.0-r0\nA:aarch64\n\n"))
	clientFetchesCurl(t, m, "alpine", idx.URL+dir+"/APKINDEX.tar.gz",
		"alpine", "8.9.0-r0", "edge/v3.20/main/aarch64/curl-8.9.0-r0.apk", idx.URL+dir+"/curl-8.9.0-r0.apk")
	idx.set(dir+"/APKINDEX.tar.gz", apkIndexArchive(t, "P:curl\nV:8.9.1-r0\nA:aarch64\n\n"))

	transport := &countingTransport{next: idx.Client().Transport}
	m.proxy.HTTPClient = &http.Client{Transport: transport}
	m.Disabled = func(ecosystem string) bool { return ecosystem == "alpine" }
	fetcher.reset()

	progress := refreshOnce(t, m)
	if n := transport.calls.Load(); n != 0 {
		t.Errorf("read %d index documents for a turned-off ecosystem", n)
	}
	if len(fetcher.fetched()) != 0 {
		t.Errorf("fetched %v for a turned-off ecosystem", fetcher.fetched())
	}
	if progress.Failed != 1 || turnedOffFailures(progress, "alpine") != 1 {
		t.Errorf("progress = %+v, want alpine failed as turned off", progress)
	}
}
