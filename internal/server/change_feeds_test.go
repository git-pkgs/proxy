package server

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/git-pkgs/proxy/internal/config"
	"github.com/git-pkgs/proxy/internal/database"
	"github.com/git-pkgs/proxy/internal/handler"
	"github.com/git-pkgs/proxy/internal/storage"
	"github.com/git-pkgs/registries/changes"
	"github.com/git-pkgs/registries/client"
	"github.com/git-pkgs/registries/fetch"
	"github.com/go-chi/chi/v5"
)

// changingRegistry serves npm packuments, Composer metadata, and both
// registries' change feeds, all from one set of documents that a test can
// change. The feeds list every document changed since the previous read.
type changingRegistry struct {
	*httptest.Server
	mu      sync.Mutex
	docs    map[string]string // path -> body
	changed []string          // paths changed since the feed was last read
	feedSeq int
	fetches map[string]int
}

func newChangingRegistry(t *testing.T) *changingRegistry {
	t.Helper()
	reg := &changingRegistry{docs: map[string]string{}, fetches: map[string]int{}}
	reg.Server = httptest.NewServer(http.HandlerFunc(reg.serve))
	t.Cleanup(reg.Close)
	return reg
}

func (reg *changingRegistry) set(path, body string) {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	if _, ok := reg.docs[path]; ok {
		reg.changed = append(reg.changed, path)
	}
	reg.docs[path] = body
}

func (reg *changingRegistry) fetched(path string) int {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	return reg.fetches[path]
}

// takeChanged returns the changed paths with prefix and forgets them.
func (reg *changingRegistry) takeChanged(prefix string) []string {
	var taken, kept []string
	for _, p := range reg.changed {
		if name, ok := strings.CutPrefix(p, prefix); ok {
			taken = append(taken, name)
		} else {
			kept = append(kept, p)
		}
	}
	reg.changed = kept
	return taken
}

func (reg *changingRegistry) serve(w http.ResponseWriter, r *http.Request) {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/metadata/changes.json":
		if r.URL.Query().Get("since") == "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":"missing since","timestamp":1}`)
			return
		}
		var actions []string
		for _, name := range reg.takeChanged("/p2/") {
			actions = append(actions, fmt.Sprintf(`{"type":"update","package":%q,"time":1}`, strings.TrimSuffix(name, ".json")))
		}
		_, _ = fmt.Fprintf(w, `{"actions":[%s],"timestamp":2}`, strings.Join(actions, ","))
		return
	case "/replicate/registry/":
		_, _ = fmt.Fprintf(w, `{"update_seq":%d}`, reg.feedSeq)
		return
	case "/replicate/registry/_changes":
		var results []string
		for _, name := range reg.takeChanged("/npm/") {
			reg.feedSeq++
			results = append(results, fmt.Sprintf(`{"seq":%d,"id":%q}`, reg.feedSeq, name))
		}
		_, _ = fmt.Fprintf(w, `{"results":[%s],"last_seq":%d}`, strings.Join(results, ","), reg.feedSeq)
		return
	}
	reg.fetches[r.URL.Path]++
	body, ok := reg.docs[r.URL.Path]
	if !ok {
		http.NotFound(w, r)
		return
	}
	etag := fmt.Sprintf(`"%x"`, sha256.Sum256([]byte(body)))
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("ETag", etag)
	_, _ = io.WriteString(w, body)
}

func composerDoc(versions ...string) string {
	var entries []string
	for _, v := range versions {
		entries = append(entries, fmt.Sprintf(`{"name":"acme/lib","version":%q,"version_normalized":"%s.0","dist":{"type":"zip","url":"https://example.test/acme/lib/%s.zip","reference":"r%s"}}`, v, v, v, v))
	}
	return `{"packages":{"acme/lib":[` + strings.Join(entries, ",") + `]},"minified":"composer/2.0"}`
}

func npmDoc(versions ...string) string {
	var entries []string
	for _, v := range versions {
		entries = append(entries, fmt.Sprintf(`%q:{"name":"left-pad","version":%q,"dist":{"tarball":"https://registry.example.test/left-pad/-/left-pad-%s.tgz"}}`, v, v, v))
	}
	return `{"name":"left-pad","dist-tags":{"latest":"` + versions[len(versions)-1] + `"},"versions":{` + strings.Join(entries, ",") + `}}`
}

// changeFeedRouter mounts the npm and Composer handlers on reg, with
// metadata cached for an hour.
func changeFeedRouter(t *testing.T, reg *changingRegistry) (http.Handler, *database.DB) {
	t.Helper()
	db, err := database.Create(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("creating database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store, err := storage.OpenBucket(context.Background(), "file://"+t.TempDir())
	if err != nil {
		t.Fatalf("opening storage: %v", err)
	}
	fetcher := fetch.NewFetcher(fetch.WithHTTPClient(reg.Client()), fetch.WithMaxRetries(0))
	t.Cleanup(func() { _ = fetcher.Close() })
	proxy := handler.NewProxy(db, store, fetcher, fetch.NewResolver(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	proxy.HTTPClient = reg.Client()
	proxy.CacheMetadata = true
	proxy.MetadataTTL = time.Hour
	proxy.SetMetadataRewriteCacheSize(1 << 20)

	cfg := config.Default()
	cfg.BaseURL = "http://proxy.example"
	cfg.Upstream.NPM = reg.URL + "/npm"
	cfg.Upstream.Composer = reg.URL
	cfg.Upstream.ComposerRepository = reg.URL
	s := &Server{cfg: cfg}
	r := chi.NewRouter()
	s.mountProtocolHandlers(r, proxy)
	return r, db
}

func getBody(t *testing.T, h http.Handler, path string) string {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", path, w.Code, w.Body.String())
	}
	return w.Body.String()
}

// TestChangeFeedRevalidatesChangedMetadata checks the whole path: cached
// metadata is served within its TTL even after upstream changed, until the
// change feed lists the package, after which the next request revalidates
// and serves the new release. Packages the feed doesn't list stay cached.
func TestChangeFeedRevalidatesChangedMetadata(t *testing.T) {
	reg := newChangingRegistry(t)
	reg.set("/p2/acme/lib.json", composerDoc("1.0.0"))
	reg.set("/p2/acme/other.json", `{"packages":{"acme/other":[]},"minified":"composer/2.0"}`)
	reg.set("/npm/left-pad", npmDoc("1.0.0"))
	h, db := changeFeedRouter(t, reg)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := changes.WithClient(client.NewClient(client.WithHTTPClient(reg.Client()), client.WithMaxRetries(0)))
	feeds := []changes.Feed{
		changes.NewPackagist(c, changes.WithURL(changes.PackagistFeedURL(reg.URL))),
		changes.NewNPM(c, changes.WithURL(reg.URL+"/replicate/registry/_changes")),
	}
	cursors := make([]changes.Cursor, len(feeds))
	for i, feed := range feeds {
		cursor, err := readChangeFeed(context.Background(), db, logger, feed, "")
		if err != nil || cursor == "" {
			t.Fatalf("starting %s feed: cursor %q, %v", feed.Ecosystem(), cursor, err)
		}
		cursors[i] = cursor
	}

	getBody(t, h, "/composer/p2/acme/lib.json")
	getBody(t, h, "/composer/p2/acme/other.json")
	getBody(t, h, "/npm/left-pad")

	reg.set("/p2/acme/lib.json", composerDoc("1.0.0", "1.1.0"))
	reg.set("/npm/left-pad", npmDoc("1.0.0", "1.1.0"))
	if body := getBody(t, h, "/composer/p2/acme/lib.json"); strings.Contains(body, "1.1.0") {
		t.Fatal("composer metadata revalidated before the feed listed it; the test proves nothing")
	}
	if body := getBody(t, h, "/npm/left-pad"); strings.Contains(body, "1.1.0") {
		t.Fatal("npm packument revalidated before the feed listed it; the test proves nothing")
	}

	for i, feed := range feeds {
		next, err := readChangeFeed(context.Background(), db, logger, feed, cursors[i])
		if err != nil || next == cursors[i] {
			t.Fatalf("reading %s feed: cursor %q -> %q, %v", feed.Ecosystem(), cursors[i], next, err)
		}
	}

	if body := getBody(t, h, "/composer/p2/acme/lib.json"); !strings.Contains(body, "1.1.0") {
		t.Errorf("composer metadata after the feed listed it = %s, want 1.1.0", body)
	}
	if body := getBody(t, h, "/npm/left-pad"); !strings.Contains(body, "1.1.0") {
		t.Errorf("npm packument after the feed listed it = %s, want 1.1.0", body)
	}
	getBody(t, h, "/composer/p2/acme/other.json")
	if n := reg.fetched("/p2/acme/other.json"); n != 1 {
		t.Errorf("unlisted package fetched %d times, want once", n)
	}
	if n := reg.fetched("/p2/acme/lib.json"); n != 2 {
		t.Errorf("listed package fetched %d times, want twice", n)
	}
}

// countingFeed fails its first reads and then reports no changes.
type countingFeed struct {
	reads    atomic.Int32
	failures int32
}

func (f *countingFeed) Ecosystem() string { return "npm" }

func (f *countingFeed) Start(context.Context) (changes.Cursor, error) {
	return f.read()
}

func (f *countingFeed) Changes(_ context.Context, cursor changes.Cursor) (changes.Changes, changes.Cursor, error) {
	if _, err := f.read(); err != nil {
		return changes.Changes{}, cursor, err
	}
	return changes.Changes{}, cursor, nil
}

func (f *countingFeed) read() (changes.Cursor, error) {
	if f.reads.Add(1) <= f.failures {
		return "", errors.New("feed down")
	}
	return "1", nil
}

// fixedFeed reports the same names on every read.
type fixedFeed struct{ names []string }

func (fixedFeed) Ecosystem() string                             { return "npm" }
func (fixedFeed) Start(context.Context) (changes.Cursor, error) { return "1", nil }
func (f fixedFeed) Changes(context.Context, changes.Cursor) (changes.Changes, changes.Cursor, error) {
	return changes.Changes{Names: f.names}, "2", nil
}

// TestReadChangeFeedKeepsCursorWhenExpiryFails checks that the cursor moves
// past the changes only once they are expired, so changes whose expiry
// failed are read again.
func TestReadChangeFeedKeepsCursorWhenExpiryFails(t *testing.T) {
	db, err := database.Create(filepath.Join(t.TempDir(), "proxy.db"))
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	feed := fixedFeed{names: []string{"left-pad"}}

	if next, err := readChangeFeed(context.Background(), db, logger, feed, "1"); err != nil || next != "2" {
		t.Fatalf("read = %q, %v; want cursor 2", next, err)
	}
	_ = db.Close()
	if next, err := readChangeFeed(context.Background(), db, logger, feed, "1"); err == nil || next != "1" {
		t.Errorf("read with a failing expiry = %q, %v; want an error and cursor 1", next, err)
	}
}

// TestFollowChangeFeedStopsWithContext checks that a failing feed is retried
// rather than given up on, and that following stops when ctx ends.
func TestFollowChangeFeedStopsWithContext(t *testing.T) {
	feed := &countingFeed{failures: 2}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		followChangeFeed(ctx, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), feed, time.Millisecond)
	}()
	deadline := time.After(5 * time.Second)
	for feed.reads.Load() < 4 {
		select {
		case <-deadline:
			t.Fatalf("feed read %d times, want it read again after failing", feed.reads.Load())
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("followChangeFeed did not return after ctx ended")
	}
}
