package mirror

import (
	"reflect"
	"testing"
	"time"

	"github.com/git-pkgs/registries"
)

func releases(numbers ...string) []registries.Version {
	out := make([]registries.Version, len(numbers))
	for i, n := range numbers {
		out[i] = registries.Version{Number: n, PublishedAt: time.Date(2026, 1, 1+i, 0, 0, 0, 0, time.UTC)}
	}
	return out
}

func TestNewestPerCachedLine(t *testing.T) {
	allowAll := func(registries.Version) bool { return true }
	yanked := releases("4.17.20", "4.17.21", "4.17.22")
	yanked[2].Status = registries.StatusYanked

	tests := []struct {
		name     string
		cached   []string
		releases []registries.Version
		allowed  func(registries.Version) bool
		want     []string
	}{
		{name: "newer patch on the cached line", cached: []string{"4.17.20"},
			releases: releases("4.17.20", "4.17.21", "5.0.0"), want: []string{"4.17.21"}},
		{name: "newest already cached", cached: []string{"4.17.21"},
			releases: releases("4.17.20", "4.17.21", "5.0.0")},
		{name: "one per cached major line", cached: []string{"1.2.0", "2.0.0"},
			releases: releases("1.2.0", "1.3.0", "2.0.0", "2.1.0", "3.0.0"), want: []string{"1.3.0", "2.1.0"}},
		{name: "zero major lines are minor lines", cached: []string{"0.3.1"},
			releases: releases("0.3.1", "0.3.2", "0.4.0"), want: []string{"0.3.2"}},
		{name: "prereleases skipped", cached: []string{"2.0.0"},
			releases: releases("2.0.0", "2.1.0-beta.1")},
		{name: "yanked skipped", cached: []string{"4.17.20"},
			releases: yanked, want: []string{"4.17.21"}},
		{name: "newest withheld, older newer one taken", cached: []string{"4.17.20"},
			releases: releases("4.17.20", "4.17.21", "4.17.22"),
			allowed:  func(v registries.Version) bool { return v.Number != "4.17.22" },
			want:     []string{"4.17.21"}},
		{name: "nothing allowed newer than cached", cached: []string{"4.17.20"},
			releases: releases("4.17.20", "4.17.21"),
			allowed:  func(v registries.Version) bool { return v.Number == "4.17.20" }},
		{name: "cached newer than every listed one", cached: []string{"4.17.25"},
			releases: releases("4.17.20", "4.17.21")},
		{name: "only the newest cached of a line counts", cached: []string{"4.17.19", "4.17.21"},
			releases: releases("4.17.19", "4.17.20", "4.17.21")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			allowed := tt.allowed
			if allowed == nil {
				allowed = allowAll
			}
			got := newestPerCachedLine("npm", tt.cached, tt.releases, allowed)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("newestPerCachedLine = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestNewestPerCachedLineUsesEcosystemOrdering(t *testing.T) {
	// Maven orders 1.0.10 after 1.0.9, and a gem's 1.0.0.rc1 is a prerelease.
	got := newestPerCachedLine("maven", []string{"1.0.9"}, releases("1.0.9", "1.0.10"), func(registries.Version) bool { return true })
	if !reflect.DeepEqual(got, []string{"1.0.10"}) {
		t.Errorf("maven: got %v, want [1.0.10]", got)
	}
	got = newestPerCachedLine("gem", []string{"1.0.0"}, releases("1.0.0", "1.0.1.rc1"), func(registries.Version) bool { return true })
	if got != nil {
		t.Errorf("gem: got %v, want nothing", got)
	}
}
