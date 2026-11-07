package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/git-pkgs/proxy/internal/config"
	"github.com/git-pkgs/proxy/internal/mirror"
)

func gitTestEnv(home string) []string {
	return []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + home,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
	}
}

func gitCommand(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = gitTestEnv(t.TempDir())
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// gitHTTPBackend finds git http-backend, which serves the test upstream, and
// skips the test where git is installed without it.
func gitHTTPBackend(t *testing.T) string {
	t.Helper()
	dir := gitCommand(t, "", "--exec-path")
	for _, name := range []string{"git-http-backend", "git-http-backend.exe"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return filepath.Join(dir, name)
		}
	}
	t.Skip("git http-backend is not installed")
	return ""
}

// syncBuffer is a log destination the server and the test share.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// newGitTestRepo creates the bare repository {root}/{path}.git with one
// commit on main and returns the commit.
func newGitTestRepo(t *testing.T, root, path string) string {
	t.Helper()
	bare := filepath.Join(root, filepath.FromSlash(path)+".git")
	gitCommand(t, "", "init", "--quiet", "--bare", "--initial-branch=main", bare)
	work := t.TempDir()
	gitCommand(t, "", "clone", "--quiet", bare, work)
	if err := os.WriteFile(filepath.Join(work, "file.txt"), []byte("hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, work, "add", "file.txt")
	gitCommand(t, work, "commit", "--quiet", "-m", "first")
	gitCommand(t, work, "push", "--quiet", "origin", "HEAD:main")
	return gitCommand(t, work, "rev-parse", "HEAD")
}

// gitBackend serves the repositories under root with git http-backend.
func gitBackend(t *testing.T, root string) http.Handler {
	t.Helper()
	return &cgi.Handler{
		Path: gitHTTPBackend(t),
		Env:  append(gitTestEnv(t.TempDir()), "GIT_PROJECT_ROOT="+root, "GIT_HTTP_EXPORT_ALL=1"),
	}
}

// startGitTestServer starts the whole server with the git cache, after
// configure has set up cfg, and returns cfg and the server's log.
func startGitTestServer(t *testing.T, configure func(*config.Config)) (*config.Config, *syncBuffer) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tempDir := t.TempDir()
	cfg := config.Default()
	cfg.Listen = listener.Addr().String()
	cfg.BaseURL = "http://" + cfg.Listen
	cfg.Database.Path = filepath.Join(tempDir, "proxy.db")
	cfg.Storage.URL = "file://" + filepath.Join(tempDir, "artifacts")
	cfg.Upstream.AllowLoopback = true
	cfg.Git.CacheDir = filepath.Join(tempDir, "git")
	configure(cfg)
	if err := cfg.Validate(); err != nil {
		t.Fatalf("validating config: %v", err)
	}

	logs := &syncBuffer{}
	proxyServer, err := New(cfg, slog.New(slog.NewTextHandler(logs, nil)), BuildInfo{Version: "test", Commit: "test"})
	if err != nil {
		t.Fatalf("creating server: %v", err)
	}
	startErr := make(chan error, 1)
	go func() { startErr <- proxyServer.Start(listener) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := proxyServer.Shutdown(ctx); err != nil {
			t.Errorf("shutting down server: %v", err)
		}
		if err := <-startErr; !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("Start() error = %v, want %v", err, http.ErrServerClosed)
		}
	})
	return cfg, logs
}

// TestGitRouteServesClonesWithUpstreamCredentials clones through the whole
// server, with credentials from upstream.auth that only the proxy holds.
func TestGitRouteServesClonesWithUpstreamCredentials(t *testing.T) {
	root := t.TempDir()
	head := newGitTestRepo(t, root, "team/repo")
	backend := gitBackend(t, root)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer git-secret" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		backend.ServeHTTP(w, r)
	}))
	defer upstream.Close()

	cfg, logs := startGitTestServer(t, func(cfg *config.Config) {
		cfg.Upstream.Git = map[string]config.GitUpstream{"private": {URL: upstream.URL, Repos: []string{"team/*"}}}
		cfg.Upstream.Auth = map[string]config.AuthConfig{upstream.URL: {Type: "bearer", Token: "git-secret"}}
	})

	clone := filepath.Join(t.TempDir(), "clone")
	gitCommand(t, "", "clone", "--quiet", cfg.BaseURL+"/git/private/team/repo.git", clone)
	if got := gitCommand(t, clone, "rev-parse", "HEAD"); got != head {
		t.Errorf("HEAD = %s, want %s", got, head)
	}
	if strings.Contains(logs.String(), "request deadlines") {
		t.Errorf("request deadlines not extended:\n%s", logs.String())
	}

	resp, err := http.Get(cfg.BaseURL + "/git/private/other/repo.git/info/refs?service=git-upload-pack")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("repository outside repos: status %d, want 404", resp.StatusCode)
	}
}

// TestMirrorAPICachesGitCommit mirrors a pkg:git commit through the mirror
// API, then clones it through /git while the upstream is down.
func TestMirrorAPICachesGitCommit(t *testing.T) {
	root := t.TempDir()
	head := newGitTestRepo(t, root, "team/repo")
	upstream := httptest.NewServer(gitBackend(t, root))
	defer upstream.Close()
	cfg, _ := startGitTestServer(t, func(cfg *config.Config) {
		cfg.MirrorAPI = true
		cfg.Upstream.Git = map[string]config.GitUpstream{"up": {URL: upstream.URL}}
	})

	location := strings.TrimPrefix(upstream.URL, "http://") + "/team/repo"
	body, err := json.Marshal(map[string][]string{"purls": {"pkg:git/" + location + "@" + head}})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(cfg.BaseURL+"/api/mirror", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	var created struct{ ID string }
	err = json.NewDecoder(resp.Body).Decode(&created)
	_ = resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusAccepted {
		t.Fatalf("creating job: status %d, %v", resp.StatusCode, err)
	}

	var job mirror.Job
	deadline := time.Now().Add(30 * time.Second)
	for job.State != mirror.JobStateComplete && job.State != mirror.JobStateFailed && job.State != mirror.JobStateCanceled {
		if time.Now().After(deadline) {
			t.Fatalf("job did not finish: %+v", job)
		}
		time.Sleep(50 * time.Millisecond)
		resp, err := http.Get(cfg.BaseURL + "/api/mirror/" + created.ID)
		if err != nil {
			t.Fatal(err)
		}
		err = json.NewDecoder(resp.Body).Decode(&job)
		_ = resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	if job.State != mirror.JobStateComplete || job.Progress.Completed != 1 {
		t.Fatalf("job = %+v, want one repository mirrored", job)
	}

	upstream.Close()
	clone := filepath.Join(t.TempDir(), "clone")
	gitCommand(t, "", "clone", "--quiet", cfg.BaseURL+"/git/up/team/repo.git", clone)
	if got := gitCommand(t, clone, "rev-parse", "HEAD"); got != head {
		t.Errorf("HEAD = %s, want %s", got, head)
	}
}
