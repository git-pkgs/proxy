package mirror

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/git-pkgs/registries"
)

// cachedFile is a file a client downloaded: the version and filename the
// download handler cached it under.
type cachedFile struct{ version, filename string }

// refreshRetries caches cached of name the way a client would, lists
// releases, and runs three refreshes: the first with the URLs containing
// failing failing, then two without. It checks the first fails one version
// and returns what the second and third fetched.
func refreshRetries(t *testing.T, ecosystem, name string, cached []cachedFile, releases []registries.Version, failing string) (second, third []string) {
	t.Helper()
	m, fetcher := setupRefreshMirror(t, "file")
	ctx := context.Background()
	for _, f := range cached {
		result, err := m.proxy.GetOrFetchArtifactFromURLWithDigest(ctx, ecosystem, name, f.version, f.filename, "https://files.example/"+f.filename, "")
		if err != nil {
			t.Fatal(err)
		}
		_ = result.Reader.Close()
	}
	for i := range releases {
		for j := range releases[i].Artifacts {
			releases[i].Artifacts[j].URL = "https://files.example/" + releases[i].Artifacts[j].Filename
		}
	}
	m.Versions = stubReleases{ecosystem + "/" + name: releases}

	refresh := func() (Progress, []string) {
		fetcher.reset()
		progress, err := m.Refresh(ctx, Refresh{Top: 10, Since: 24 * time.Hour})
		if err != nil {
			t.Fatalf("Refresh: %v", err)
		}
		got := fetcher.fetched()
		slices.Sort(got)
		return *progress, got
	}
	fetcher.failing(failing)
	if progress, got := refresh(); progress.Failed != 1 {
		t.Fatalf("first refresh: progress = %+v, fetched %v; want one failure", progress, got)
	}
	fetcher.failing("")
	progress, second := refresh()
	if progress.Failed != 0 {
		t.Errorf("second refresh: progress = %+v, want no failures", progress)
	}
	_, third = refresh()
	return second, third
}

func release(number string, age time.Duration, files ...registries.Artifact) registries.Version {
	return registries.Version{Number: number, PublishedAt: time.Now().Add(-age), Artifacts: files}
}

const (
	olderRelease = 60 * 24 * time.Hour
	newerRelease = 30 * 24 * time.Hour
)

func TestRefreshRetriesTheMissingFilesOfARelease(t *testing.T) {
	conda := func(subdir, filename string) registries.Artifact {
		return registries.Artifact{Filename: filename, Metadata: map[string]any{"subdir": subdir, "build": "h5f976f7_0"}}
	}
	gem := func(filename, platform string) registries.Artifact {
		return registries.Artifact{Filename: filename, Metadata: map[string]any{"platform": platform}}
	}
	tests := []struct {
		name, ecosystem, pkg string
		cached               []cachedFile
		releases             []registries.Version
		failing              string
	}{
		{
			name: "pypi", ecosystem: "pypi", pkg: "requests",
			cached: []cachedFile{{"2.32.3", "requests-2.32.3.tar.gz"}, {"2.32.3", "requests-2.32.3-py3-none-any.whl"}},
			releases: []registries.Version{
				release("2.32.3", olderRelease, wheels("requests-2.32.3.tar.gz", "requests-2.32.3-py3-none-any.whl")...),
				release("2.32.4", newerRelease, wheels("requests-2.32.4.tar.gz", "requests-2.32.4-py3-none-any.whl")...),
			},
			failing: "requests-2.32.4-py3-none-any.whl",
		},
		{
			name: "conda", ecosystem: "conda", pkg: "conda-forge/zlib",
			cached: []cachedFile{{"1.3.0", "linux-64/zlib-1.3.0-h5f976f7_0.conda"}, {"1.3.0", "osx-arm64/zlib-1.3.0-h5f976f7_0.conda"}},
			releases: []registries.Version{
				release("1.3.0", olderRelease, conda("linux-64", "zlib-1.3.0-h5f976f7_0.conda"), conda("osx-arm64", "zlib-1.3.0-h5f976f7_0.conda")),
				release("1.3.1", newerRelease, conda("linux-64", "zlib-1.3.1-h5f976f7_0.conda"), conda("osx-arm64", "zlib-1.3.1-h99e199e_0.conda")),
			},
			failing: "zlib-1.3.1-h99e199e_0.conda",
		},
		{
			name: "gem", ecosystem: "gem", pkg: "nokogiri",
			cached: []cachedFile{{"1.13.6", "nokogiri-1.13.6.gem"}, {"1.13.6-x86_64-linux", "nokogiri-1.13.6-x86_64-linux.gem"}},
			releases: []registries.Version{
				release("1.13.6", olderRelease, gem("nokogiri-1.13.6.gem", "ruby"), gem("nokogiri-1.13.6-x86_64-linux.gem", "x86_64-linux")),
				release("1.13.7", newerRelease, gem("nokogiri-1.13.7.gem", "ruby"), gem("nokogiri-1.13.7-x86_64-linux.gem", "x86_64-linux")),
			},
			failing: "nokogiri-1.13.7-x86_64-linux.gem",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			second, third := refreshRetries(t, tt.ecosystem, tt.pkg, tt.cached, tt.releases, tt.failing)
			if want := []string{"https://files.example/" + tt.failing}; !slices.Equal(second, want) {
				t.Errorf("second refresh fetched %v, want %v", second, want)
			}
			if len(third) != 0 {
				t.Errorf("third refresh fetched %v, want nothing", third)
			}
		})
	}
}

// A release missing only kinds of files it doesn't publish, or files the
// download handler wouldn't cache, has nothing to retry, so a refresh
// doesn't look it up again.
func TestRefreshLeavesACompleteReleaseAlone(t *testing.T) {
	m, fetcher := setupRefreshMirror(t, "gem")
	ctx := context.Background()
	for _, f := range []cachedFile{{"1.13.6-java", "nokogiri-1.13.6-java.gem"}, {"1.13.7", "nokogiri-1.13.7.gem"}} {
		result, err := m.proxy.GetOrFetchArtifactFromURLWithDigest(ctx, "gem", "nokogiri", f.version, f.filename, "https://rubygems.example/downloads/"+f.filename, "")
		if err != nil {
			t.Fatal(err)
		}
		_ = result.Reader.Close()
	}
	newest := gemBuilds("1.13.7", newerRelease, "ruby", "arm64-darwin")
	newest.Artifacts = append(newest.Artifacts, registries.Artifact{
		URL: "https://rubygems.example/downloads/unreadable", Filename: "unreadable", Metadata: map[string]any{"platform": "ruby"},
	})
	m.Versions = stubReleases{"gem/nokogiri": {gemBuilds("1.13.6", olderRelease, "ruby", "java"), newest}}
	fetcher.reset()

	progress, err := m.Refresh(ctx, Refresh{Top: 10, Since: 24 * time.Hour})
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if got := fetcher.fetched(); len(got) != 0 || progress.Total != 0 {
		t.Errorf("progress = %+v, fetched %v; want nothing to do", progress, got)
	}
}
