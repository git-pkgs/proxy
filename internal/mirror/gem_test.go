package mirror

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/git-pkgs/registries"
)

// gemBuilds returns a version of nokogiri whose artifacts are the given
// platform builds, as the RubyGems adapter lists them: one per platform, with
// "ruby" for the build that installs anywhere.
func gemBuilds(number string, age time.Duration, platforms ...string) registries.Version {
	const name = "nokogiri"
	v := registries.Version{Number: number, PublishedAt: time.Now().Add(-age)}
	for _, platform := range platforms {
		file := name + "-" + number + "-" + platform + ".gem"
		if platform == "ruby" {
			file = name + "-" + number + ".gem"
		}
		v.Artifacts = append(v.Artifacts, registries.Artifact{
			URL:      "https://rubygems.example/downloads/" + file,
			Filename: file,
			Metadata: map[string]any{"platform": platform},
		})
	}
	return v
}

func TestMirrorGemPlatformBuilds(t *testing.T) {
	tests := []struct {
		purl string
		want []string
	}{
		{"pkg:gem/nokogiri@1.13.6", []string{"nokogiri-1.13.6-arm64-darwin.gem", "nokogiri-1.13.6-x86_64-linux.gem", "nokogiri-1.13.6.gem"}},
		{"pkg:gem/nokogiri@1.13.6?platform=x86_64-linux", []string{"nokogiri-1.13.6-x86_64-linux.gem"}},
		{"pkg:gem/nokogiri@1.13.6?platform=ruby", []string{"nokogiri-1.13.6.gem"}},
		// Gemfile.lock, and SBOMs made from one, give a platform build's
		// version with the platform appended.
		{"pkg:gem/nokogiri@1.13.6-x86_64-linux", []string{"nokogiri-1.13.6-x86_64-linux.gem"}},
		{"pkg:gem/nokogiri@1.13.6-x86_64-linux?platform=x86_64-linux", []string{"nokogiri-1.13.6-x86_64-linux.gem"}},
	}
	for _, tt := range tests {
		t.Run(tt.purl, func(t *testing.T) {
			m, fetcher := setupRefreshMirror(t, "gem")
			m.Versions = stubReleases{"gem/nokogiri": {
				gemBuilds("1.13.6", 30*24*time.Hour, "ruby", "x86_64-linux", "arm64-darwin"),
			}}

			progress, err := m.Run(context.Background(), &PURLSource{PURLs: []string{tt.purl}})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if progress.Completed != 1 || progress.Failed != 0 {
				t.Errorf("progress = %+v, want one version mirrored", progress)
			}
			got := fetcher.fetched()
			slices.Sort(got)
			want := make([]string, len(tt.want))
			for i, f := range tt.want {
				want[i] = "https://rubygems.example/downloads/" + f
			}
			if !slices.Equal(got, want) {
				t.Errorf("fetched %v, want %v", got, want)
			}

			// Each build is cached where the gem handler looks for it: a
			// platform build under its version with the platform appended.
			for _, f := range tt.want {
				version := strings.TrimSuffix(strings.TrimPrefix(f, "nokogiri-"), ".gem")
				a, err := m.db.GetArtifact("pkg:gem/nokogiri@"+version, f)
				if err != nil || a == nil {
					t.Errorf("%s not cached under version %s: %v", f, version, err)
				}
			}
		})
	}
}

func TestMirrorGemPlatformMatchingNoBuild(t *testing.T) {
	m, _ := setupRefreshMirror(t, "gem")
	m.Versions = stubReleases{"gem/nokogiri": {
		gemBuilds("1.13.6", 30*24*time.Hour, "ruby", "x86_64-linux"),
	}}

	progress, err := m.Run(context.Background(), &PURLSource{PURLs: []string{"pkg:gem/nokogiri@1.13.6?platform=java"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if progress.Failed != 1 || len(progress.Errors) != 1 || !strings.Contains(progress.Errors[0].Error, "platform=java") {
		t.Errorf("progress = %+v, want a failure naming platform=java", progress)
	}
}

// A platform in the version that has no build fails under the version as
// given.
func TestMirrorGemVersionPlatformMatchingNoBuild(t *testing.T) {
	m, _ := setupRefreshMirror(t, "gem")
	m.Versions = stubReleases{"gem/nokogiri": {
		gemBuilds("1.13.6", 30*24*time.Hour, "ruby", "x86_64-linux"),
	}}

	progress, err := m.Run(context.Background(), &PURLSource{PURLs: []string{"pkg:gem/nokogiri@1.13.6-java"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if progress.Failed != 1 || len(progress.Errors) != 1 || !strings.Contains(progress.Errors[0].Error, "platform=java") ||
		progress.Errors[0].Version != "1.13.6-java" {
		t.Errorf("progress = %+v, want a failure of version 1.13.6-java naming platform=java", progress)
	}
}

// A platform in the version and a different one in the qualifier name no
// build, so the PURL is refused.
func TestMirrorGemPlatformConflictsWithVersion(t *testing.T) {
	m, fetcher := setupRefreshMirror(t, "gem")
	m.Versions = stubReleases{"gem/nokogiri": {
		gemBuilds("1.13.6", 30*24*time.Hour, "ruby", "x86_64-linux", "arm64-darwin"),
	}}

	progress, err := m.Run(context.Background(), &PURLSource{PURLs: []string{"pkg:gem/nokogiri@1.13.6-x86_64-linux?platform=arm64-darwin"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if progress.Failed != 1 || len(progress.Errors) != 1 || !strings.Contains(progress.Errors[0].Error, "platform=arm64-darwin") ||
		len(fetcher.fetched()) != 0 {
		t.Errorf("progress = %+v, fetched %v; want a failure naming platform=arm64-darwin and nothing fetched", progress, fetcher.fetched())
	}
}

// A file whose name doesn't read as a gem's is not cached, as the gem
// handler could not serve it.
func TestMirrorGemSkipsUnreadableFilename(t *testing.T) {
	m, fetcher := setupRefreshMirror(t, "gem")
	v := gemBuilds("1.13.6", 30*24*time.Hour, "ruby")
	v.Artifacts[0].Filename = "nokogiri.gem"
	m.Versions = stubReleases{"gem/nokogiri": {v}}

	progress, err := m.Run(context.Background(), &PURLSource{PURLs: []string{"pkg:gem/nokogiri@1.13.6"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if progress.Failed != 1 || len(fetcher.fetched()) != 0 {
		t.Errorf("progress = %+v, fetched %v; want a failure and nothing fetched", progress, fetcher.fetched())
	}
}

// A refresh mirrors the platforms clients fetched of an older version, and
// compares the versions without their platforms.
func TestRefreshMirrorsTheGemPlatformsClientsFetched(t *testing.T) {
	m, fetcher := setupRefreshMirror(t, "gem")
	ctx := context.Background()
	// The gem handler caches a platform build under its version with the
	// platform appended, as it reads it from the filename.
	for _, f := range [][2]string{
		{"1.13.6", "nokogiri-1.13.6.gem"},
		{"1.13.6-x86_64-linux", "nokogiri-1.13.6-x86_64-linux.gem"},
	} {
		result, err := m.proxy.GetOrFetchArtifactFromURLWithDigest(ctx, "gem", "nokogiri", f[0], f[1], "https://rubygems.example/downloads/"+f[1], "")
		if err != nil {
			t.Fatal(err)
		}
		_ = result.Reader.Close()
	}
	m.Versions = stubReleases{"gem/nokogiri": {
		gemBuilds("1.13.6", 60*24*time.Hour, "ruby", "x86_64-linux", "arm64-darwin"),
		gemBuilds("1.13.7", 30*24*time.Hour, "ruby", "x86_64-linux", "arm64-darwin", "java"),
	}}
	fetcher.reset()

	progress, err := m.Refresh(ctx, Refresh{Top: 10, Since: 24 * time.Hour})
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	got := fetcher.fetched()
	slices.Sort(got)
	want := []string{
		"https://rubygems.example/downloads/nokogiri-1.13.7-x86_64-linux.gem",
		"https://rubygems.example/downloads/nokogiri-1.13.7.gem",
	}
	if progress.Completed != 1 || progress.Failed != 0 || !slices.Equal(got, want) {
		t.Errorf("progress = %+v, fetched %v; want %v", progress, got, want)
	}

	// Once 1.13.7 is cached, a refresh has nothing left to fetch.
	fetcher.reset()
	progress, err = m.Refresh(ctx, Refresh{Top: 10, Since: 24 * time.Hour})
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if got := fetcher.fetched(); len(got) != 0 || progress.Completed+progress.Skipped != 0 {
		t.Errorf("second refresh: progress = %+v, fetched %v; want nothing to do", progress, got)
	}
}

// With only platform builds cached, a refresh still reads their version as
// the release's number: 1.13.7 follows 1.13.6-java, and once it is cached
// there is nothing left to fetch.
func TestRefreshComparesGemVersionsWithoutPlatform(t *testing.T) {
	m, fetcher := setupRefreshMirror(t, "gem")
	ctx := context.Background()
	result, err := m.proxy.GetOrFetchArtifactFromURLWithDigest(ctx, "gem", "nokogiri", "1.13.6-java", "nokogiri-1.13.6-java.gem", "https://rubygems.example/downloads/nokogiri-1.13.6-java.gem", "")
	if err != nil {
		t.Fatal(err)
	}
	_ = result.Reader.Close()
	m.Versions = stubReleases{"gem/nokogiri": {
		gemBuilds("1.13.6", 60*24*time.Hour, "ruby", "java"),
		gemBuilds("1.13.7", 30*24*time.Hour, "ruby", "java"),
	}}

	for i, want := range [][]string{{"https://rubygems.example/downloads/nokogiri-1.13.7-java.gem"}, nil} {
		fetcher.reset()
		progress, err := m.Refresh(ctx, Refresh{Top: 10, Since: 24 * time.Hour})
		if err != nil {
			t.Fatalf("Refresh: %v", err)
		}
		got := fetcher.fetched()
		if progress.Failed != 0 || progress.Skipped != 0 || !slices.Equal(got, want) {
			t.Errorf("refresh %d: progress = %+v, fetched %v; want %v", i+1, progress, got, want)
		}
	}
}
