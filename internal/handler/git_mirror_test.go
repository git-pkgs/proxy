package handler

import (
	"context"
	"os"
	"path/filepath"
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

func TestGitSubmoduleLocation(t *testing.T) {
	tests := []struct {
		repoURL, submoduleURL, want string
	}{
		{"https://github.com/acme/widgets", "../lib.git", "github.com/acme/lib.git"},
		{"https://github.com/acme/widgets.git", "../../other/lib", "github.com/other/lib"},
		{"https://github.com/acme/widgets", "./vendored", "github.com/acme/widgets/vendored"},
		{"http://127.0.0.1:8080/team/repo", "../lib.git", "127.0.0.1:8080/team/lib.git"},
		{"https://github.com/acme/widgets", "https://GitLab.com/group/lib.git/", "gitlab.com/group/lib.git"},
		{"https://github.com/acme/widgets", "ssh://git@github.com:22/acme/lib.git", "github.com/acme/lib.git"},
		{"https://github.com/acme/widgets", "git://git.example/lib", "git.example/lib"},
		{"https://github.com/acme/widgets", "git@github.com:acme/lib.git", "github.com/acme/lib.git"},
		{"https://github.com/acme/widgets", "/srv/git/lib.git", ""},
		{"https://github.com/acme/widgets", "file:///srv/git/lib.git", ""},
		{"https://github.com/acme/widgets", "", ""},
	}
	for _, tt := range tests {
		got, err := gitSubmoduleLocation(tt.repoURL, tt.submoduleURL)
		if tt.want == "" {
			if err == nil {
				t.Errorf("location of %q = %q, want an error", tt.submoduleURL, got)
			}
			continue
		}
		if err != nil || got != tt.want {
			t.Errorf("location of %q from %s = %q, %v; want %q", tt.submoduleURL, tt.repoURL, got, err, tt.want)
		}
	}
}

// TestGitMirrorCachesSubmodules mirrors a commit with a submodule served by
// the same upstream and one on a host no upstream.git serves, which is
// skipped.
func TestGitMirrorCachesSubmodules(t *testing.T) {
	g := newGitTestSetup(t, 0, 0)
	libBare := filepath.Join(g.root, "lib.git")
	runGitCommand(t, "", "init", "--quiet", "--bare", "--initial-branch=main", libBare)
	libWork := t.TempDir()
	runGitCommand(t, "", "clone", "--quiet", libBare, libWork)
	runGitCommand(t, libWork, "commit", "--quiet", "--allow-empty", "-m", "lib")
	runGitCommand(t, libWork, "push", "--quiet", "origin", "HEAD:main")
	libCommit := runGitCommand(t, libWork, "rev-parse", "HEAD")

	gitmodules := "[submodule \"lib\"]\n\tpath = lib\n\turl = ../lib.git\n" +
		"[submodule \"elsewhere\"]\n\tpath = elsewhere\n\turl = https://elsewhere.example/x.git\n"
	if err := os.WriteFile(filepath.Join(g.work, ".gitmodules"), []byte(gitmodules), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitCommand(t, g.work, "add", ".gitmodules")
	runGitCommand(t, g.work, "update-index", "--add", "--cacheinfo", "160000,"+libCommit+",lib")
	runGitCommand(t, g.work, "update-index", "--add", "--cacheinfo", "160000,"+strings.Repeat("ab", 20)+",elsewhere")
	runGitCommand(t, g.work, "commit", "--quiet", "-m", "submodules")
	runGitCommand(t, g.work, "push", "--quiet", "origin", "HEAD:main")
	sha := runGitCommand(t, g.work, "rev-parse", "HEAD")
	location := strings.TrimPrefix(g.upstreamURL, "http://") + "/repo"

	got, err := g.handler.Mirror(context.Background(), location, sha)
	if err != nil {
		t.Fatalf("Mirror: %v", err)
	}
	if got.Cached {
		t.Errorf("first mirror = %+v, want fetched", got)
	}
	if got, err = g.handler.Mirror(context.Background(), location, sha); err != nil || !got.Cached {
		t.Errorf("second mirror = %+v, %v, want cached", got, err)
	}
	if !strings.Contains(g.logs.String(), "elsewhere.example") {
		t.Errorf("log doesn't name the skipped submodule:\n%s", g.logs.String())
	}

	// With the upstream gone, the submodule's commit is served from the cache.
	g.upstream.Close()
	clone := t.TempDir()
	runGitCommand(t, "", "clone", "--quiet", g.server.URL+"/git/up/lib", clone)
	if head := runGitCommand(t, clone, "rev-parse", "HEAD"); head != libCommit {
		t.Errorf("cloned lib HEAD %s, want %s", head, libCommit)
	}
}
