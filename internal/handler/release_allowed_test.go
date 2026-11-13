package handler

import (
	"testing"
	"time"

	"github.com/git-pkgs/cooldown"
	"github.com/git-pkgs/proxy/internal/denylist"
)

func TestReleaseAllowed(t *testing.T) {
	deny, err := denylist.New([]string{"pkg:npm/lodash@4.17.22"})
	if err != nil {
		t.Fatal(err)
	}
	p := &Proxy{Cooldown: &cooldown.Config{Default: "3d"}, Denylist: deny}
	old := time.Now().Add(-10 * 24 * time.Hour)
	fresh := time.Now().Add(-time.Hour)

	tests := []struct {
		name                    string
		ecosystem, pkg, version string
		published               time.Time
		want                    bool
	}{
		{"old enough", "npm", "lodash", "4.17.21", old, true},
		{"within cooldown", "npm", "lodash", "4.17.23", fresh, false},
		{"denied", "npm", "lodash", "4.17.22", old, false},
		{"no publish time under cooldown", "npm", "lodash", "4.17.24", time.Time{}, false},
		// deb has no publish times, so cooldown never applies to it.
		{"ecosystem without cooldown", "deb", "curl", "8.5.0-2", time.Time{}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := p.ReleaseAllowed(tt.ecosystem, tt.pkg, tt.version, tt.published); got != tt.want {
				t.Errorf("ReleaseAllowed = %v, want %v", got, tt.want)
			}
		})
	}

	if !(&Proxy{}).ReleaseAllowed("npm", "lodash", "4.17.23", fresh) {
		t.Error("a proxy without cooldown or denylist refused a release")
	}
}
