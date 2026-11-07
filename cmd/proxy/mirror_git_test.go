package main

import (
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/git-pkgs/proxy/internal/gitcache"
)

// gitMirrorUpstream serves team/repo, with one commit on main, from git
// http-backend, and returns the server and the commit.
func gitMirrorUpstream(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	git := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = []string{
			"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(), "GIT_CONFIG_NOSYSTEM=1",
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
		}
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	backend := ""
	for _, name := range []string{"git-http-backend", "git-http-backend.exe"} {
		if path := filepath.Join(git("", "--exec-path"), name); fileExistsForTest(path) {
			backend = path
		}
	}
	if backend == "" {
		t.Skip("git http-backend is not installed")
	}
	root := t.TempDir()
	bare := filepath.Join(root, "team", "repo.git")
	git("", "init", "--quiet", "--bare", "--initial-branch=main", bare)
	work := t.TempDir()
	git("", "clone", "--quiet", bare, work)
	git(work, "commit", "--quiet", "--allow-empty", "-m", "first")
	git(work, "push", "--quiet", "origin", "HEAD:main")
	head := git(work, "rev-parse", "HEAD")

	server := httptest.NewServer(&cgi.Handler{
		Path: backend,
		Env:  []string{"PATH=" + os.Getenv("PATH"), "GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1"},
	})
	t.Cleanup(server.Close)
	return server, head
}

func fileExistsForTest(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// writeGitMirrorConfig writes a config with upstream.git "up" at
// upstreamURL and the git cache, storage and database under dir.
func writeGitMirrorConfig(t *testing.T, dir, upstreamURL string) string {
	t.Helper()
	cfg := "storage:\n" +
		"  url: file://" + filepath.Join(dir, "artifacts") + "\n" +
		"database:\n" +
		"  path: " + filepath.Join(dir, "proxy.db") + "\n" +
		"upstream:\n" +
		"  allow_loopback: true\n" +
		"  git:\n" +
		"    up: " + upstreamURL + "\n" +
		"git:\n" +
		"  cache_dir: " + filepath.Join(dir, "git") + "\n"
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatalf("writing config: %v", err)
	}
	return path
}

func TestMirrorCommandCachesGitCommit(t *testing.T) {
	upstream, head := gitMirrorUpstream(t)
	dir := t.TempDir()
	configPath := writeGitMirrorConfig(t, dir, upstream.URL)
	purl := "pkg:git/" + strings.TrimPrefix(upstream.URL, "http://") + "/team/repo@" + head

	output := runMirrorProcess(t, 0, "-config", configPath, purl)
	if !strings.Contains(output, "1 downloaded") {
		t.Errorf("output:\n%s", output)
	}
	cached, _ := filepath.Glob(filepath.Join(dir, "git", "up-*", "team", "repo.git"))
	if len(cached) != 1 {
		t.Errorf("cached repositories %v, want team/repo.git", cached)
	}

	output = runMirrorProcess(t, 0, "-config", configPath, purl)
	if !strings.Contains(output, "1 skipped") {
		t.Errorf("second run output:\n%s", output)
	}
}

// TestMirrorCommandGitCacheInUse checks pkg:git items fail, pointing at the
// mirror API, while the running server holds git.cache_dir.
func TestMirrorCommandGitCacheInUse(t *testing.T) {
	upstream, head := gitMirrorUpstream(t)
	dir := t.TempDir()
	configPath := writeGitMirrorConfig(t, dir, upstream.URL)
	cache, err := gitcache.New(gitcache.Options{
		CacheDir:    filepath.Join(dir, "git"),
		RefreshWait: time.Second,
		Client:      &http.Client{},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cache.Close() }()
	purl := "pkg:git/" + strings.TrimPrefix(upstream.URL, "http://") + "/team/repo@" + head

	output := runMirrorProcess(t, 1, "-config", configPath, purl)
	for _, want := range []string{"in use by another process", "mirror API", "1 failed"} {
		if !strings.Contains(output, want) {
			t.Errorf("output missing %q:\n%s", want, output)
		}
	}
}
