package server

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/git-pkgs/proxy/internal/config"
	"github.com/git-pkgs/proxy/internal/database"
	"github.com/git-pkgs/proxy/internal/gitcache"
	"github.com/git-pkgs/proxy/internal/metrics"
	"github.com/prometheus/client_golang/prometheus/testutil"
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
	for _, want := range []string{"1 repository in the git cache", ">up<", "team/repo", cfg.BaseURL + "/git/up/team/repo.git", "1 request", "main", "1 branch, 0 tags"} {
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
	rows, size := gitRepositoryRows("http://proxy.example", upstreams, []gitcache.RepositoryStats{
		{Key: dir + "/acme/widgets.git", Size: 3 << 20, Requests: 1, LastError: "fetch_failed", LastErrorAt: failed},
		{Key: "gone-12345678/x.git"},
	}, state)
	if len(rows) != 1 || size != 3<<20 {
		t.Fatalf("rows %+v, size %d; want the configured upstream's one and its size", rows, size)
	}
	r := rows[0]
	if r.Upstream != "github" || r.Repo != "acme/widgets" || r.CloneURL != "http://proxy.example/git/github/acme/widgets.git" ||
		r.Size != "3.0 MB" || r.Requests != "1 request" || r.LastError != "fetch_failed" ||
		r.Branch != "main" || r.Commit != "0123456" || r.Subject != "Fix the build" || r.CommitAge != "3 hours ago" || r.Refs != "2 branches, 1 tag" {
		t.Errorf("row %+v", r)
	}
}

// TestGitCacheGauges checks the git cache has gauges of its own, published
// even when the database can't be read.
func TestGitCacheGauges(t *testing.T) {
	cacheDir := t.TempDir()
	upstreamDir := gitcache.UpstreamDir("up", "https://example.com")
	gitCommand(t, "", "init", "--quiet", "--bare", filepath.Join(cacheDir, upstreamDir, "team", "repo.git"))
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cache, err := gitcache.New(gitcache.Options{
		CacheDir: cacheDir, RefreshWait: time.Second, StaleMaxAge: time.Hour,
		Client: &http.Client{}, Logger: logger, Upstreams: map[string]string{"up": upstreamDir},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cache.Close() })
	db, err := database.Create(filepath.Join(t.TempDir(), "proxy.db"))
	if err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	metrics.UpdateGitCacheStats(0, 0)
	s := &Server{db: db, git: cache, logger: logger}
	s.updateGauges()
	if got := testutil.ToFloat64(metrics.GitCachedRepositories); got != 1 {
		t.Errorf("proxy_git_cached_repositories = %v, want 1", got)
	}
	if got := testutil.ToFloat64(metrics.GitCacheSize); got <= 0 {
		t.Errorf("proxy_git_cache_size_bytes = %v, want the repository's size", got)
	}
}
