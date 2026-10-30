package handler

import (
	"sync"
	"time"

	"github.com/git-pkgs/cooldown"
)

// A rewrite made with cooldown on stays right until a version it held back
// leaves cooldown: versions it let through stay allowed as time passes, and
// new versions upstream change the document and so the cache key. So the
// cached rewrite expires when the earliest version it withheld becomes
// available, instead of after a fixed time.
//
// Handlers ask cooldown through Proxy.cooldown, which tells every rewrite
// running at that moment when a withheld version becomes available. A
// rewrite may hear of versions another rewrite withheld at the same time;
// that only makes it expire earlier than it has to, never later.

// maxCooldownRewriteTTL is the longest a rewrite made with cooldown on is
// served, even when it held nothing back.
const maxCooldownRewriteTTL = 24 * time.Hour

// minCooldownRewriteTTL is the shortest cap on a rewrite made with cooldown
// on, however short the cooldown.
const minCooldownRewriteTTL = time.Minute

// cooldownRewriteTTL returns the longest a rewrite made with cooldown on is
// served: the shortest cooldown configured, between a minute and a day. It
// only matters when a rewrite held nothing back, or held back a version
// whose availability is unknown.
func (p *Proxy) cooldownRewriteTTL() time.Duration {
	ttl := p.ShortestCooldown
	switch {
	case ttl <= 0 || ttl > maxCooldownRewriteTTL:
		return maxCooldownRewriteTTL
	case ttl < minCooldownRewriteTTL:
		return minCooldownRewriteTTL
	}
	return ttl
}

// cooldownWatch holds the rewrites running with cooldown on.
type cooldownWatch struct {
	mu     sync.Mutex
	active map[*cooldownRecorder]struct{}
}

// cooldownRecorder collects when the versions withheld during one rewrite
// become available.
type cooldownRecorder struct {
	// earliest is when the first withheld version becomes available, or
	// zero if none was withheld.
	earliest time.Time
	// unknown is set when a version was withheld without a time it becomes
	// available.
	unknown bool
}

// watchCooldown starts recording the versions cooldown holds back for a
// rewrite about to run. unwatchCooldown ends it.
func (p *Proxy) watchCooldown() *cooldownRecorder {
	rec := &cooldownRecorder{}
	w := &p.cooldownWatch
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.active == nil {
		w.active = make(map[*cooldownRecorder]struct{})
	}
	w.active[rec] = struct{}{}
	return rec
}

func (p *Proxy) unwatchCooldown(rec *cooldownRecorder) {
	w := &p.cooldownWatch
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.active, rec)
}

// cooldownExpiry returns when a rewrite that started at start and recorded
// into rec should expire: when the first version it held back leaves
// cooldown, and no later than cooldownRewriteTTL after start.
func (p *Proxy) cooldownExpiry(rec *cooldownRecorder, start time.Time) time.Time {
	w := &p.cooldownWatch
	w.mu.Lock()
	earliest, unknown := rec.earliest, rec.unknown
	w.mu.Unlock()

	limit := start.Add(p.cooldownRewriteTTL())
	if unknown || earliest.IsZero() || earliest.After(limit) {
		return limit
	}
	return earliest
}

// withheld tells the running rewrites that a version is held back until
// availableAt, zero when that is unknown.
func (p *Proxy) withheld(availableAt time.Time) {
	w := &p.cooldownWatch
	w.mu.Lock()
	defer w.mu.Unlock()
	for rec := range w.active {
		switch {
		case availableAt.IsZero():
			rec.unknown = true
		case rec.earliest.IsZero() || availableAt.Before(rec.earliest):
			rec.earliest = availableAt
		}
	}
}

// cooldown returns the proxy's cooldown policy, telling the running rewrites
// about each version it holds back. Handlers use it rather than p.Cooldown.
func (p *Proxy) cooldown() CooldownPolicy { //nolint:ireturn // nil when cooldown is not configured, as p.Cooldown is
	if p.Cooldown == nil {
		return nil
	}
	return recordingCooldown{p: p}
}

// recordingCooldown is the proxy's cooldown policy, reporting the versions it
// holds back to the running rewrites.
type recordingCooldown struct {
	p *Proxy
}

func (r recordingCooldown) IsAllowed(ecosystem, packagePURL string, publishedAt time.Time) bool {
	if r.p.Cooldown.IsAllowed(ecosystem, packagePURL, publishedAt) {
		return true
	}
	r.p.withheld(r.availableAt(ecosystem, packagePURL, publishedAt))
	return false
}

func (r recordingCooldown) Evaluate(ecosystem, packagePURL string, publishedAt, evaluatedAt time.Time) cooldown.Decision {
	d := r.p.Cooldown.Evaluate(ecosystem, packagePURL, publishedAt, evaluatedAt)
	if !d.Allowed {
		r.p.withheld(d.AvailableAt)
	}
	return d
}

func (r recordingCooldown) For(ecosystem, packagePURL string) time.Duration {
	return r.p.Cooldown.For(ecosystem, packagePURL)
}

func (r recordingCooldown) Enabled() bool {
	return r.p.Cooldown.Enabled()
}

// availableAt is when a version IsAllowed held back leaves cooldown, or zero
// when that is unknown.
func (r recordingCooldown) availableAt(ecosystem, packagePURL string, publishedAt time.Time) time.Time {
	if publishedAt.IsZero() {
		return time.Time{}
	}
	d := r.p.Cooldown.For(ecosystem, packagePURL)
	if d <= 0 {
		return time.Time{}
	}
	return publishedAt.Add(d)
}
