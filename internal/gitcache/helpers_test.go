package gitcache

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"log"
	"log/slog"
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
)

// gitEnv isolates the tests' own git commands from the machine's config.
func gitEnv(home string) []string {
	return []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + home,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
	}
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := tryGit(t, dir, args...)
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(out)
}

func tryGit(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = gitEnv(t.TempDir())
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// httpBackendPath finds git http-backend, which serves the fake upstreams,
// and skips the test where git is installed without it.
func httpBackendPath(t *testing.T) string {
	t.Helper()
	dir := runGit(t, "", "--exec-path")
	for _, name := range []string{"git-http-backend", "git-http-backend.exe"} {
		if path := filepath.Join(dir, name); fileExists(path) {
			return path
		}
	}
	t.Skip("git http-backend is not installed")
	return ""
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// fakeUpstream serves bare repositories under root with git http-backend.
type fakeUpstream struct {
	t      *testing.T
	root   string
	server *httptest.Server

	// status, when set, is returned instead of serving the request.
	status atomic.Int32
	// requireAuth, when set, is the Authorization every request must carry.
	requireAuth string
	// v0Only drops Git-Protocol, so only protocol v0 is spoken.
	v0Only bool
	// hold, when set, blocks requests until it is closed.
	holdMu sync.Mutex
	hold   chan struct{}

	requests atomic.Int32
	// gitRequests counts requests from git processes, which reach the
	// upstream through the relay, and gitSessions the fetches they make.
	gitRequests atomic.Int32
	gitSessions atomic.Int32
	unauthed    atomic.Int32

	// stallFetches makes git's own upload-pack requests hang, and
	// delayFetches holds them for that long.
	stallFetches atomic.Bool
	delayFetches atomic.Int64
	// fetchHold, while set, holds git's own upload-pack requests until it
	// is closed; fetchArrived is closed when the first one arrives.
	fetchHold    chan struct{}
	fetchArrived chan struct{}
	fetchOnce    *sync.Once

	bodiesMu sync.Mutex
	bodies   []string
}

// recordBody keeps the decoded body of a POST. It also gives the request a
// Content-Length, since net/http/cgi refuses chunked bodies, which git sends
// once a request outgrows http.postBuffer.
func (u *fakeUpstream) recordBody(r *http.Request) {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		u.t.Errorf("reading request body: %v", err)
	}
	r.Body = io.NopCloser(bytes.NewReader(raw))
	r.ContentLength = int64(len(raw))
	r.TransferEncoding = nil
	decoded := raw
	if r.Header.Get("Content-Encoding") == "gzip" {
		if zr, err := gzip.NewReader(bytes.NewReader(raw)); err == nil {
			decoded, _ = io.ReadAll(zr)
		}
	}
	u.bodiesMu.Lock()
	u.bodies = append(u.bodies, string(decoded))
	u.bodiesMu.Unlock()
}

// lsRefsBodies returns the ls-refs requests the upstream received.
func (u *fakeUpstream) lsRefsBodies() []string {
	u.bodiesMu.Lock()
	defer u.bodiesMu.Unlock()
	var out []string
	for _, body := range u.bodies {
		if strings.Contains(body, "command=ls-refs") {
			out = append(out, body)
		}
	}
	return out
}

func newFakeUpstream(t *testing.T) *fakeUpstream {
	t.Helper()
	u := &fakeUpstream{t: t, root: t.TempDir()}
	backend := &cgi.Handler{
		Path:   httpBackendPath(t),
		Stderr: io.Discard,
		Logger: log.New(io.Discard, "", 0),
		Env: append(gitEnv(t.TempDir()),
			"GIT_PROJECT_ROOT="+u.root,
			"GIT_HTTP_EXPORT_ALL=1"),
	}
	u.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.requests.Add(1)
		if r.UserAgent() != gitUserAgent {
			u.gitRequests.Add(1)
			if r.Method == http.MethodGet {
				u.gitSessions.Add(1)
			}
		}
		u.holdMu.Lock()
		hold := u.hold
		u.holdMu.Unlock()
		if hold != nil {
			<-hold
		}
		if status := u.status.Load(); status != 0 {
			http.Error(w, "unavailable", int(status))
			return
		}
		if u.requireAuth != "" && r.Header.Get("Authorization") != u.requireAuth {
			u.unauthed.Add(1)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if u.v0Only {
			r.Header.Del("Git-Protocol")
		}
		if r.UserAgent() != gitUserAgent && r.Method == http.MethodPost {
			u.holdMu.Lock()
			fetchHold, fetchArrived, fetchOnce := u.fetchHold, u.fetchArrived, u.fetchOnce
			u.holdMu.Unlock()
			if fetchHold != nil {
				fetchOnce.Do(func() { close(fetchArrived) })
				<-fetchHold
			}
		}
		if delay := u.delayFetches.Load(); delay > 0 && r.UserAgent() != gitUserAgent && r.Method == http.MethodPost {
			time.Sleep(time.Duration(delay))
		}
		if u.stallFetches.Load() && r.UserAgent() != gitUserAgent && r.Method == http.MethodPost {
			// The server notices the client leaving only once the body is read.
			_, _ = io.Copy(io.Discard, r.Body)
			<-r.Context().Done()
			return
		}
		if r.Method == http.MethodPost {
			u.recordBody(r)
		}
		backend.ServeHTTP(w, r)
	}))
	t.Cleanup(u.server.Close)
	return u
}

// holdFetches holds git's own upload-pack requests until release is called.
// arrived is closed when the first one reaches the upstream.
func (u *fakeUpstream) holdFetches() (arrived <-chan struct{}, release func()) {
	u.holdMu.Lock()
	defer u.holdMu.Unlock()
	hold, first := make(chan struct{}), make(chan struct{})
	u.fetchHold, u.fetchArrived, u.fetchOnce = hold, first, &sync.Once{}
	var once sync.Once
	return first, func() {
		once.Do(func() {
			u.holdMu.Lock()
			u.fetchHold = nil
			u.holdMu.Unlock()
			close(hold)
		})
	}
}

func (u *fakeUpstream) holdRequests() func() {
	u.holdMu.Lock()
	defer u.holdMu.Unlock()
	hold := make(chan struct{})
	u.hold = hold
	var once sync.Once
	return func() {
		once.Do(func() {
			u.holdMu.Lock()
			u.hold = nil
			u.holdMu.Unlock()
			close(hold)
		})
	}
}

// newRepo creates {name}.git upstream with one commit on main and a tag, and
// returns a working clone to commit and push from.
func (u *fakeUpstream) newRepo(name string) string {
	u.t.Helper()
	bare := filepath.Join(u.root, name+".git")
	runGit(u.t, "", "init", "--quiet", "--bare", "--initial-branch=main", bare)
	work := u.t.TempDir()
	runGit(u.t, "", "clone", "--quiet", bare, work)
	runGit(u.t, work, "checkout", "--quiet", "-b", "main")
	u.commit(work, "first")
	runGit(u.t, work, "tag", "-a", "-m", "v1", "v1")
	runGit(u.t, work, "push", "--quiet", "origin", "main", "v1")
	return work
}

func (u *fakeUpstream) commit(work, msg string) string {
	u.t.Helper()
	if err := os.WriteFile(filepath.Join(work, "file.txt"), []byte(msg+"\n"), 0o600); err != nil {
		u.t.Fatal(err)
	}
	runGit(u.t, work, "add", "file.txt")
	runGit(u.t, work, "commit", "--quiet", "-m", msg)
	return runGit(u.t, work, "rev-parse", "HEAD")
}

// waitIdle waits until no request or refresh is using a repository, so a
// test can inspect or change the cache without racing a request finishing.
func waitIdle(t *testing.T, c *Cache) {
	t.Helper()
	waitFor(t, func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		for _, rp := range c.repos {
			if rp.users > 0 {
				return false
			}
		}
		return true
	})
}

// bare is the upstream repository named "repo".
// repo returns the cache's entry for key, reading the map under the lock.
func (tc *testCache) repo(key string) *repo {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	return tc.repos[key]
}

// usersOf is the number of requests and refreshes using the repository.
func (tc *testCache) usersOf(key string) int {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	return tc.repos[key].users
}

func (u *fakeUpstream) bare() string {
	return filepath.Join(u.root, "repo.git")
}

type testCache struct {
	*Cache
	server *httptest.Server
}

func newTestCache(t *testing.T, upstreamURL string, configure func(*Options)) *testCache {
	t.Helper()
	// The cache's log is shown when the test fails.
	logs := &lockedBuffer{}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("git cache log:\n%s", logs.String())
		}
	})
	opts := Options{
		Upstreams:   map[string]string{"up": "up"},
		CacheDir:    t.TempDir(),
		RefreshWait: 5 * time.Second,
		StaleMaxAge: time.Hour,
		Client:      &http.Client{},
		Logger:      slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
	if configure != nil {
		configure(&opts)
	}
	c, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	server := httptest.NewServer(testFront(c, upstreamURL, true))
	t.Cleanup(func() {
		server.Close()
		_ = c.Close()
	})
	return &testCache{Cache: c, server: server}
}

// testFront serves /git/up/{repo}/info/refs and git-upload-pack from c, as
// internal/handler does once it has validated the request, with repo
// fetched from upstreamURL/{repo}. fetchWants is OpenOptions.FetchWants.
func testFront(c *Cache, upstreamURL string, fetchWants bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/git/up/")
		repo, infoRefs := strings.CutSuffix(path, "/info/refs")
		if !infoRefs {
			var ok bool
			if repo, ok = strings.CutSuffix(path, "/git-upload-pack"); !ok || r.Method != http.MethodPost {
				http.Error(w, "not served", http.StatusForbidden)
				return
			}
		} else if r.URL.RawQuery != "service=git-upload-pack" {
			http.Error(w, "not served", http.StatusForbidden)
			return
		}
		var req *Request
		if !infoRefs {
			var err error
			if req, err = c.ReadRequest(r); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			defer req.Close()
		}
		lease, err := c.Open(r.Context(), "up/"+strings.TrimSuffix(repo, ".git")+".git", upstreamURL+"/"+repo,
			OpenOptions{Refresh: infoRefs, FetchWants: fetchWants})
		switch {
		case errors.Is(err, ErrNotFound):
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		case errors.Is(err, ErrTooLarge), errors.Is(err, ErrCacheFull):
			http.Error(w, err.Error(), http.StatusInsufficientStorage)
			return
		case err != nil:
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer lease.Release()
		if infoRefs {
			lease.ServeInfoRefs(w, r)
		} else {
			lease.ServeUploadPack(w, r, req)
		}
	})
}

func (tc *testCache) url(repo string) string {
	return tc.server.URL + "/git/up/" + repo
}

// clone clones repo through the cache with the given protocol version and
// returns the working copy.
func (tc *testCache) clone(t *testing.T, repo string, args ...string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "clone")
	runGit(t, "", append(append([]string{"clone", "--quiet"}, args...), tc.url(repo), dir)...)
	return dir
}

// cachedRefs lists the refs of the cached copy of "repo".
func (tc *testCache) cachedRefs(t *testing.T) string {
	t.Helper()
	return runGit(t, filepath.Join(tc.dir, "up", "repo.git"), "for-each-ref", "--format=%(objectname) %(refname)")
}

// lockedBuffer is a bytes.Buffer safe for concurrent writers.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
