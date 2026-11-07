package handler

import (
	"context"
	"strings"
	"testing"

	"github.com/git-pkgs/proxy/internal/config"
)

func TestGitMirrorLocation(t *testing.T) {
	h := NewGitHandler(nil, nil, config.UpstreamConfig{Git: map[string]config.GitUpstream{
		"github":    {URL: "https://github.com"},
		"wikimedia": {URL: "https://gitlab.wikimedia.org", Repos: []string{"mediawiki/**"}},
		"sub":       {URL: "https://git.example/r/"},
		"nested":    {URL: "https://git.example/r/deep"},
	}})
	tests := []struct {
		location     string
		ok           bool
		key, repoURL string
	}{
		{"github.com/acme/widgets", true, h.upstreams["github"].dir + "/acme/widgets.git", "https://github.com/acme/widgets"},
		{"github.com/acme/widgets.git", true, h.upstreams["github"].dir + "/acme/widgets.git", "https://github.com/acme/widgets.git"},
		{"gitlab.wikimedia.org/mediawiki/skins/Vector", true, h.upstreams["wikimedia"].dir + "/mediawiki/skins/Vector.git", "https://gitlab.wikimedia.org/mediawiki/skins/Vector"},
		{"gitlab.wikimedia.org/other/repo", false, "", ""},
		{"git.example/r/a", true, h.upstreams["sub"].dir + "/a.git", "https://git.example/r/a"},
		// The longest matching upstream URL wins.
		{"git.example/r/deep/a", true, h.upstreams["nested"].dir + "/a.git", "https://git.example/r/deep/a"},
		{"git.example/other", false, "", ""},
		{"git.example/r", false, "", ""},
		{"github.com.evil/acme/widgets", false, "", ""},
		{"github.com/acme/../widgets", false, "", ""},
		{"codeberg.org/forgejo/forgejo", false, "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.location, func(t *testing.T) {
			repo, err := h.mirrorRepository(tt.location)
			if (err == nil) != tt.ok {
				t.Fatalf("error %v, want ok %v", err, tt.ok)
			}
			if tt.ok && (repo.key != tt.key || repo.url != tt.repoURL) {
				t.Errorf("key %q, URL %q; want %q, %q", repo.key, repo.url, tt.key, tt.repoURL)
			}
		})
	}
}

func TestGitMirrorCachesCommitForClones(t *testing.T) {
	g := newGitTestSetup(t, 0, 0)
	sha := runGitCommand(t, g.work, "rev-parse", "HEAD")
	location := strings.TrimPrefix(g.upstreamURL, "http://") + "/repo"

	got, err := g.handler.Mirror(context.Background(), location, sha)
	if err != nil {
		t.Fatalf("Mirror: %v", err)
	}
	if got.Cached {
		t.Errorf("first mirror = %+v, want fetched", got)
	}
	got, err = g.handler.Mirror(context.Background(), location, sha)
	if err != nil || !got.Cached {
		t.Errorf("second mirror = %+v, %v, want cached", got, err)
	}

	// With the upstream gone, a clone through the proxy gets the mirrored copy.
	g.upstream.Close()
	clone := t.TempDir()
	runGitCommand(t, "", "clone", "--quiet", g.url(), clone)
	if head := runGitCommand(t, clone, "rev-parse", "HEAD"); head != sha {
		t.Errorf("cloned HEAD %s, want %s", head, sha)
	}
}
