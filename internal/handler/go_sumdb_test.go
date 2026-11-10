package handler

import (
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/mod/sumdb"
	"golang.org/x/mod/sumdb/note"
	"golang.org/x/mod/sumdb/tlog"
)

const (
	testSumDBName = "sum.example"
	// testSumDBTree looks like a signed tree as far as the proxy checks.
	testSumDBTree = "go.sum database tree\n1\nhash\n\n— sum.example signature\n"
	// testSumDBRecord looks like a lookup answer as far as the proxy checks.
	testSumDBRecord = "1\ngolang.org/x/text v0.14.0 h1:hash=\ngolang.org/x/text v0.14.0/go.mod h1:hash=\n\n" + testSumDBTree
)

// hashTile returns a hash tile body of the given width.
func hashTile(width int, fill string) string {
	return strings.Repeat(fill, width*tlog.HashSize)
}

// dataTile returns a data tile body holding width records, laid out the way
// sum.golang.org serves it: a blank line between records, none after the
// last.
func dataTile(width int) string {
	records := make([]string, width)
	for i := range records {
		records[i] = fmt.Sprintf("example.com/m%d v1.0.0 h1:hash=\nexample.com/m%d v1.0.0/go.mod h1:hash=\n", i, i)
	}
	return strings.Join(records, "\n")
}

// fakeSumDB is a checksum database upstream serving fixed files. Unknown
// paths get 404 with the body from notFound, or a default text.
type fakeSumDB struct {
	server *httptest.Server

	mu       sync.Mutex
	files    map[string]string
	notFound map[string]string
	hits     map[string]int
	down     atomic.Bool
}

func newFakeSumDB(t *testing.T, files map[string]string) *fakeSumDB {
	t.Helper()
	if files == nil {
		files = map[string]string{}
	}
	db := &fakeSumDB{files: files, notFound: map[string]string{}, hits: map[string]int{}}
	db.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		db.mu.Lock()
		db.hits[r.URL.Path]++
		body, ok := db.files[r.URL.Path]
		text, custom := db.notFound[r.URL.Path]
		db.mu.Unlock()
		if db.down.Load() {
			http.Error(w, "unavailable", http.StatusInternalServerError)
			return
		}
		if !ok {
			if !custom {
				text = "not found: " + r.URL.Path
			}
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, text)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(db.server.Close)
	return db
}

func (db *fakeSumDB) setFile(path, body string) {
	db.mu.Lock()
	defer db.mu.Unlock()
	db.files[path] = body
}

func (db *fakeSumDB) setNotFound(path, text string) {
	db.mu.Lock()
	defer db.mu.Unlock()
	db.notFound[path] = text
}

func (db *fakeSumDB) requests(path string) int {
	db.mu.Lock()
	defer db.mu.Unlock()
	return db.hits[path]
}

func (db *fakeSumDB) totalRequests() int {
	db.mu.Lock()
	defer db.mu.Unlock()
	total := 0
	for _, n := range db.hits {
		total += n
	}
	return total
}

func newSumDBTestHandler(t *testing.T, sumDBURL string, cacheMetadata bool, ttl time.Duration) *GoHandler {
	t.Helper()
	proxy, _, _, _ := setupTestProxy(t)
	proxy.HTTPClient = http.DefaultClient
	proxy.CacheMetadata = cacheMetadata
	proxy.MetadataTTL = ttl
	h := NewGoHandler(proxy, "http://proxy.example")
	h.SetSumDBs(map[string]string{testSumDBName: sumDBURL}, false)
	return h
}

func serveGoRequest(h *GoHandler, target string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, httptest.NewRequest(http.MethodGet, target, nil))
	return w
}

func TestGoSumDB_DefaultsToSumGolangOrg(t *testing.T) {
	h := NewGoHandler(nil, "http://proxy.example")
	if got := h.sumDBs[defaultGoSumDBName]; got != defaultGoSumDBURL {
		t.Errorf("default checksum database = %q, want %q", got, defaultGoSumDBURL)
	}
	h.SetSumDBs(nil, false)
	if got := h.sumDBs[defaultGoSumDBName]; got != defaultGoSumDBURL || len(h.sumDBs) != 1 {
		t.Errorf("SetSumDBs(nil) = %v, want only the default", h.sumDBs)
	}
}

func TestGoSumDB_SupportedProbe(t *testing.T) {
	db := newFakeSumDB(t, map[string]string{"/latest": testSumDBTree})
	h := newSumDBTestHandler(t, db.server.URL, true, time.Hour)

	for range 2 {
		for target, want := range map[string]int{
			"/sumdb/sum.example/supported":    http.StatusOK,
			"/sumdb/sum.golang.org/supported": http.StatusNotFound,
			"/sumdb/other.example/supported":  http.StatusNotFound,
		} {
			if w := serveGoRequest(h, target); w.Code != want {
				t.Errorf("%s: status = %d, want %d", target, w.Code, want)
			}
		}
	}
	if n := db.totalRequests(); n != 1 {
		t.Errorf("upstream requests = %d, want 1: the probe fetches latest once, then uses the cache", n)
	}
	if h.sumDBProbes.recentlyFailed(testSumDBName) {
		t.Error("a probe served from the fresh cache was recorded as a failure")
	}
}

// TestGoSumDB_SupportedFallsBackWhenUnreachable covers the go command's only
// way back to the database: once supported answers 200 it never contacts the
// database directly, so a database the proxy cannot reach must get 404.
func TestGoSumDB_SupportedFallsBackWhenUnreachable(t *testing.T) {
	for _, cacheMetadata := range []bool{true, false} {
		t.Run(fmt.Sprintf("cache_metadata=%v", cacheMetadata), func(t *testing.T) {
			db := newFakeSumDB(t, map[string]string{"/latest": testSumDBTree})
			db.down.Store(true)
			h := newSumDBTestHandler(t, db.server.URL, cacheMetadata, time.Hour)

			for i := range 2 {
				if w := serveGoRequest(h, "/sumdb/sum.example/supported"); w.Code != http.StatusNotFound {
					t.Fatalf("request %d: status = %d, want 404", i, w.Code)
				}
			}
			if n := db.requests("/latest"); n != 1 {
				t.Errorf("probes = %d, want 1: a failed probe is not repeated within %s", n, goSumDBProbeBackoff)
			}
		})
	}
}

// TestGoSumDB_SupportedWithCachedTreeWhileUnreachable keeps the offline case:
// a proxy that has served the database before can still serve it from cache.
func TestGoSumDB_SupportedWithCachedTreeWhileUnreachable(t *testing.T) {
	t.Run("fresh tree", func(t *testing.T) {
		db := newFakeSumDB(t, map[string]string{"/latest": testSumDBTree})
		h := newSumDBTestHandler(t, db.server.URL, true, time.Hour)
		if w := serveGoRequest(h, "/sumdb/sum.example/latest"); w.Code != http.StatusOK {
			t.Fatalf("latest: status = %d, want 200", w.Code)
		}
		db.down.Store(true)
		if w := serveGoRequest(h, "/sumdb/sum.example/supported"); w.Code != http.StatusOK {
			t.Errorf("supported with a fresh cached tree: status = %d, want 200", w.Code)
		}
	})

	// A stale tree only counts with go_sumdbs_offline. Without it, clients
	// that can reach the database themselves should go there.
	for _, offline := range []bool{true, false} {
		t.Run(fmt.Sprintf("stale tree offline=%v", offline), func(t *testing.T) {
			db := newFakeSumDB(t, map[string]string{"/latest": testSumDBTree})
			h := newSumDBTestHandler(t, db.server.URL, true, time.Nanosecond)
			h.sumDBOffline = offline
			if w := serveGoRequest(h, "/sumdb/sum.example/latest"); w.Code != http.StatusOK {
				t.Fatalf("latest: status = %d, want 200", w.Code)
			}
			db.down.Store(true)
			want := http.StatusNotFound
			if offline {
				want = http.StatusOK
			}
			for i := range 2 { // second probe hits the remembered failure
				if w := serveGoRequest(h, "/sumdb/sum.example/supported"); w.Code != want {
					t.Errorf("probe %d: status = %d, want %d", i, w.Code, want)
				}
			}
			if n := db.requests("/latest"); n != 2 {
				t.Errorf("upstream requests = %d, want 2: the failure is remembered", n)
			}
		})
	}
}

// TestGoSumDB_SupportedGivesUpOnSilentDatabase: a database that accepts the
// connection but never answers (a firewall dropping packets) must not hold
// the go command, which waits for supported without a timeout of its own.
func TestGoSumDB_SupportedGivesUpOnSilentDatabase(t *testing.T) {
	release := make(chan struct{})
	db := newFakeSumDB(t, nil)
	t.Cleanup(func() { close(release) }) // runs before the server closes
	db.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	})
	h := newSumDBTestHandler(t, db.server.URL, true, time.Hour)
	h.sumDBProbes.timeout = 50 * time.Millisecond

	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() { answered <- serveGoRequest(h, "/sumdb/sum.example/supported") }()
	select {
	case w := <-answered:
		if w.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", w.Code)
		}
	case <-time.After(time.Second):
		t.Fatal("supported still waiting for the database after a second")
	}
}

// TestGoSumDB_SupportedWithStaleTreeWhileSilent: the fetch only falls back
// to a stale cached tree once upstream has failed, which a silent database
// delays past the probe timeout. The proxy can still serve from cache, so
// supported must say so.
func TestGoSumDB_SupportedWithStaleTreeWhileSilent(t *testing.T) {
	release := make(chan struct{})
	db := newFakeSumDB(t, map[string]string{"/latest": testSumDBTree})
	t.Cleanup(func() { close(release) })
	h := newSumDBTestHandler(t, db.server.URL, true, time.Nanosecond)
	h.sumDBOffline = true
	h.sumDBProbes.timeout = 50 * time.Millisecond

	if w := serveGoRequest(h, "/sumdb/sum.example/latest"); w.Code != http.StatusOK {
		t.Fatalf("latest: status = %d, want 200", w.Code)
	}
	db.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	})

	for i := range 2 { // second probe hits the remembered failure
		answered := make(chan *httptest.ResponseRecorder, 1)
		go func() { answered <- serveGoRequest(h, "/sumdb/sum.example/supported") }()
		select {
		case w := <-answered:
			if w.Code != http.StatusOK {
				t.Fatalf("probe %d: status = %d, want 200 from the cached tree", i, w.Code)
			}
		case <-time.After(time.Second):
			t.Fatalf("probe %d: supported still waiting after a second", i)
		}
	}
}

// TestGoSumDB_SlowProbeSuccessClearsFailure: a database that answers after
// the probe timeout gets a 404 the first time, but once its answer is in
// the cache, supported says yes without waiting for the next backoff.
func TestGoSumDB_SlowProbeSuccessClearsFailure(t *testing.T) {
	db := newFakeSumDB(t, map[string]string{"/latest": testSumDBTree})
	inner := db.server.Config.Handler
	db.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
		inner.ServeHTTP(w, r)
	})
	h := newSumDBTestHandler(t, db.server.URL, true, time.Hour)
	h.sumDBProbes.timeout = 10 * time.Millisecond

	if w := serveGoRequest(h, "/sumdb/sum.example/supported"); w.Code != http.StatusNotFound {
		t.Fatalf("first probe: status = %d, want 404", w.Code)
	}
	deadline := time.Now().Add(5 * time.Second)
	for h.sumDBProbes.recentlyFailed(testSumDBName) {
		if time.Now().After(deadline) {
			t.Fatal("the late fetch never cleared the failure")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if w := serveGoRequest(h, "/sumdb/sum.example/supported"); w.Code != http.StatusOK {
		t.Errorf("after the late answer: status = %d, want 200", w.Code)
	}
	if n := db.requests("/latest"); n != 1 {
		t.Errorf("upstream requests = %d, want 1: the second probe is a cache hit", n)
	}
}

// TestGoSumDB_StaleFallbackIsNotAnAnswer: the fetch reports no error when
// it falls back to the cached tree after the database failed. That must
// not count as the database answering, or the failure would be forgotten
// and every probe would wait for the timeout again.
func TestGoSumDB_StaleFallbackIsNotAnAnswer(t *testing.T) {
	db := newFakeSumDB(t, map[string]string{"/latest": testSumDBTree})
	h := newSumDBTestHandler(t, db.server.URL, true, time.Nanosecond)
	h.sumDBOffline = true
	h.sumDBProbes.timeout = 10 * time.Millisecond

	if w := serveGoRequest(h, "/sumdb/sum.example/latest"); w.Code != http.StatusOK {
		t.Fatalf("latest: status = %d, want 200", w.Code)
	}
	inner := db.server.Config.Handler
	db.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
		inner.ServeHTTP(w, r)
	})
	db.down.Store(true)

	if w := serveGoRequest(h, "/sumdb/sum.example/supported"); w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 from the cached tree", w.Code)
	}
	time.Sleep(300 * time.Millisecond) // the fetch has failed and fallen back by now
	if !h.sumDBProbes.recentlyFailed(testSumDBName) {
		t.Error("the stale fallback cleared the remembered failure")
	}
}

// TestGoSumDB_AnswerCountsWhenCacheWriteFails: an answer from the database
// is an answer even when it could not be cached afterwards, whether or not
// a stale tree was cached before.
func TestGoSumDB_AnswerCountsWhenCacheWriteFails(t *testing.T) {
	for _, staleTree := range []bool{false, true} {
		t.Run(fmt.Sprintf("stale tree cached=%v", staleTree), func(t *testing.T) {
			db := newFakeSumDB(t, map[string]string{"/latest": testSumDBTree})
			h := newSumDBTestHandler(t, db.server.URL, true, time.Nanosecond)
			if staleTree {
				if w := serveGoRequest(h, "/sumdb/sum.example/latest"); w.Code != http.StatusOK {
					t.Fatalf("latest: status = %d, want 200", w.Code)
				}
			}
			h.proxy.Storage.(*mockStorage).storeErr = errors.New("disk full")

			if w := serveGoRequest(h, "/sumdb/sum.example/supported"); w.Code != http.StatusOK {
				t.Errorf("status = %d, want 200: the database answered", w.Code)
			}
			if h.sumDBProbes.recentlyFailed(testSumDBName) {
				t.Error("an answered probe was recorded as a failure")
			}
		})
	}
}

// TestGoSumDB_OfflineServesCacheWithoutWaiting: in offline mode, once the
// database is known to be unreachable, stale lookups must come from the
// cache at once instead of waiting for the upstream timeout each time.
func TestGoSumDB_OfflineServesCacheWithoutWaiting(t *testing.T) {
	const lookup = "/lookup/golang.org/x/text@v0.14.0"
	release := make(chan struct{})
	db := newFakeSumDB(t, map[string]string{"/latest": testSumDBTree, lookup: testSumDBRecord})
	t.Cleanup(func() { close(release) })
	h := newSumDBTestHandler(t, db.server.URL, true, time.Nanosecond)
	h.sumDBOffline = true
	h.sumDBProbes.timeout = 50 * time.Millisecond

	for _, path := range []string{"/latest", lookup} {
		if w := serveGoRequest(h, "/sumdb/sum.example"+path); w.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200", path, w.Code)
		}
	}
	db.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	})

	for _, path := range []string{"/supported", "/latest", lookup} {
		answered := make(chan *httptest.ResponseRecorder, 1)
		go func() { answered <- serveGoRequest(h, "/sumdb/sum.example"+path) }()
		select {
		case w := <-answered:
			if w.Code != http.StatusOK {
				t.Errorf("%s: status = %d, want 200 from the cache", path, w.Code)
			}
		case <-time.After(time.Second):
			t.Fatalf("%s: still waiting for the database after a second", path)
		}
	}
}

// TestGoSumDB_StaleLookupRemembersFailure: the go command asks supported
// only once per process. A lookup that falls back to the cache must renew
// the remembered failure itself, or offline mode would wait for the upstream
// timeout on every lookup a minute into a long build.
func TestGoSumDB_StaleLookupRemembersFailure(t *testing.T) {
	const lookup = "/lookup/golang.org/x/text@v0.14.0"
	db := newFakeSumDB(t, map[string]string{lookup: testSumDBRecord})
	h := newSumDBTestHandler(t, db.server.URL, true, time.Nanosecond)
	h.sumDBOffline = true

	if w := serveGoRequest(h, "/sumdb/sum.example"+lookup); w.Code != http.StatusOK {
		t.Fatalf("online: status = %d, want 200", w.Code)
	}
	if h.sumDBProbes.recentlyFailed(testSumDBName) {
		t.Fatal("an answered lookup was recorded as a failure")
	}
	db.down.Store(true)
	if w := serveGoRequest(h, "/sumdb/sum.example"+lookup); w.Code != http.StatusOK || w.Body.String() != testSumDBRecord {
		t.Fatalf("outage: status = %d, body = %q, want the cached record", w.Code, w.Body.String())
	}
	if !h.sumDBProbes.recentlyFailed(testSumDBName) {
		t.Error("a lookup served from the cache after the database failed did not remember the failure")
	}
}

// TestGoSumDB_LateStaleLookupKeepsNewerSuccess: a lookup sent while the
// database was failing, which falls back to the cache only after a probe has
// reached the database again, must not mark it unreachable.
func TestGoSumDB_LateStaleLookupKeepsNewerSuccess(t *testing.T) {
	const lookup = "/lookup/golang.org/x/text@v0.14.0"
	db := newFakeSumDB(t, map[string]string{"/latest": testSumDBTree, lookup: testSumDBRecord})
	h := newSumDBTestHandler(t, db.server.URL, true, time.Nanosecond)
	if w := serveGoRequest(h, "/sumdb/sum.example"+lookup); w.Code != http.StatusOK {
		t.Fatalf("lookup: status = %d, want 200", w.Code)
	}

	var sentOnce sync.Once
	sent := make(chan struct{})
	release := make(chan struct{})
	inner := db.server.Config.Handler
	db.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == lookup {
			sentOnce.Do(func() { close(sent) })
			<-release
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		inner.ServeHTTP(w, r)
	})
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- serveGoRequest(h, "/sumdb/sum.example"+lookup) }()
	<-sent
	if w := serveGoRequest(h, "/sumdb/sum.example/supported"); w.Code != http.StatusOK {
		t.Fatalf("supported: status = %d, want 200", w.Code)
	}
	close(release)
	if w := <-done; w.Code != http.StatusOK || w.Body.String() != testSumDBRecord {
		t.Fatalf("lookup: status = %d, body = %q, want the cached record", w.Code, w.Body.String())
	}
	if h.sumDBProbes.recentlyFailed(testSumDBName) {
		t.Error("a lookup that fell back after the probe succeeded marked the database unreachable")
	}
}

// TestGoSumDB_ShortcutNeedsOfflineMode: without go_sumdbs_offline, a
// remembered failure does not keep cached lookups from asking the database,
// which may well be back.
func TestGoSumDB_ShortcutNeedsOfflineMode(t *testing.T) {
	const lookup = "/lookup/golang.org/x/text@v0.14.0"
	db := newFakeSumDB(t, map[string]string{lookup: testSumDBRecord})
	h := newSumDBTestHandler(t, db.server.URL, true, time.Nanosecond)
	if w := serveGoRequest(h, "/sumdb/sum.example"+lookup); w.Code != http.StatusOK {
		t.Fatalf("lookup: status = %d, want 200", w.Code)
	}

	h.sumDBProbes.record(testSumDBName, false)
	if w := serveGoRequest(h, "/sumdb/sum.example"+lookup); w.Code != http.StatusOK {
		t.Fatalf("lookup with a remembered failure: status = %d, want 200", w.Code)
	}
	if n := db.requests(lookup); n != 2 {
		t.Errorf("database requests = %d, want 2: the cached lookup was served without asking", n)
	}
}

// TestGoSumDB_FreshTreeIsNotAnAnswer: a supported check answered from a
// fresh cached tree did not reach the database, so it must not cancel a
// failure that a lookup started earlier reports afterwards.
func TestGoSumDB_FreshTreeIsNotAnAnswer(t *testing.T) {
	db := newFakeSumDB(t, map[string]string{"/latest": testSumDBTree})
	h := newSumDBTestHandler(t, db.server.URL, true, time.Hour)
	if w := serveGoRequest(h, "/sumdb/sum.example/supported"); w.Code != http.StatusOK {
		t.Fatalf("supported: status = %d, want 200", w.Code)
	}

	// Coarse clocks (Windows) can give the first probe's success and start
	// the same time; start must come after it.
	h.sumDBProbes.mu.Lock()
	succeeded := h.sumDBProbes.succeeded[testSumDBName]
	h.sumDBProbes.mu.Unlock()
	start := time.Now()
	for !start.After(succeeded) {
		start = time.Now()
	}
	db.down.Store(true)
	if w := serveGoRequest(h, "/sumdb/sum.example/supported"); w.Code != http.StatusOK {
		t.Fatalf("supported with a fresh cached tree: status = %d, want 200", w.Code)
	}
	h.sumDBProbes.recordFailure(testSumDBName, start)
	if !h.sumDBProbes.recentlyFailed(testSumDBName) {
		t.Error("a supported check answered from the cache counted as reaching the database")
	}
}

func TestGoSumDBProbes_TimeoutDoesNotOverwriteLaterSuccess(t *testing.T) {
	var probes goSumDBProbes
	start := time.Now()
	probes.record(testSumDBName, true)
	probes.recordFailure(testSumDBName, start)
	if probes.recentlyFailed(testSumDBName) {
		t.Error("a success recorded after the probe started was overwritten by its timeout")
	}
	probes.recordFailure(testSumDBName, time.Now().Add(time.Second))
	if !probes.recentlyFailed(testSumDBName) {
		t.Error("a timeout with no later success was not recorded")
	}
}

// TestGoSumDB_SupportedRefusesBlockPage: egress proxies answer blocked hosts
// with a 200 page. That is not a reachable database, and must not be cached
// as its signed tree either.
func TestGoSumDB_SupportedRefusesBlockPage(t *testing.T) {
	db := newFakeSumDB(t, map[string]string{"/latest": "<html>access to this site is blocked</html>"})
	h := newSumDBTestHandler(t, db.server.URL, true, time.Hour)

	if w := serveGoRequest(h, "/sumdb/sum.example/supported"); w.Code != http.StatusNotFound {
		t.Errorf("supported: status = %d, want 404", w.Code)
	}
	if w := serveGoRequest(h, "/sumdb/sum.example/latest"); w.Code != http.StatusBadGateway {
		t.Errorf("latest: status = %d, want 502 for a block page", w.Code)
	}

	// The page was not cached: once the database answers, its tree is served.
	db.setFile("/latest", testSumDBTree)
	w := serveGoRequest(h, "/sumdb/sum.example/latest")
	if w.Code != http.StatusOK || w.Body.String() != testSumDBTree {
		t.Errorf("latest after recovery: status = %d, body = %q, want the signed tree", w.Code, w.Body.String())
	}
}

// TestGoSumDB_LookupRefusesBlockPage: a block page in place of a lookup must
// not be cached, where it would replace the copy served during an outage.
// That includes a page that happens to start like a record.
func TestGoSumDB_LookupRefusesBlockPage(t *testing.T) {
	const lookup = "/lookup/golang.org/x/text@v0.14.0"
	pages := map[string]string{
		"html":                "<html>access to this site is blocked</html>",
		"record without tree": "1\ngolang.org/x/text v0.14.0 h1:hash=\n\n<html>access to this site is blocked</html>",
	}
	for name, page := range pages {
		t.Run(name, func(t *testing.T) {
			db := newFakeSumDB(t, map[string]string{lookup: testSumDBRecord})
			h := newSumDBTestHandler(t, db.server.URL, true, time.Nanosecond)

			if w := serveGoRequest(h, "/sumdb/sum.example"+lookup); w.Code != http.StatusOK {
				t.Fatalf("lookup: status = %d, want 200", w.Code)
			}
			db.setFile(lookup, page)
			if w := serveGoRequest(h, "/sumdb/sum.example"+lookup); w.Code != http.StatusOK || w.Body.String() != testSumDBRecord {
				t.Errorf("behind a block page: status = %d, body = %q, want the cached record", w.Code, w.Body.String())
			}

			// The page did not replace the record: it is still there in an outage.
			db.down.Store(true)
			if w := serveGoRequest(h, "/sumdb/sum.example"+lookup); w.Code != http.StatusOK || w.Body.String() != testSumDBRecord {
				t.Errorf("in an outage: status = %d, body = %q, want the cached record", w.Code, w.Body.String())
			}
		})
	}
}

func TestGoSumDB_RequestsGoToTheDatabase(t *testing.T) {
	var moduleUpstreamRequests atomic.Int32
	moduleUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		moduleUpstreamRequests.Add(1)
		http.NotFound(w, r)
	}))
	defer moduleUpstream.Close()

	const lookup = "/lookup/golang.org/x/text@v0.14.0"
	db := newFakeSumDB(t, map[string]string{lookup: testSumDBRecord})

	proxy, _, _, _ := setupTestProxy(t)
	proxy.HTTPClient = http.DefaultClient
	h := NewGoHandlerWithUpstream(proxy, "http://proxy.example", moduleUpstream.URL)
	h.SetSumDBs(map[string]string{testSumDBName: db.server.URL + "/"}, false)

	w := serveGoRequest(h, "/sumdb/sum.example"+lookup)
	if w.Code != http.StatusOK || w.Body.String() != testSumDBRecord {
		t.Fatalf("status = %d, body = %q; want 200 with the database's response", w.Code, w.Body.String())
	}
	if n := db.requests(lookup); n != 1 {
		t.Errorf("database requests for %s = %d, want 1", lookup, n)
	}
	if n := moduleUpstreamRequests.Load(); n != 0 {
		t.Errorf("module upstream requests = %d, want 0", n)
	}
}

func TestGoSumDB_LookupCachedWithinTTL(t *testing.T) {
	const lookup = "/lookup/golang.org/x/text@v0.14.0"
	db := newFakeSumDB(t, map[string]string{lookup: testSumDBRecord})
	h := newSumDBTestHandler(t, db.server.URL, true, time.Hour)

	for i := range 2 {
		w := serveGoRequest(h, "/sumdb/sum.example"+lookup)
		if w.Code != http.StatusOK || w.Body.String() != testSumDBRecord {
			t.Fatalf("request %d: status = %d, body = %q", i, w.Code, w.Body.String())
		}
	}
	if n := db.requests(lookup); n != 1 {
		t.Errorf("database requests = %d, want 1: the second lookup is a cache hit", n)
	}
}

func TestGoSumDB_CompleteTilesSkipRevalidation(t *testing.T) {
	files := map[string]string{
		"/tile/8/0/000":     hashTile(256, "a"),
		"/tile/8/0/001.p/5": hashTile(5, "b"),
		"/latest":           testSumDBTree,
	}
	wantRequests := map[string]int{
		"/tile/8/0/000":     1,
		"/tile/8/0/001.p/5": 2,
		"/latest":           2,
	}
	db := newFakeSumDB(t, files)
	// Every cached entry is past its TTL by the second request.
	h := newSumDBTestHandler(t, db.server.URL, true, time.Nanosecond)

	for range 2 {
		for path, want := range files {
			w := serveGoRequest(h, "/sumdb/sum.example"+path)
			if w.Code != http.StatusOK || w.Body.String() != want {
				t.Fatalf("%s: status = %d, body = %q, want %q", path, w.Code, w.Body.String(), want)
			}
		}
	}
	for path, want := range wantRequests {
		if n := db.requests(path); n != want {
			t.Errorf("%s: database requests = %d, want %d", path, n, want)
		}
	}
}

// TestGoSumDB_DataTilesAreNotStored: the go command never fetches data
// tiles, and stored they would add up to the whole log.
func TestGoSumDB_DataTilesAreNotStored(t *testing.T) {
	files := map[string]string{
		"/tile/8/data/000":     dataTile(256),
		"/tile/8/data/001.p/5": dataTile(5),
	}
	db := newFakeSumDB(t, files)
	h := newSumDBTestHandler(t, db.server.URL, true, time.Hour)

	for range 2 {
		for path, want := range files {
			w := serveGoRequest(h, "/sumdb/sum.example"+path)
			if w.Code != http.StatusOK || w.Body.String() != want {
				t.Fatalf("%s: status = %d, body length %d, want the %d-byte tile", path, w.Code, w.Body.Len(), len(want))
			}
		}
	}
	for path := range files {
		if n := db.requests(path); n != 2 {
			t.Errorf("%s: database requests = %d, want 2", path, n)
		}
		key := goSumDBCacheKey(testSumDBName, db.server.URL, strings.TrimPrefix(path, "/"))
		if entry, _ := h.proxy.DB.GetMetadataCache(goEcosystem, key); entry != nil {
			t.Errorf("%s: stored in the metadata cache", path)
		}
	}
}

// TestGoSumDB_BadTilesAreNotCached guards complete tiles, which are never
// revalidated once cached: a response of the wrong size must not be stored.
func TestGoSumDB_BadTilesAreNotCached(t *testing.T) {
	const tile = "/tile/8/0/000"
	db := newFakeSumDB(t, map[string]string{
		tile: "<html>please log in</html>",
	})
	h := newSumDBTestHandler(t, db.server.URL, true, time.Hour)

	if w := serveGoRequest(h, "/sumdb/sum.example"+tile); w.Code != http.StatusBadGateway {
		t.Fatalf("wrong-size tile: status = %d, want 502", w.Code)
	}

	want := hashTile(256, "a")
	db.setFile(tile, want)
	w := serveGoRequest(h, "/sumdb/sum.example"+tile)
	if w.Code != http.StatusOK || w.Body.String() != want {
		t.Fatalf("after upstream recovered: status = %d, body length %d, want the %d-byte tile", w.Code, w.Body.Len(), len(want))
	}
	if n := db.requests(tile); n != 2 {
		t.Errorf("upstream requests = %d, want 2: the bad response was not cached", n)
	}
}

// TestGoSumDB_DamagedCachedTileIsFetchedAgain guards the check on the cached
// copy: a complete tile is never revalidated, so a damaged blob would
// otherwise be served for good. The database sends an ETag, so past the TTL
// a request that asks whether the tile changed would get 304 and the damaged
// copy back.
func TestGoSumDB_DamagedCachedTileIsFetchedAgain(t *testing.T) {
	const tile = "/tile/8/0/000"
	want := hashTile(256, "a")
	for _, ttl := range []time.Duration{time.Hour, time.Nanosecond} {
		t.Run(fmt.Sprintf("ttl=%s", ttl), func(t *testing.T) {
			db := newFakeSumDB(t, map[string]string{tile: want})
			inner := db.server.Config.Handler
			db.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("ETag", `"tile"`)
				if r.Header.Get("If-None-Match") == `"tile"` {
					w.WriteHeader(http.StatusNotModified)
					return
				}
				inner.ServeHTTP(w, r)
			})
			h := newSumDBTestHandler(t, db.server.URL, true, ttl)

			if w := serveGoRequest(h, "/sumdb/sum.example"+tile); w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", w.Code)
			}

			store := h.proxy.Storage.(*mockStorage)
			blob := metadataStoragePath(goEcosystem, goSumDBCacheKey(testSumDBName, db.server.URL, "tile/8/0/000"))
			store.mu.Lock()
			if _, ok := store.files[blob]; !ok {
				store.mu.Unlock()
				t.Fatalf("tile not stored at %s", blob)
			}
			store.files[blob] = []byte(want[:100])
			store.mu.Unlock()

			w := serveGoRequest(h, "/sumdb/sum.example"+tile)
			if w.Code != http.StatusOK || w.Body.String() != want {
				t.Fatalf("after damage: status = %d, body length %d, want the %d-byte tile", w.Code, w.Body.Len(), len(want))
			}
			if n := db.requests(tile); n != 2 {
				t.Errorf("upstream requests = %d, want 2: the damaged copy was fetched again", n)
			}
		})
	}
}

func TestGoSumDB_CompleteTilesNotStoredWithoutMetadataCache(t *testing.T) {
	const tile = "/tile/8/0/000"
	db := newFakeSumDB(t, map[string]string{tile: hashTile(256, "a")})
	h := newSumDBTestHandler(t, db.server.URL, false, time.Hour)

	for range 2 {
		if w := serveGoRequest(h, "/sumdb/sum.example"+tile); w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", w.Code)
		}
	}
	if n := db.requests(tile); n != 2 {
		t.Errorf("database requests = %d, want 2 with cache_metadata off", n)
	}
}

func TestGoSumDB_OfflineFallback(t *testing.T) {
	files := map[string]string{
		"/lookup/golang.org/x/text@v0.14.0": testSumDBRecord,
		"/latest":                           testSumDBTree,
		"/tile/8/0/000":                     hashTile(256, "a"),
		"/tile/8/0/001.p/5":                 hashTile(5, "b"),
	}
	db := newFakeSumDB(t, files)
	h := newSumDBTestHandler(t, db.server.URL, true, time.Nanosecond)

	for path := range files {
		if w := serveGoRequest(h, "/sumdb/sum.example"+path); w.Code != http.StatusOK {
			t.Fatalf("%s online: status = %d, want 200", path, w.Code)
		}
	}

	db.down.Store(true)
	for path, want := range files {
		w := serveGoRequest(h, "/sumdb/sum.example"+path)
		if w.Code != http.StatusOK || w.Body.String() != want {
			t.Errorf("%s offline: status = %d, body = %q, want 200 with %q", path, w.Code, w.Body.String(), want)
		}
	}
}

func TestGoSumDB_NotFoundRelaysUpstreamText(t *testing.T) {
	const lookup = "/lookup/github.com/google/uuid@v9.9.9"
	const reason = "not found: github.com/google/uuid@v9.9.9: invalid version: unknown revision v9.9.9"

	for _, cacheMetadata := range []bool{true, false} {
		t.Run(fmt.Sprintf("cache_metadata=%v", cacheMetadata), func(t *testing.T) {
			db := newFakeSumDB(t, nil)
			db.setNotFound(lookup, reason)
			h := newSumDBTestHandler(t, db.server.URL, cacheMetadata, time.Hour)

			w := serveGoRequest(h, "/sumdb/sum.example"+lookup)
			if w.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404", w.Code)
			}
			if got := strings.TrimSpace(w.Body.String()); got != reason {
				t.Errorf("body = %q, want the database's text %q", got, reason)
			}
		})
	}

	t.Run("long text is cut", func(t *testing.T) {
		db := newFakeSumDB(t, nil)
		db.setNotFound(lookup, strings.Repeat("x", upstreamNotFoundBodyLimit+1000))
		h := newSumDBTestHandler(t, db.server.URL, true, time.Hour)

		w := serveGoRequest(h, "/sumdb/sum.example"+lookup)
		if w.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", w.Code)
		}
		if got := w.Body.Len(); got != upstreamNotFoundBodyLimit {
			t.Errorf("body length = %d, want %d", got, upstreamNotFoundBodyLimit)
		}
	})
}

func TestGoSumDB_InvalidPathsNeverReachUpstream(t *testing.T) {
	db := newFakeSumDB(t, nil)
	h := newSumDBTestHandler(t, db.server.URL, true, time.Hour)

	for _, target := range []string{
		"/sumdb/sum.example",
		"/sumdb/sum.example/",
		"/sumdb/sum.example/other",
		"/sumdb/sum.example/latest/extra",
		"/sumdb/sum.example/lookup/",
		"/sumdb/sum.example/lookup/golang.org/x/text",
		"/sumdb/sum.example/lookup/golang.org/x/text@",
		"/sumdb/sum.example/lookup/golang.org/x/text@master",
		"/sumdb/sum.example/lookup/github.com/Google/uuid@v1.6.0",
		"/sumdb/sum.example/lookup/../../etc/passwd@v1.0.0",
		"/sumdb/sum.example/tile/x/0/000",
		"/sumdb/sum.example/tile/8/0/000.p/0",
		"/sumdb/sum.example/tile/8/0/000.p/256",
		"/sumdb/sum.example/tile/8/0/../../../latest",
		"/sumdb/unknown.example/latest",
	} {
		if w := serveGoRequest(h, target); w.Code != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404", target, w.Code)
		}
	}
	if n := db.totalRequests(); n != 0 {
		t.Errorf("upstream requests = %d, want 0", n)
	}
}

// sumDBClientOps lets the x/mod checksum database client read through the
// proxy, keeping its configuration and cache in memory.
type sumDBClientOps struct {
	t      *testing.T
	remote string
	key    string

	mu     sync.Mutex
	config map[string][]byte
	cache  map[string][]byte
}

func newSumDBClientOps(t *testing.T, remote, key string) *sumDBClientOps {
	return &sumDBClientOps{t: t, remote: remote, key: key, config: map[string][]byte{}, cache: map[string][]byte{}}
}

func (o *sumDBClientOps) ReadRemote(path string) ([]byte, error) {
	resp, err := http.Get(o.remote + path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s: %s", path, resp.Status, body)
	}
	return body, nil
}

func (o *sumDBClientOps) ReadConfig(file string) ([]byte, error) {
	if file == "key" {
		return []byte(o.key), nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.config[file], nil
}

func (o *sumDBClientOps) WriteConfig(file string, old, data []byte) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if !bytes.Equal(o.config[file], old) {
		return sumdb.ErrWriteConflict
	}
	o.config[file] = data
	return nil
}

func (o *sumDBClientOps) ReadCache(file string) ([]byte, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	data, ok := o.cache[file]
	if !ok {
		return nil, errors.New("not cached")
	}
	return data, nil
}

func (o *sumDBClientOps) WriteCache(file string, data []byte) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.cache[file] = data
}

func (o *sumDBClientOps) Log(msg string) { o.t.Log(msg) }

func (o *sumDBClientOps) SecurityError(msg string) {
	o.t.Errorf("checksum database security error: %s", msg)
}

// TestGoSumDB_ClientRoundTrip runs the go command's checksum database client
// against a real database implementation through the proxy, so signatures,
// record proofs and tile hashes are checked end to end. A second client then
// verifies the same lookup from the proxy's cache with the database down.
func TestGoSumDB_ClientRoundTrip(t *testing.T) {
	signerKey, verifierKey, err := note.GenerateKey(rand.Reader, testSumDBName)
	if err != nil {
		t.Fatal(err)
	}
	gosum := func(path, version string) ([]byte, error) {
		return fmt.Appendf(nil, "%s %s h1:Y29udGVudA==\n%s %s/go.mod h1:Z28ubW9k\n", path, version, path, version), nil
	}
	database := sumdb.NewServer(sumdb.NewTestServer(signerKey, gosum))
	var down atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if down.Load() {
			http.Error(w, "unavailable", http.StatusInternalServerError)
			return
		}
		database.ServeHTTP(w, r)
	}))
	defer upstream.Close()

	h := newSumDBTestHandler(t, upstream.URL, true, time.Hour)
	proxy := httptest.NewServer(h.Routes())
	defer proxy.Close()
	remote := proxy.URL + "/sumdb/" + testSumDBName

	client := sumdb.NewClient(newSumDBClientOps(t, remote, verifierKey))
	for _, m := range [][2]string{{"rsc.io/quote", "v1.5.2"}, {"golang.org/x/text", "v0.3.0"}, {"github.com/Google/UUID", "v1.6.0"}} {
		lines, err := client.Lookup(m[0], m[1])
		if err != nil {
			t.Fatalf("lookup %s@%s through the proxy: %v", m[0], m[1], err)
		}
		if want := m[0] + " " + m[1] + " h1:Y29udGVudA=="; len(lines) != 1 || lines[0] != want {
			t.Errorf("lookup %s@%s = %q, want [%q]", m[0], m[1], lines, want)
		}
	}

	down.Store(true)
	offline := sumdb.NewClient(newSumDBClientOps(t, remote, verifierKey))
	if _, err := offline.Lookup("rsc.io/quote", "v1.5.2"); err != nil {
		t.Fatalf("lookup from the proxy cache with the database down: %v", err)
	}
}

// TestGoSumDB_OfflineCopyMarkedStaleWithoutTTL: with metadata_ttl 0 the
// cached copy offline mode serves while the database is unreachable still
// carries the Warning header.
func TestGoSumDB_OfflineCopyMarkedStaleWithoutTTL(t *testing.T) {
	const lookup = "/lookup/golang.org/x/text@v0.14.0"
	db := newFakeSumDB(t, map[string]string{lookup: testSumDBRecord})
	h := newSumDBTestHandler(t, db.server.URL, true, 0)
	h.sumDBOffline = true

	if w := serveGoRequest(h, "/sumdb/sum.example"+lookup); w.Code != http.StatusOK {
		t.Fatalf("online: status = %d, want 200", w.Code)
	}
	db.down.Store(true)
	serveGoRequest(h, "/sumdb/sum.example"+lookup)
	if !h.sumDBProbes.recentlyFailed(testSumDBName) {
		t.Fatal("the failure was not remembered")
	}
	w := serveGoRequest(h, "/sumdb/sum.example"+lookup)
	if w.Code != http.StatusOK || w.Header().Get("Warning") != staleWarning {
		t.Errorf("offline copy = %d Warning %q, want 200 %q", w.Code, w.Header().Get("Warning"), staleWarning)
	}
}
