package handler

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestNPMRefreshDoesNotRetainCancellation(t *testing.T) {
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
		t.Run(cause.Error(), func(t *testing.T) {
			p, _, _, _ := setupTestProxy(t)
			h := NewNPMHandler(p, "http://proxy.test", "https://registry.example")
			h.retainPackumentRefresh("leftpad", fmt.Errorf("fetch failed: %w", cause))
			if _, limited := h.limitedPackumentRefresh("leftpad"); limited {
				t.Fatalf("retained caller cancellation: %v", cause)
			}
		})
	}
}

// Under cooldown, a refreshed row replaced by an OLDER document (one that
// lacks the version) must not yield "no entry, no error".
func TestNPMCooldownRefreshReplacedByOlderDocument(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		missing, upstreamFails bool
	}{
		{name: "changed"},
		{name: "missing", missing: true},
		{name: "changed-upstream-fails", upstreamFails: true},
		{name: "missing-upstream-fails", missing: true, upstreamFails: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testNPMCooldownOlderDocument(t, tc.missing, tc.upstreamFails)
		})
	}
}

func testNPMCooldownOlderDocument(t *testing.T, missing, upstreamFails bool) {
	// Make the requested version newly published, so recovery must withhold
	// it rather than return an absent entry with an empty publish time.
	body := strings.Replace(bigPackument(100), "2016-01-01T00:00:50Z", time.Now().UTC().Format(time.RFC3339), 1)
	u := newStreamUpstream(t, body)
	p, _, _ := streamProxy(t, u) // cooldown on
	h := NewNPMHandler(p, "http://proxy.test", u.URL)
	res := h.refreshMissingPackument(t.Context(), u.URL+"/big", "big", false)
	if res.err != nil || res.entry == nil {
		t.Fatalf("refresh: %v", res.err)
	}
	older := bigPackument(5)
	current := *res.entry
	if missing {
		current.StoragePath += ".older"
	}
	if _, _, err := p.Storage.Store(t.Context(), current.StoragePath, strings.NewReader(older)); err != nil {
		t.Fatal(err)
	}
	if missing {
		if err := p.Storage.Delete(t.Context(), res.entry.StoragePath); err != nil {
			t.Fatal(err)
		}
	}
	current.ContentDigest.String = sha256Digest([]byte(older))
	current.Size.Int64 = int64(len(older))
	if err := p.DB.UpsertMetadataCache(&current); err != nil {
		t.Fatal(err)
	}
	before := u.requests.Load()
	u.fail.Store(upstreamFails)
	data, err := h.readRefreshedNPMVersion(t.Context(), res, u.URL+"/big", "big", "1.0.50", false)
	if got := u.requests.Load() - before; got != 1 {
		t.Fatalf("recovery upstream requests=%d, want one forced refresh; entry=%s published=%q err=%v", got, data.entry, data.published, err)
	}
	if upstreamFails {
		if err == nil {
			t.Fatal("failed recovery did not fail closed")
		}
		return
	}
	if err != nil || data.entry == nil {
		t.Fatalf("cooldown recovery: entry=%s published=%q err=%v", data.entry, data.published, err)
	}
	withheld, err := h.versionInCooldown("big", "1.0.50", func() (npmVersionData, error) { return data, nil })
	if err != nil || !withheld {
		t.Fatalf("new version was not withheld: withheld=%t err=%v published=%q", withheld, err, data.published)
	}
}
