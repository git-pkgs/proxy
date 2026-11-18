package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/git-pkgs/proxy/internal/config"
	"github.com/git-pkgs/proxy/internal/gitcache"
)

func TestGitRepositoryDelete(t *testing.T) {
	for _, prefix := range []string{"/api/git/", "/ui/api/git/"} {
		t.Run(prefix, func(t *testing.T) {
			root := t.TempDir()
			newGitTestRepo(t, root, "team/repo")
			upstream := httptest.NewServer(gitBackend(t, root))
			defer upstream.Close()
			cfg, _ := startGitTestServer(t, func(cfg *config.Config) {
				cfg.Upstream.Git = map[string]config.GitUpstream{"up": {URL: upstream.URL}}
				cfg.CacheDelete = true
			})
			gitCommand(t, "", "clone", "--quiet", cfg.BaseURL+"/git/up/team/repo.git", filepath.Join(t.TempDir(), "clone"))

			// A literal %2F names no cached repository, and leaves team/repo.
			if status, body := request(t, http.MethodDelete, cfg.BaseURL+prefix+"up/team%252Frepo.git", "", nil); status != http.StatusNotFound {
				t.Errorf("DELETE up/team%%252Frepo.git: status %d, want 404\n%s", status, body)
			}
			if _, body := request(t, http.MethodGet, cfg.BaseURL+"/ui/git", "", nil); !strings.Contains(body, `data-cache-url="/ui/api/git/up/team/repo.git"`) {
				t.Errorf("the git page has no delete control:\n%s", body)
			}

			// An encoded separator names the repository the clone handler
			// does, team/repo.
			status, body := request(t, http.MethodDelete, cfg.BaseURL+prefix+"up/team%2Frepo.git", "", nil)
			if status != http.StatusOK {
				t.Fatalf("DELETE: status %d\n%s", status, body)
			}
			var resp GitDeleteResponse
			if err := json.Unmarshal([]byte(body), &resp); err != nil {
				t.Fatalf("decoding %s: %v", body, err)
			}
			if resp.Repository != "up/team/repo" || resp.FreedBytes <= 0 {
				t.Errorf("response = %+v", resp)
			}
			if _, body := request(t, http.MethodGet, cfg.BaseURL+"/ui/git", "", nil); !strings.Contains(body, "No repository is cached yet") {
				t.Errorf("the repository is still listed:\n%s", body)
			}

			for _, path := range []string{"up/team/repo.git", "up/team/repo", "other/team/repo.git", "up/"} {
				if status, body := request(t, http.MethodDelete, cfg.BaseURL+prefix+path, "", nil); status != http.StatusNotFound {
					t.Errorf("DELETE %s: status %d, want 404\n%s", path, status, body)
				}
			}
		})
	}
}

func TestGitRepositoryDeleteNeedsCacheDelete(t *testing.T) {
	root := t.TempDir()
	newGitTestRepo(t, root, "team/widgets")
	upstream := httptest.NewServer(gitBackend(t, root))
	defer upstream.Close()
	cfg, _ := startGitTestServer(t, func(cfg *config.Config) {
		cfg.Upstream.Git = map[string]config.GitUpstream{"up": {URL: upstream.URL}}
	})
	gitCommand(t, "", "clone", "--quiet", cfg.BaseURL+"/git/up/team/widgets.git", filepath.Join(t.TempDir(), "clone"))

	if _, body := request(t, http.MethodGet, cfg.BaseURL+"/ui/git", "", nil); strings.Contains(body, "data-cache-url") {
		t.Errorf("the git page shows a delete control without cache_delete:\n%s", body)
	}
	for _, prefix := range []string{"/api/git/", "/ui/api/git/"} {
		status, _ := request(t, http.MethodDelete, cfg.BaseURL+prefix+"up/team/widgets.git", "", nil)
		if status == http.StatusOK {
			t.Errorf("DELETE %s: status 200 without cache_delete", prefix)
		}
	}
	if _, body := request(t, http.MethodGet, cfg.BaseURL+"/ui/git", "", nil); !strings.Contains(body, "team/widgets") {
		t.Error("the repository was deleted without cache_delete")
	}
}

// A repository of GitHub is cached under its lowercased path, so it is
// deleted whatever the case it is named in.
func TestGitRepositoryDeleteFoldsForgeCase(t *testing.T) {
	cfg, _ := startGitTestServer(t, func(cfg *config.Config) {
		cfg.Upstream.Git = map[string]config.GitUpstream{"github": {URL: "https://github.com", Repos: []string{"Azure/*"}}}
		cfg.CacheDelete = true
		dir := filepath.Join(cfg.Git.CacheDir, gitcache.UpstreamDir("github", "https://github.com"), "azure", "login.git")
		gitCommand(t, "", "init", "--quiet", "--bare", dir)
	})
	if status, body := request(t, http.MethodDelete, cfg.BaseURL+"/api/git/github/Azure/Login.git", "", nil); status != http.StatusOK {
		t.Errorf("DELETE github/Azure/Login.git: status %d, want 200\n%s", status, body)
	}
}

// A copy cached under its mixed-case path before GitHub paths were folded
// stays removable by that spelling.
func TestGitRepositoryDeleteRemovesAMixedCaseCopy(t *testing.T) {
	cfg, _ := startGitTestServer(t, func(cfg *config.Config) {
		cfg.Upstream.Git = map[string]config.GitUpstream{"github": {URL: "https://github.com", Repos: []string{"Azure/*"}}}
		cfg.CacheDelete = true
		dir := filepath.Join(cfg.Git.CacheDir, gitcache.UpstreamDir("github", "https://github.com"), "Azure", "Login.git")
		gitCommand(t, "", "init", "--quiet", "--bare", dir)
	})
	if status, body := request(t, http.MethodDelete, cfg.BaseURL+"/api/git/github/Azure/Login.git", "", nil); status != http.StatusOK {
		t.Errorf("DELETE github/Azure/Login.git: status %d, want 200\n%s", status, body)
	}
}
