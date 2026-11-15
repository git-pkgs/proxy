package retention

import (
	"slices"
	"testing"
)

func TestNewestVersions(t *testing.T) {
	tests := []struct {
		name     string
		key      string
		versions []string
		n        int
		want     []string
	}{
		{"npm orders by number, not by text", "npm", []string{"1.9.0", "1.10.0", "1.2.0"}, 2, []string{"1.10.0", "1.9.0"}},
		{"a prerelease is below its release", "npm", []string{"2.0.0-beta.1", "2.0.0", "1.0.0"}, 1, []string{"2.0.0"}},
		{"a prerelease counts as a version", "npm", []string{"2.0.0-beta.1", "1.0.0"}, 1, []string{"2.0.0-beta.1"}},
		{"go pseudo-version is below a tag", "golang", []string{"v0.0.0-20200101000000-abcdefabcdef", "v1.10.0", "v1.9.0"}, 1, []string{"v1.10.0"}},
		{"pypi post release", "pypi", []string{"1.0", "1.0.post1", "0.9"}, 1, []string{"1.0.post1"}},
		{"fewer versions than n", "cargo", []string{"0.1.0", "0.2.0"}, 5, []string{"0.2.0", "0.1.0"}},
		{"zero keeps none", "npm", []string{"1.0.0"}, 0, nil},
		{"no versions", "npm", nil, 2, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			given := slices.Clone(tt.versions)
			got := NewestVersions(VersionOrder(tt.key), tt.versions, tt.n)
			if !slices.Equal(got, tt.want) {
				t.Errorf("NewestVersions() = %v, want %v", got, tt.want)
			}
			if !slices.Equal(tt.versions, given) {
				t.Errorf("NewestVersions() reordered its input to %v", tt.versions)
			}
		})
	}
}

// Two spellings of one release count as one version, so neither is left out
// for the other.
func TestNewestVersionsCountsEqualVersionsOnce(t *testing.T) {
	want := []string{"1.0.0", "1.0"}
	for _, versions := range [][]string{{"1.0", "1.0.0", "0.9"}, {"0.9", "1.0.0", "1.0"}} {
		if got := NewestVersions(VersionOrder("pypi"), versions, 1); !slices.Equal(got, want) {
			t.Errorf("NewestVersions(%v) = %v, want %v", versions, got, want)
		}
	}
	got := NewestVersions(VersionOrder("pypi"), []string{"1.0", "1.0.0", "0.9", "0.8"}, 2)
	if want := []string{"1.0.0", "1.0", "0.9"}; !slices.Equal(got, want) {
		t.Errorf("NewestVersions() with n=2 = %v, want %v", got, want)
	}
}

func TestRegistryKeepLatestKeys(t *testing.T) {
	reg := NewRegistry()
	reg.Register(Spec{Key: "npm", CompareVersions: VersionOrder("npm")})
	reg.Register(Spec{Key: "gem"})
	reg.Register(Spec{Key: "cargo", CompareVersions: VersionOrder("cargo")})
	reg.Register(Spec{Key: "oci", KeepLatestImages: true})

	if got := reg.KeepLatestKeys(); !slices.Equal(got, []string{"cargo", "npm", "oci"}) {
		t.Errorf("KeepLatestKeys() = %v, want [cargo npm oci]", got)
	}
}
