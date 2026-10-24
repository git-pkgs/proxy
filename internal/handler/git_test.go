package handler

import (
	"bufio"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/git-pkgs/proxy/internal/config"
	"github.com/git-pkgs/proxy/internal/gitcache"
)

func TestParseGitRequest(t *testing.T) {
	h := NewGitHandler(nil, nil, config.UpstreamConfig{Git: map[string]config.GitUpstream{
		"up":      {URL: "https://git.example/r/"},
		"limited": {URL: "https://git.example", Repos: []string{"allowed/*"}},
	}})
	up, limited := h.upstreams["up"].dir, h.upstreams["limited"].dir
	tests := []struct {
		method, target string
		status         int
		key, repoURL   string
	}{
		{"GET", "/up/repo.git/info/refs?service=git-upload-pack", 0, up + "/repo.git", "https://git.example/r/repo.git"},
		{"GET", "/up/a/b/info/refs?service=git-upload-pack", 0, up + "/a/b.git", "https://git.example/r/a/b"},
		{"POST", "/up/a/b%20c/git-upload-pack", 0, up + "/a/b c.git", "https://git.example/r/a/b%20c"},
		{"GET", "/up/a.git/b/info/refs?service=git-upload-pack", http.StatusNotFound, "", ""},
		{"GET", "/up/a/../b/info/refs?service=git-upload-pack", http.StatusNotFound, "", ""},
		{"GET", "/up/a//b/info/refs?service=git-upload-pack", http.StatusNotFound, "", ""},
		{"GET", "/up/.git/info/refs?service=git-upload-pack", http.StatusNotFound, "", ""},
		{"GET", "/other/repo/info/refs?service=git-upload-pack", http.StatusNotFound, "", ""},
		{"GET", "/up/info/refs?service=git-upload-pack", http.StatusNotFound, "", ""},
		{"GET", "/up/repo/info/refs?service=git-receive-pack", http.StatusForbidden, "", ""},
		{"POST", "/up/repo/git-receive-pack", http.StatusForbidden, "", ""},
		{"GET", "/up/repo/info/refs", http.StatusForbidden, "", ""},
		{"GET", "/up/repo/objects/info/packs", http.StatusNotFound, "", ""},
		{"POST", "/up/repo/info/refs?service=git-upload-pack", http.StatusMethodNotAllowed, "", ""},
		{"GET", "/up/repo/git-upload-pack", http.StatusMethodNotAllowed, "", ""},
		{"GET", "/limited/allowed/repo/info/refs?service=git-upload-pack", 0, limited + "/allowed/repo.git", "https://git.example/allowed/repo"},
		{"GET", "/limited/other/repo/info/refs?service=git-upload-pack", http.StatusNotFound, "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.method+" "+tt.target, func(t *testing.T) {
			r := httptest.NewRequest(tt.method, tt.target, nil)
			r.Header.Set("Content-Type", gitRequestType)
			req, status, _ := h.parse(r)
			if status != tt.status {
				t.Fatalf("status %d, want %d", status, tt.status)
			}
			if status == 0 && (req.key != tt.key || req.repoURL != tt.repoURL) {
				t.Errorf("key %q, URL %q; want %q, %q", req.key, req.repoURL, tt.key, tt.repoURL)
			}
		})
	}

	r := httptest.NewRequest(http.MethodPost, "/up/repo/git-upload-pack", nil)
	if _, status, _ := h.parse(r); status != http.StatusUnsupportedMediaType {
		t.Errorf("POST without the request content type: status %d, want 415", status)
	}
}

// TestGitUpstreamCacheDirectory checks the cache directory follows the
// upstream's URL, so repointing an upstream does not serve the old host's
// repositories.
func TestGitUpstreamCacheDirectory(t *testing.T) {
	dir := func(url string) string {
		h := NewGitHandler(nil, nil, config.UpstreamConfig{Git: map[string]config.GitUpstream{"up": {URL: url}}})
		return h.upstreams["up"].dir
	}
	if a, b := dir("https://git.example"), dir("https://other.example"); a == b || !strings.HasPrefix(a, "up-") {
		t.Errorf("directories %q and %q, want distinct ones named after the upstream", a, b)
	}
	if a, b := dir("https://git.example"), dir("https://git.example/"); a != b {
		t.Errorf("a trailing slash changed the directory: %q, %q", a, b)
	}
}

// TestGitFetchByIDOffWithCredentials checks commits are not fetched by ID
// for an upstream that upstream.auth has credentials for: there a fetch by
// ID could reach repositories outside its repos allowlist.
func TestGitFetchByIDOffWithCredentials(t *testing.T) {
	h := NewGitHandler(nil, nil, config.UpstreamConfig{
		Git: map[string]config.GitUpstream{
			"public":  {URL: "https://github.com"},
			"private": {URL: "https://git.example", Repos: []string{"team/*"}},
			"scoped":  {URL: "https://other.example", Repos: []string{"team/*"}},
		},
		Auth: map[string]config.AuthConfig{
			"https://git.example":        {Type: "bearer", Token: "t"},
			"https://other.example/team": {Type: "bearer", Token: "t"},
		},
	})
	for name, want := range map[string]bool{"public": true, "private": false, "scoped": false} {
		if got := h.upstreams[name].fetchWants; got != want {
			t.Errorf("%s: fetchWants = %v, want %v", name, got, want)
		}
	}
}

func runGitCommand(t *testing.T, dir string, args ...string) string {
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

// gitTestSetup is a GitHandler in front of a cache of one upstream
// repository, "repo", served by git http-backend.
type gitTestSetup struct {
	work   string
	server *httptest.Server
	logs   *syncLog
	// slow delays every upstream request by half a second.
	slow atomic.Bool
	// active counts the requests the handler is serving.
	active atomic.Int32
}

// newGitTestSetup serves the cache with the given server read timeout and,
// unless zero, GitHandler.writeStall.
func newGitTestSetup(t *testing.T, readTimeout, writeStall time.Duration) *gitTestSetup {
	t.Helper()
	execPath := runGitCommand(t, "", "--exec-path")
	backendPath := ""
	for _, name := range []string{"git-http-backend", "git-http-backend.exe"} {
		if path := filepath.Join(execPath, name); fileExists(path) {
			backendPath = path
		}
	}
	if backendPath == "" {
		t.Skip("git http-backend is not installed")
	}
	root := t.TempDir()
	bare := filepath.Join(root, "repo.git")
	runGitCommand(t, "", "init", "--quiet", "--bare", "--initial-branch=main", bare)
	runGitCommand(t, bare, "config", "uploadpack.allowAnySHA1InWant", "true")
	g := &gitTestSetup{work: t.TempDir(), logs: &syncLog{}}
	runGitCommand(t, "", "clone", "--quiet", bare, g.work)
	runGitCommand(t, g.work, "commit", "--quiet", "--allow-empty", "-m", "first")
	runGitCommand(t, g.work, "push", "--quiet", "origin", "HEAD:main")

	backend := &cgi.Handler{
		Path: backendPath,
		Env:  []string{"PATH=" + os.Getenv("PATH"), "GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1"},
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if g.slow.Load() {
			time.Sleep(500 * time.Millisecond)
		}
		backend.ServeHTTP(w, r)
	}))
	t.Cleanup(upstream.Close)

	logger := slog.New(slog.NewTextHandler(g.logs, nil))
	cache, err := gitcache.New(gitcache.Options{
		CacheDir:    t.TempDir(),
		RefreshWait: 5 * time.Second,
		StaleMaxAge: time.Hour,
		Client:      &http.Client{},
		Logger:      logger,
		Upstreams:   map[string]string{"up": gitcache.UpstreamDir("up", upstream.URL)},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cache.Close() })
	h := NewGitHandler(&Proxy{Logger: logger}, cache, config.UpstreamConfig{Git: map[string]config.GitUpstream{"up": {URL: upstream.URL}}})
	if writeStall > 0 {
		h.writeStall = writeStall
	}
	routes := http.StripPrefix("/git", h.Routes())
	g.server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.active.Add(1)
		defer g.active.Add(-1)
		routes.ServeHTTP(w, r)
	}))
	g.server.Config.ReadTimeout = readTimeout
	g.server.Start()
	t.Cleanup(g.server.Close)
	return g
}

func (g *gitTestSetup) url() string {
	return g.server.URL + "/git/up/repo"
}

type syncLog struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (l *syncLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *syncLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// TestGitRequestBodyOutlastsServerReadTimeout sends a protocol v0 request
// whose haves come after a want the cache must fetch first, so upload-pack
// reads them after the server's read timeout has passed.
func TestGitRequestBodyOutlastsServerReadTimeout(t *testing.T) {
	g := newGitTestSetup(t, 200*time.Millisecond, 0)
	runGitCommand(t, g.work, "commit", "--quiet", "--allow-empty", "-m", "pull request")
	runGitCommand(t, g.work, "push", "--quiet", "origin", "HEAD:refs/pull/1/head")
	sha := runGitCommand(t, g.work, "rev-parse", "HEAD")
	runGitCommand(t, "", "clone", "--quiet", g.url(), filepath.Join(t.TempDir(), "clone"))

	var body strings.Builder
	body.WriteString(pktLine("want "+sha+" side-band-64k\n") + "0000")
	for i := range 2000 {
		body.WriteString(pktLine(fmt.Sprintf("have %040x\n", i+1)))
	}
	body.WriteString(pktLine("done\n"))

	g.slow.Store(true)
	resp, err := http.Post(g.url()+"/git-upload-pack", gitRequestType, strings.NewReader(body.String()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(out), "PACK") {
		t.Errorf("status %d, no pack in %d bytes: %.200q", resp.StatusCode, len(out), out)
	}
}

// TestGitSlowRequestBodyStartsNothing sends part of a request body and then
// nothing. The request ends at the server's read timeout, without an
// upload-pack waiting for the rest.
func TestGitSlowRequestBodyStartsNothing(t *testing.T) {
	g := newGitTestSetup(t, 200*time.Millisecond, 0)
	runGitCommand(t, "", "clone", "--quiet", g.url(), filepath.Join(t.TempDir(), "clone"))

	conn, err := net.Dial("tcp", g.server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	part := pktLine("command=ls-refs\n")
	_, err = fmt.Fprintf(conn, "POST /git/up/repo/git-upload-pack HTTP/1.1\r\nHost: proxy\r\n"+
		"Content-Type: %s\r\nGit-Protocol: version=2\r\nContent-Length: 100\r\n\r\n%s", gitRequestType, part)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("no answer to a request whose body stopped: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status %d, want %d", resp.StatusCode, http.StatusBadRequest)
	}
}

// TestGitStalledClientIsDropped fetches a pack larger than the connection
// buffers and never reads it. The request ends once a write has waited
// writeStall, rather than holding upload-pack.
func TestGitStalledClientIsDropped(t *testing.T) {
	g := newGitTestSetup(t, 0, 200*time.Millisecond)
	data := make([]byte, 16<<20)
	_, _ = rand.NewChaCha8([32]byte{}).Read(data)
	if err := os.WriteFile(filepath.Join(g.work, "large"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	runGitCommand(t, g.work, "add", "large")
	runGitCommand(t, g.work, "commit", "--quiet", "-m", "large")
	runGitCommand(t, g.work, "push", "--quiet", "origin", "HEAD:main")
	sha := runGitCommand(t, g.work, "rev-parse", "HEAD")
	runGitCommand(t, "", "clone", "--quiet", g.url(), filepath.Join(t.TempDir(), "clone"))

	conn, err := net.Dial("tcp", g.server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	body := pktLine("want "+sha+" side-band-64k\n") + "0000" + pktLine("done\n")
	_, err = fmt.Fprintf(conn, "POST /git/up/repo/git-upload-pack HTTP/1.1\r\nHost: proxy\r\n"+
		"Content-Type: %s\r\nContent-Length: %d\r\n\r\n%s", gitRequestType, len(body), body)
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); g.active.Load() == 0; {
		if time.Now().After(deadline) {
			t.Fatal("request never reached the handler")
		}
		time.Sleep(10 * time.Millisecond)
	}
	for deadline := time.Now().Add(10 * time.Second); g.active.Load() != 0; {
		if time.Now().After(deadline) {
			t.Fatal("the handler still serves a client that stopped reading")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func pktLine(payload string) string {
	return fmt.Sprintf("%04x%s", len(payload)+4, payload)
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
