package server

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/git-pkgs/proxy/internal/config"
	"github.com/git-pkgs/proxy/internal/gitcache"
)

func TestGitRepositoriesPage(t *testing.T) {
	root := t.TempDir()
	newGitTestRepo(t, root, "team/repo")
	upstream := httptest.NewServer(gitBackend(t, root))
	defer upstream.Close()
	cfg, _ := startGitTestServer(t, func(cfg *config.Config) {
		cfg.Upstream.Git = map[string]config.GitUpstream{"up": {URL: upstream.URL}}
	})

	if status, body := request(t, http.MethodGet, cfg.BaseURL+"/ui/git", "", nil); status != http.StatusOK || !strings.Contains(body, "No repository is cached yet") {
		t.Errorf("empty page: status %d\n%s", status, body)
	}
	gitCommand(t, "", "clone", "--quiet", cfg.BaseURL+"/git/up/team/repo.git", filepath.Join(t.TempDir(), "clone"))

	if _, body := request(t, http.MethodGet, cfg.BaseURL+"/ui/", "", nil); !strings.Contains(body, `href="/ui/git"`) {
		t.Error("navigation doesn't link the git page")
	}
	status, body := request(t, http.MethodGet, cfg.BaseURL+"/ui/git", "", nil)
	if status != http.StatusOK {
		t.Fatalf("status %d", status)
	}
	for _, want := range []string{">up<", "team/repo", cfg.BaseURL + "/git/up/team/repo.git", "1 request", "main", "1 branch, 0 tags"} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q:\n%s", want, body)
		}
	}
}

func TestGitRepositoriesPageNeedsUpstreamGit(t *testing.T) {
	cfg, _ := startGitTestServer(t, func(*config.Config) {})
	if status, _ := request(t, http.MethodGet, cfg.BaseURL+"/ui/git", "", nil); status != http.StatusNotFound {
		t.Errorf("status %d, want 404 without upstream.git", status)
	}
	if _, body := request(t, http.MethodGet, cfg.BaseURL+"/ui/", "", nil); strings.Contains(body, `href="/ui/git"`) {
		t.Error("navigation links the git page without upstream.git")
	}
}

func TestGitRepositoryRows(t *testing.T) {
	upstreams := map[string]config.GitUpstream{"github": {URL: "https://github.com"}}
	dir := gitcache.UpstreamDir("github", "https://github.com")
	failed := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	state := func(key string) (gitcache.RepositoryState, bool) {
		return gitcache.RepositoryState{
			Branch: "main", Commit: "0123456789abcdef0123456789abcdef01234567", Subject: "Fix the build",
			CommitTime: time.Now().Add(-3 * time.Hour), Branches: 2, Tags: 1,
		}, key == dir+"/acme/widgets.git"
	}
	rows := gitRepositoryRows("http://proxy.example", upstreams, []gitcache.RepositoryStats{
		{Key: dir + "/acme/widgets.git", Size: 3 << 20, Requests: 1, LastError: "fetch_failed", LastErrorAt: failed},
		{Key: "gone-12345678/x.git"},
	}, state)
	if len(rows) != 1 {
		t.Fatalf("rows %+v, want the configured upstream's one", rows)
	}
	r := rows[0]
	if r.Upstream != "github" || r.Repo != "acme/widgets" || r.CloneURL != "http://proxy.example/git/github/acme/widgets.git" ||
		r.Size != "3.0 MB" || r.Requests != "1 request" || r.LastError != "fetch_failed" ||
		r.Branch != "main" || r.Commit != "0123456" || r.Subject != "Fix the build" || r.CommitAge != "3 hours ago" || r.Refs != "2 branches, 1 tag" {
		t.Errorf("row %+v", r)
	}

	stats := gitEcosystemStats([]gitcache.RepositoryStats{{Size: 10}, {Size: 32}})
	if stats.Ecosystem != "git" || stats.Artifacts != 2 || stats.CacheSize != 42 {
		t.Errorf("ecosystem stats %+v, want git with 2 repositories and 42 bytes", stats)
	}
}
