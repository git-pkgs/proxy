package server

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
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

// TestGitRepositoriesPageSaysWhatIsNotReadYet checks a view that is out of
// time for reading commits lists the repositories without them and says so.
func TestGitRepositoriesPageSaysWhatIsNotReadYet(t *testing.T) {
	cacheDir := t.TempDir()
	upstreams := map[string]config.GitUpstream{"up": {URL: "https://example.com"}}
	upstreamDir := gitcache.UpstreamDir("up", "https://example.com")
	for _, name := range []string{"one", "two"} {
		gitCommand(t, "", "init", "--quiet", "--bare", "--initial-branch=main", filepath.Join(cacheDir, upstreamDir, "team", name+".git"))
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cache, err := gitcache.New(gitcache.Options{
		CacheDir: cacheDir, RefreshWait: time.Second, StaleMaxAge: time.Hour,
		Client: &http.Client{}, Logger: logger, Upstreams: map[string]string{"up": upstreamDir},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cache.Close() })
	s := &Server{cfg: &config.Config{BaseURL: "http://proxy.example"}, git: cache, logger: logger, templates: &Templates{}}
	s.cfg.Upstream.Git = upstreams

	view := func(ctx context.Context) string {
		w := httptest.NewRecorder()
		s.handleGitRepositories(w, httptest.NewRequest(http.MethodGet, "/ui/git", nil).WithContext(ctx))
		return w.Body.String()
	}
	done, stop := context.WithCancel(context.Background())
	stop()
	body := view(done)
	for _, want := range []string{"2 repositories in the git cache", "team/one", "team/two", "not read yet for 2 repositories"} {
		if !strings.Contains(body, want) {
			t.Errorf("page out of time lacks %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "0 branches") {
		t.Errorf("page out of time shows a state:\n%s", body)
	}
	body = view(context.Background())
	if !strings.Contains(body, "0 branches, 0 tags") || strings.Contains(body, "not read yet") {
		t.Errorf("page with time shows no state, or says one is missing:\n%s", body)
	}
}

// TestGitRepositoriesPageLogsAStateItCannotRead checks a repository git
// fails to read is still listed, and the log says why it shows no commit.
func TestGitRepositoriesPageLogsAStateItCannotRead(t *testing.T) {
	cacheDir := t.TempDir()
	upstreamDir := gitcache.UpstreamDir("up", "https://example.com")
	repoDir := filepath.Join(cacheDir, upstreamDir, "team", "broken.git")
	gitCommand(t, "", "init", "--quiet", "--bare", "--initial-branch=main", repoDir)
	// A branch naming a commit the repository doesn't have.
	if err := os.WriteFile(filepath.Join(repoDir, "refs", "heads", "main"), []byte(strings.Repeat("1", 40)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	cache, err := gitcache.New(gitcache.Options{
		CacheDir: cacheDir, RefreshWait: time.Second, StaleMaxAge: time.Hour,
		Client: &http.Client{}, Logger: logger, Upstreams: map[string]string{"up": upstreamDir},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cache.Close() })
	s := &Server{cfg: &config.Config{BaseURL: "http://proxy.example"}, git: cache, logger: logger, templates: &Templates{}}
	s.cfg.Upstream.Git = map[string]config.GitUpstream{"up": {URL: "https://example.com"}}

	w := httptest.NewRecorder()
	s.handleGitRepositories(w, httptest.NewRequest(http.MethodGet, "/ui/git", nil))
	if body := w.Body.String(); !strings.Contains(body, "team/broken") || strings.Contains(body, "not read yet") {
		t.Errorf("page doesn't list the repository, or says it has yet to read it:\n%s", body)
	}
	for _, want := range []string{"level=WARN", "repository=" + upstreamDir + "/team/broken.git", "error="} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("log lacks %q:\n%s", want, logs.String())
		}
	}
}

// TestGitRepositoryRowsKeepCase checks an upstream, a repos pattern and a
// repository with uppercase letters are listed as they are spelled. Where
// cache_dir ignores case, as on macOS and Windows, the cache tracks them
// under a lowercased key.
func TestGitRepositoryRowsKeepCase(t *testing.T) {
	cacheDir := t.TempDir()
	upstreams := map[string]config.GitUpstream{"Up": {URL: "https://example.com", Repos: []string{"Team/*"}}}
	upstreamDir := gitcache.UpstreamDir("Up", "https://example.com")
	gitCommand(t, "", "init", "--quiet", "--bare", filepath.Join(cacheDir, upstreamDir, "Team", "Repo.git"))
	cache, err := gitcache.New(gitcache.Options{
		CacheDir: cacheDir, RefreshWait: time.Second, StaleMaxAge: time.Hour,
		Client: &http.Client{}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Upstreams: map[string]string{"Up": upstreamDir},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cache.Close() })

	rows, _ := gitRepositoryRows("http://proxy.example", upstreams, cache.Repositories(), func(key string) (gitcache.RepositoryState, bool) {
		st, err := cache.RepositoryState(context.Background(), key)
		return st, err == nil
	})
	if len(rows) != 1 || rows[0].Upstream != "Up" || rows[0].Repo != "Team/Repo" ||
		rows[0].CloneURL != "http://proxy.example/git/Up/Team/Repo.git" || rows[0].Refs == "" {
		t.Errorf("rows %+v, want Up / Team/Repo with its clone URL and state", rows)
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

// TestGitRepositoryRowsHideWhatGitRefuses checks the page leaves out
// repositories the repos allowlist no longer allows, and shows no cached
// commit of one the upstream refused, as /git serves neither.
func TestGitRepositoryRowsHideWhatGitRefuses(t *testing.T) {
	upstreams := map[string]config.GitUpstream{"github": {URL: "https://github.com", Repos: []string{"acme/*"}}}
	dir := gitcache.UpstreamDir("github", "https://github.com")
	state := func(string) (gitcache.RepositoryState, bool) {
		return gitcache.RepositoryState{Branch: "main", Commit: "0123456789", Subject: "Secret plans", Branches: 1}, true
	}
	rows, size := gitRepositoryRows("http://proxy.example", upstreams, []gitcache.RepositoryStats{
		{Key: dir + "/acme/widgets.git", Size: 1, Refused: true, LastError: "refused"},
		{Key: dir + "/acme/gadgets.git", Size: 2, LastError: "fetch_failed"},
		{Key: dir + "/other/secret.git", Size: 4},
	}, state)
	if len(rows) != 2 || size != 3 {
		t.Fatalf("rows %+v, size %d; want the two allowed repositories", rows, size)
	}
	if r := rows[0]; r.Repo != "acme/widgets" || r.Commit != "" || r.Subject != "" || r.Branch != "" || r.Refs != "" || r.LastError != "refused" {
		t.Errorf("refused row %+v, want no cached state but the error", r)
	}
	if r := rows[1]; r.Repo != "acme/gadgets" || r.Subject != "Secret plans" {
		t.Errorf("row after a failed fetch %+v, want its cached state", r)
	}
}

// TestGitRepositoryRowsEscapeCloneURL checks path segments such as Azure
// DevOps project names, which may hold spaces, '#' or '%', are escaped.
func TestGitRepositoryRowsEscapeCloneURL(t *testing.T) {
	upstreams := map[string]config.GitUpstream{"azure": {URL: "https://dev.azure.com"}}
	dir := gitcache.UpstreamDir("azure", "https://dev.azure.com")
	none := func(string) (gitcache.RepositoryState, bool) { return gitcache.RepositoryState{}, false }
	rows, _ := gitRepositoryRows("http://proxy.example/", upstreams, []gitcache.RepositoryStats{
		{Key: dir + "/org/My Project/_git/My Repo#1?x%.git"},
	}, none)
	want := "http://proxy.example/git/azure/org/My%20Project/_git/My%20Repo%231%3Fx%25.git"
	if len(rows) != 1 || rows[0].CloneURL != want {
		t.Errorf("rows %+v, want clone URL %s", rows, want)
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
