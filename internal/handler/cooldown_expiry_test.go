package handler

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/git-pkgs/cooldown"
)

// TestCachedRewrite_ExpiresWhenWithheldVersionIsAvailable checks that a
// rewrite which held a version back is cached until that version leaves
// cooldown, and no longer.
func TestCachedRewrite_ExpiresWhenWithheldVersionIsAvailable(t *testing.T) {
	proxy, _, _, _ := setupTestProxy(t)
	clock := &testClock{t: time.Now()}
	proxy.now = clock.now
	proxy.SetMetadataRewriteCacheSize(1 << 20)
	proxy.Cooldown = &cooldown.Config{Default: "3d"}
	purl := canonicalPackagePURL("npm", "left-pad")
	// One version is 3d-5h old: it leaves cooldown in five hours.
	published := clock.now().Add(-72*time.Hour + 5*time.Hour)

	var calls int
	var rewrite keepingRewrite = func(b []byte) ([]byte, []string, error) {
		calls++
		if proxy.cooldown().IsAllowed("npm", purl, published) {
			return b, []string{"1.0.0", "1.1.0"}, nil
		}
		return b, []string{"1.0.0"}, nil
	}
	get := func() {
		if _, err := proxy.cachedRewrite(context.Background(), "npm", "http://proxy", "left-pad", []byte("doc"), "", rewrite); err != nil {
			t.Fatal(err)
		}
	}

	get()
	clock.advance(5*time.Hour - time.Minute)
	get()
	if calls != 1 {
		t.Errorf("rewrites before the withheld version is available = %d, want 1", calls)
	}
	clock.advance(time.Minute)
	get()
	if calls != 2 {
		t.Errorf("rewrites once the withheld version is available = %d, want 2", calls)
	}
}

// TestCachedRewrite_NothingWithheldCachedForCap checks that a rewrite which
// held nothing back is cached for cooldownRewriteTTL.
func TestCachedRewrite_NothingWithheldCachedForCap(t *testing.T) {
	proxy, _, _, _ := setupTestProxy(t)
	clock := &testClock{t: time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)}
	proxy.now = clock.now
	proxy.SetMetadataRewriteCacheSize(1 << 20)
	proxy.Cooldown = &cooldown.Config{Default: "2h"}
	proxy.ShortestCooldown = 2 * time.Hour
	var r countingRewrite

	get := func() {
		_, _ = proxy.cachedRewrite(context.Background(), "npm", "http://proxy", "left-pad", []byte("doc"), "", r.keeping)
	}
	get()
	clock.advance(2*time.Hour - time.Second)
	get()
	if got := r.calls.Load(); got != 1 {
		t.Errorf("rewrites within the cap = %d, want 1", got)
	}
	clock.advance(time.Second)
	get()
	if got := r.calls.Load(); got != 2 {
		t.Errorf("rewrites after the cap = %d, want 2", got)
	}
}

// TestCooldownExpiry checks how one rewrite's recorded decisions set its
// expiry, including decisions made for other rewrites running at the same
// time, which may only make it expire earlier.
func TestCooldownExpiry(t *testing.T) {
	p := &Proxy{Cooldown: &cooldown.Config{Default: "3d"}, ShortestCooldown: 72 * time.Hour}
	// IsAllowed compares with the wall clock, so the times are relative to it.
	start := time.Now()
	limit := start.Add(24 * time.Hour)
	purl := canonicalPackagePURL("npm", "left-pad")

	rec := p.watchCooldown()
	if got := p.cooldownExpiry(rec, start); !got.Equal(limit) {
		t.Errorf("nothing withheld: expiry = %v, want the cap %v", got, limit)
	}

	other := p.watchCooldown()
	p.cooldown().IsAllowed("npm", purl, start.Add(-60*time.Hour)) // available in 12h
	p.unwatchCooldown(other)
	p.cooldown().IsAllowed("npm", purl, start.Add(-66*time.Hour)) // available in 6h, after other ended
	if got, want := p.cooldownExpiry(rec, start), start.Add(6*time.Hour); !got.Equal(want) {
		t.Errorf("rec expiry = %v, want the earliest availability %v", got, want)
	}
	if got, want := p.cooldownExpiry(other, start), start.Add(12*time.Hour); !got.Equal(want) {
		t.Errorf("other expiry = %v, want %v: decisions after it ended don't count", got, want)
	}

	p.cooldown().Evaluate("npm", purl, start.Add(-71*time.Hour), start) // available in 1h
	if got, want := p.cooldownExpiry(rec, start), start.Add(time.Hour); !got.Equal(want) {
		t.Errorf("after Evaluate: expiry = %v, want %v", got, want)
	}
	p.unwatchCooldown(rec)
	if len(p.cooldownWatch.active) != 0 {
		t.Errorf("%d recorders left after unwatch", len(p.cooldownWatch.active))
	}
}

// TestHandlersAskCooldownThroughRecorder checks that handlers make cooldown
// decisions through Proxy.cooldown. A decision made on Proxy.Cooldown
// directly would not reach the rewrites running, so a cached rewrite could
// keep hiding a version past its cooldown.
func TestHandlersAskCooldownThroughRecorder(t *testing.T) {
	direct := regexp.MustCompile(`\.Cooldown\.(IsAllowed|Evaluate)\(|Cooldown:\s+\w+(\.\w+)*\.Cooldown\b`)
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || f == "cooldown_expiry.go" {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(src), "\n") {
			if direct.MatchString(line) {
				t.Errorf("%s:%d asks Proxy.Cooldown directly; use Proxy.cooldown(): %s", f, i+1, strings.TrimSpace(line))
			}
		}
	}
}

// TestHandlerRewriteExpiresWithWithheldVersion checks, through the handlers,
// that a cached rewrite expires when the version cooldown held back from it
// becomes available.
func TestHandlerRewriteExpiresWithWithheldVersion(t *testing.T) {
	for _, c := range validatorCases {
		t.Run(c.ecosystem, func(t *testing.T) {
			u := cooldownUpstream(t) // the newest version was published 2h ago
			proxy, _, clock := validatorProxyWithDB(t, u, time.Hour)
			// Cooldown compares with the wall clock, so the proxy's clock
			// follows it here.
			clock.t = time.Now()
			proxy.Cooldown = &cooldown.Config{Default: "3h"}
			proxy.ShortestCooldown = 3 * time.Hour
			handler := c.handler(proxy, u)

			if rec := serve(handler, c.path, nil); rec.Code != http.StatusOK {
				t.Fatalf("status %d", rec.Code)
			}
			proxy.rewrites.mu.Lock()
			defer proxy.rewrites.mu.Unlock()
			if len(proxy.rewrites.entries) != 1 {
				t.Fatalf("%d cached rewrites, want 1", len(proxy.rewrites.entries))
			}
			for _, el := range proxy.rewrites.entries {
				expires := el.Value.(*rewriteEntry).expires
				// Published 2h ago with a 3h cooldown: available in an hour,
				// to the second the publish time was given in.
				if want := time.Now().Add(time.Hour); expires.Before(want.Add(-2*time.Second)) || expires.After(want.Add(time.Second)) {
					t.Errorf("rewrite expires %v, want about %v when the withheld version is available", expires, want)
				}
			}
		})
	}
}
