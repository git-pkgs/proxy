package gitcache

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

// warmTestCache is a cache with one cloned repository, "up/repo.git", that
// clients last used an hour ago and that was last checked two hours ago:
// due for a background refresh with a StaleMaxAge of one hour.
func warmTestCache(t *testing.T, configure func(*Options)) (*fakeUpstream, string, *testCache, func(string) bool) {
	t.Helper()
	upstream := newFakeUpstream(t)
	work := upstream.newRepo("repo")
	tc := newTestCache(t, upstream.server.URL, func(o *Options) {
		o.WarmMaxIdle = 24 * time.Hour
		if configure != nil {
			configure(o)
		}
	})
	tc.clone(t, "repo")
	waitIdle(t, tc.Cache)
	rp := tc.repo("up/repo.git")
	rp.lastUsed.Store(time.Now().Add(-time.Hour).UnixNano())
	rp.lastChecked.Store(time.Now().Add(-2 * time.Hour).UnixNano())
	serves := func(path string) bool {
		if path != "up/repo.git" {
			t.Errorf("serves(%q), want up/repo.git", path)
		}
		return true
	}
	return upstream, work, tc, serves
}

func TestWarmPassRefreshesARepositoryDueForACheck(t *testing.T) {
	upstream, work, tc, serves := warmTestCache(t, nil)
	head := upstream.commit(work, "second")
	runGit(t, work, "push", "--quiet", "origin", "main")
	rp := tc.repo("up/repo.git")
	used := rp.lastUsed.Load()

	if got := tc.warmPass(context.Background(), serves); got != 1 {
		t.Fatalf("warmPass refreshed %d repositories, want 1", got)
	}
	if refs := tc.cachedRefs(t); !strings.Contains(refs, head+" refs/heads/main") {
		t.Errorf("cached refs after the pass:\n%s\nwant main at %s", refs, head)
	}
	if checked := time.Unix(0, rp.lastChecked.Load()); time.Since(checked) > time.Minute {
		t.Errorf("lastChecked = %v, want just now", checked)
	}
	// A background refresh is no use by a client, so it neither renews the
	// repository's idle window nor changes the eviction order.
	if got := rp.lastUsed.Load(); got != used {
		t.Errorf("lastUsed moved from %v to %v", time.Unix(0, used), time.Unix(0, got))
	}
}

func TestWarmPassLeavesRepositoriesThatAreNotDue(t *testing.T) {
	for _, tt := range []struct {
		name      string
		configure func(*Options)
		prepare   func(*testCache, *repo)
		serves    func(string) bool
	}{
		{name: "checked recently", prepare: func(_ *testCache, rp *repo) {
			rp.lastChecked.Store(time.Now().Add(-10 * time.Minute).UnixNano())
		}},
		{name: "idle past warm_max_idle", prepare: func(_ *testCache, rp *repo) {
			rp.lastUsed.Store(time.Now().Add(-48 * time.Hour).UnixNano())
		}},
		{name: "never used", prepare: func(_ *testCache, rp *repo) {
			rp.lastUsed.Store(0)
		}},
		{name: "failed recently", prepare: func(_ *testCache, rp *repo) {
			rp.lastError.Store(&refreshError{kind: "fetch_failed", at: time.Now().Add(-5 * time.Minute)})
		}},
		// Cached before URLs were kept: the URL a client reached it at is
		// unknown, and the configured spelling may not be one the upstream
		// serves.
		{name: "no URL a client reached it at", prepare: func(_ *testCache, rp *repo) {
			rp.url.Store(nil)
		}},
		{name: "refused upstream", prepare: func(tc *testCache, rp *repo) {
			tc.mu.Lock()
			rp.refused = ErrRefused
			tc.mu.Unlock()
		}},
		{name: "too large", prepare: func(tc *testCache, rp *repo) {
			tc.markTooLarge(rp.key)
		}},
		{name: "no longer configured", serves: func(string) bool { return false }},
		{name: "warm_max_idle 0", configure: func(o *Options) { o.WarmMaxIdle = 0 }},
		{name: "stale_max_age 0", configure: func(o *Options) { o.StaleMaxAge = 0 }},
		// A check every stale_max_age/2 would come more often than every
		// warmMinInterval.
		{name: "stale_max_age under 20m", configure: func(o *Options) { o.StaleMaxAge = 15 * time.Minute }, prepare: func(_ *testCache, rp *repo) {
			rp.lastChecked.Store(time.Now().Add(-time.Hour).UnixNano())
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			upstream, _, tc, serves := warmTestCache(t, tt.configure)
			if tt.prepare != nil {
				tt.prepare(tc, tc.repo("up/repo.git"))
			}
			if tt.serves != nil {
				serves = tt.serves
			}
			rp := tc.repo("up/repo.git")
			requests, failed := upstream.requests.Load(), rp.lastError.Load()
			if got := tc.warmPass(context.Background(), serves); got != 0 {
				t.Errorf("warmPass refreshed %d repositories, want none", got)
			}
			if got := upstream.requests.Load(); got != requests {
				t.Errorf("warmPass made %d upstream requests, want none", got-requests)
			}
			if rp.lastError.Load() != failed {
				t.Errorf("warmPass tried a refresh: %+v", rp.lastError.Load())
			}
		})
	}
}

// A check that fails does not move lastChecked, so without a wait the next
// pass would try the upstream again straight away, all through an outage.
func TestWarmPassWaitsAfterAFailedCheck(t *testing.T) {
	upstream, _, tc, serves := warmTestCache(t, nil)
	upstream.status.Store(http.StatusServiceUnavailable)

	tc.warmPass(context.Background(), serves)
	requests := upstream.requests.Load()
	if requests == 0 {
		t.Fatal("the first pass made no upstream request")
	}
	tc.warmPass(context.Background(), serves)
	if got := upstream.requests.Load(); got != requests {
		t.Errorf("the second pass made %d upstream requests, want none", got-requests)
	}
}

func TestWarmRefreshesInTheBackgroundUntilClose(t *testing.T) {
	upstream, work, tc, serves := warmTestCache(t, func(o *Options) {
		o.warmTick = 10 * time.Millisecond
	})
	head := upstream.commit(work, "second")
	runGit(t, work, "push", "--quiet", "origin", "main")

	tc.Warm(serves)
	waitFor(t, func() bool {
		return strings.Contains(tc.cachedRefs(t), head+" refs/heads/main")
	})
	if err := tc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestWarmIntervals(t *testing.T) {
	for _, tt := range []struct {
		staleMaxAge, interval, retry time.Duration
	}{
		{24 * time.Hour, 12 * time.Hour, 3 * time.Hour},
		{time.Hour, 30 * time.Minute, warmMinInterval},
		{20 * time.Minute, warmMinInterval, warmMinInterval},
	} {
		c := &Cache{opts: Options{StaleMaxAge: tt.staleMaxAge}}
		if got := c.warmInterval(); got != tt.interval {
			t.Errorf("stale_max_age %v: interval = %v, want %v", tt.staleMaxAge, got, tt.interval)
		}
		if got := c.warmRetryInterval(); got != tt.retry {
			t.Errorf("stale_max_age %v: retry interval = %v, want %v", tt.staleMaxAge, got, tt.retry)
		}
	}
}

// A failed check is tried again well before the copy goes stale, so one
// failure does not leave the copy unservable through a later outage.
func TestWarmPassRetriesAFailedCheckBeforeTheCopyGoesStale(t *testing.T) {
	_, _, tc, serves := warmTestCache(t, func(o *Options) { o.StaleMaxAge = 24 * time.Hour })
	rp := tc.repo("up/repo.git")
	rp.lastChecked.Store(time.Now().Add(-15 * time.Hour).UnixNano())
	rp.lastError.Store(&refreshError{kind: "fetch_failed", at: time.Now().Add(-2 * time.Hour)})
	if got := tc.warmPass(context.Background(), serves); got != 0 {
		t.Fatalf("warmPass refreshed %d repositories two hours after a failure, want none", got)
	}
	rp.lastError.Store(&refreshError{kind: "fetch_failed", at: time.Now().Add(-4 * time.Hour)})
	if got := tc.warmPass(context.Background(), serves); got != 1 {
		t.Errorf("warmPass refreshed %d repositories four hours after a failure, want 1", got)
	}
}

// warmPass works from a list taken before its first refresh. A repository
// evicted meanwhile, or one a client checked meanwhile, is left alone.
func TestWarmRefreshRechecksEachRepositoryWhenItsTurnComes(t *testing.T) {
	for _, tt := range []struct {
		name   string
		change func(*testCache, *repo)
	}{
		{"evicted", func(tc *testCache, rp *repo) {
			tc.mu.Lock()
			delete(tc.repos, rp.key)
			tc.mu.Unlock()
		}},
		{"checked by a client", func(_ *testCache, rp *repo) {
			rp.lastChecked.Store(time.Now().UnixNano())
		}},
		{"failed for a client", func(_ *testCache, rp *repo) {
			rp.lastError.Store(&refreshError{kind: "fetch_failed", at: time.Now()})
		}},
		{"refused for a client", func(tc *testCache, rp *repo) {
			tc.mu.Lock()
			rp.refused = ErrRefused
			tc.mu.Unlock()
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			upstream, _, tc, _ := warmTestCache(t, nil)
			due := tc.warmDue(time.Now())
			if len(due) != 1 {
				t.Fatalf("due = %d repositories, want 1", len(due))
			}
			tt.change(tc, due[0])
			requests := upstream.requests.Load()
			if call := tc.startWarmRefresh(due[0]); call != nil {
				<-call.done
				t.Error("started a background refresh")
			}
			if got := upstream.requests.Load(); got != requests {
				t.Errorf("made %d upstream requests, want none", got-requests)
			}
		})
	}
}

// A background check fetches from the URL a client last reached the
// repository at, kept across restarts, since an upstream may serve a
// repository at only one spelling of its path.
func TestWarmPassUsesTheURLAClientUsed(t *testing.T) {
	upstream, work, tc, serves := warmTestCache(t, nil)
	head := upstream.commit(work, "second")
	runGit(t, work, "push", "--quiet", "origin", "main")

	if got := tc.warmPass(context.Background(), serves); got != 1 {
		t.Fatalf("warmPass refreshed %d repositories, want 1", got)
	}
	if refs := tc.cachedRefs(t); !strings.Contains(refs, head+" refs/heads/main") {
		t.Errorf("cached refs:\n%s\nwant main at %s", refs, head)
	}

	if err := tc.Close(); err != nil {
		t.Fatal(err)
	}
	reloaded, err := New(Options{
		Upstreams:   map[string]string{"up": "up"},
		CacheDir:    tc.dir,
		StaleMaxAge: time.Hour,
		Client:      &http.Client{},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reloaded.Close() }()
	if got := reloaded.repos["up/repo.git"].repoURL(); got != upstream.server.URL+"/repo" {
		t.Errorf("reloaded URL = %q, want %q", got, upstream.server.URL+"/repo")
	}
}

// Only a URL a refresh succeeded with is kept: a request served from the
// cache without a check proves nothing about the URL it named.
func TestRememberedURLIsOneARefreshSucceededWith(t *testing.T) {
	upstream, _, tc, _ := warmTestCache(t, nil)
	rp := tc.repo("up/repo.git")
	rp.lastChecked.Store(time.Now().UnixNano())
	want := upstream.server.URL + "/repo"
	if got := rp.repoURL(); got != want {
		t.Fatalf("remembered URL = %q, want %q", got, want)
	}

	lease, err := tc.Open(context.Background(), "up/repo.git", upstream.server.URL+"/missing", OpenOptions{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	lease.Release()
	if got := rp.repoURL(); got != want {
		t.Errorf("a cache-only open changed the remembered URL to %q", got)
	}
}
