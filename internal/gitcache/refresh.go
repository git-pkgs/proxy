package gitcache

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/git-pkgs/proxy/internal/metrics"
)

const (
	// fetchTimeout bounds one refresh, including a cold clone of a large
	// repository.
	fetchTimeout = time.Hour
	// defaultRefCheckTimeout bounds the ref check, so an upstream that
	// stalls mid-response does not hold a repository's refresh for
	// fetchTimeout.
	defaultRefCheckTimeout = 2 * time.Minute
	// tooLargeRetryAfter is how long a repository that passed
	// git.max_repo_size is left alone before it is tried again.
	tooLargeRetryAfter = time.Hour
)

// errUpstreamUnanswered marks a fetch the upstream never answered, such as
// one during an outage.
var errUpstreamUnanswered = errors.New("upstream did not answer the fetch")

// errClosed is returned for a refresh asked for once the cache is closing.
var errClosed = errors.New("git cache is closed")

// refreshCall is a refresh in progress, shared by every request for the
// repository that arrives while it runs.
type refreshCall struct {
	done chan struct{}
	err  error
	// checked is closed once the upstream's branches and tags have been
	// compared with the cached ones, and fetching then says whether they
	// differ, so a fetch follows.
	checked     chan struct{}
	fetching    bool
	checkedOnce sync.Once
}

func newRefreshCall() *refreshCall {
	return &refreshCall{done: make(chan struct{}), checked: make(chan struct{})}
}

func (call *refreshCall) markChecked(fetching bool) {
	call.checkedOnce.Do(func() {
		call.fetching = fetching
		close(call.checked)
	})
}

// ensureFresh brings the repository up to date with its upstream before it is
// served. A repository checked successfully within StaleMaxAge is served
// from the cache if the upstream fails or its check takes longer than
// RefreshWait; the refresh carries on in the background either way. Once the
// check finds new commits, their fetch is waited for, so a clone right after
// a push gets them. One never cloned, or not checked within StaleMaxAge, is
// served only once the refresh succeeds.
func (c *Cache) ensureFresh(ctx context.Context, rp *repo, repoURL string) error {
	call := c.startRefresh(rp, repoURL)
	checked := time.Unix(0, rp.lastChecked.Load())
	if !rp.exists() || time.Since(checked) > c.opts.StaleMaxAge {
		select {
		case <-call.done:
			return call.err
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	timer := time.NewTimer(c.opts.RefreshWait)
	defer timer.Stop()
	refChecked, timeout := call.checked, timer.C
	for {
		select {
		case <-refChecked:
			refChecked = nil
			if call.fetching {
				timeout = nil
			}
		case <-call.done:
			if call.err != nil && servesStale(call.err) {
				c.logger.Warn("git cache: refresh failed, serving cached copy",
					"repository", rp.key, "checked", checked, "error", call.err)
				return nil
			}
			return call.err
		case <-timeout:
			c.logger.Info("git cache: upstream check still running, serving cached copy",
				"repository", rp.key, "checked", checked)
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// servesStale reports whether the cached copy may be served after err: not
// when the upstream says the repository is gone or refuses access to it.
func servesStale(err error) bool {
	return !errors.Is(err, ErrNotFound) && !errors.Is(err, ErrRefused)
}

// startRefresh starts a refresh of the repository, or joins the one running.
// The refresh holds a use of the repository until it ends.
func (c *Cache) startRefresh(rp *repo, repoURL string) *refreshCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	if rp.refreshing != nil {
		return rp.refreshing
	}
	call := newRefreshCall()
	if c.closed {
		call.err = errClosed
		close(call.done)
		return call
	}
	rp.refreshing = call
	rp.users++
	c.refreshes.Add(1)
	go func() {
		defer c.refreshes.Done()
		ctx, cancel := context.WithTimeout(c.ctx, fetchTimeout)
		start := time.Now()
		call.err = c.refresh(ctx, rp, repoURL, call.markChecked)
		call.markChecked(false)
		cancel()
		metrics.RecordUpstreamFetch(ecosystem, time.Since(start))
		// A refresh cut short by Close is not an upstream failure.
		if call.err != nil && c.ctx.Err() == nil {
			metrics.RecordUpstreamError(ecosystem, errorType(call.err), metrics.FailureDetail{URL: repoURL, Err: call.err})
			c.logger.Warn("git cache: refresh failed", "repository", rp.key, "error", call.err)
		}
		c.mu.Lock()
		rp.refreshing = nil
		c.mu.Unlock()
		c.release(rp)
		close(call.done)
		if call.err == nil {
			c.evict()
		}
	}()
	return call
}

// refresh checks the upstream's branches and tags and fetches if they differ
// from the cached ones, cloning the repository if it is not cached yet.
// checked is called once a cached repository's refs have been compared.
func (c *Cache) refresh(ctx context.Context, rp *repo, repoURL string, checked func(fetching bool)) error {
	if c.isTooLarge(rp.key) {
		return ErrTooLarge
	}
	checkCtx, cancel := context.WithTimeout(ctx, c.refCheckTimeout)
	remote, err := c.listUpstreamRefs(checkCtx, repoURL)
	cancel()
	if err != nil {
		return err
	}
	if rp.exists() {
		err = c.update(ctx, rp, repoURL, remote, checked)
	} else {
		err = c.clone(ctx, rp, repoURL, remote)
	}
	if errors.Is(err, ErrTooLarge) {
		c.markTooLarge(rp.key)
	}
	if err != nil {
		return err
	}
	now := time.Now()
	rp.lastChecked.Store(now.UnixNano())
	touch(rp.dir, checkedStamp, now)
	return nil
}

// clone builds the repository in a temporary directory and moves it into
// place, so no request ever reads a partial clone.
func (c *Cache) clone(ctx context.Context, rp *repo, repoURL string, remote remoteRefs) error {
	// Released after the temporary clone is removed.
	res := c.newReservation()
	defer res.release()
	tmp, err := os.MkdirTemp(c.tmpDir(), "clone-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tmp) }()

	if _, err := c.run(ctx, tmp, "init", "--quiet", "--bare"); err != nil {
		return err
	}
	if err := c.fetch(ctx, tmp, repoURL, c.opts.MaxRepoSize, res, refreshFlags, branchesAndTags...); err != nil {
		return err
	}
	if err := c.setHead(ctx, tmp, remote, ""); err != nil {
		return err
	}
	size := dirSize(tmp)
	if c.opts.MaxRepoSize > 0 && size > c.opts.MaxRepoSize {
		return ErrTooLarge
	}
	if err := os.MkdirAll(filepath.Dir(rp.dir), dirPermissions); err != nil {
		return err
	}
	if err := os.Rename(tmp, rp.dir); err != nil {
		return fmt.Errorf("moving clone into place: %w", err)
	}
	res.settle(rp, size)
	c.logger.Info("git cache: cloned repository", "repository", rp.key, "size", size)
	return nil
}

// update fetches when the upstream's branches or tags differ from the cached
// ones, and follows a change of the upstream's default branch.
func (c *Cache) update(ctx context.Context, rp *repo, repoURL string, remote remoteRefs, checked func(fetching bool)) error {
	unlock, err := rp.lockWrites(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	local, wanted, err := c.localRefs(ctx, rp.dir)
	if err != nil {
		return err
	}
	fetched := !maps.Equal(local, remote.refs)
	checked(fetched)
	if fetched {
		limit, err := c.fetchLimit(rp)
		if err != nil {
			return err
		}
		res := c.newReservation()
		err = c.fetch(ctx, rp.dir, repoURL, limit, res, refreshFlags, branchesAndTags...)
		res.settle(rp, dirSize(rp.dir))
		if err != nil {
			return err
		}
	}
	if err := c.pruneWanted(ctx, rp.dir, wanted, fetched); err != nil {
		c.logger.Warn("git cache: pruning wanted refs failed", "repository", rp.key, "error", err)
	}
	head, _ := c.run(ctx, rp.dir, "symbolic-ref", "--quiet", "HEAD")
	return c.setHead(ctx, rp.dir, remote, strings.TrimSpace(head))
}

// branchesAndTags are the refspecs of a refresh. A bare clone has none of its
// own, and other refs, such as Gerrit's refs/changes, are left out.
var branchesAndTags = []string{"+refs/heads/*:refs/heads/*", "+refs/tags/*:refs/tags/*"}

// refreshFlags delete the branches and tags the upstream no longer has, and
// update all refs together, so a client served while a refresh runs never
// sees only some of them moved.
var refreshFlags = []string{"--prune", "--atomic"}

// fetchLimit returns how many more bytes the repository may take, 0 for no
// limit.
func (c *Cache) fetchLimit(rp *repo) (int64, error) {
	if c.opts.MaxRepoSize <= 0 {
		return 0, nil
	}
	limit := c.opts.MaxRepoSize - rp.size.Load()
	if limit <= 0 {
		return 0, ErrTooLarge
	}
	return limit, nil
}

// fetch fetches refspecs from the upstream into the repository at dir,
// through the relay, adding flags to git fetch. limit caps the bytes
// fetched, 0 for no cap, and res holds space within MaxSize for them.
func (c *Cache) fetch(ctx context.Context, dir, repoURL string, limit int64, res *reservation, flags []string, refspecs ...string) error {
	select {
	case c.fetches <- struct{}{}:
		defer func() { <-c.fetches }()
	case <-ctx.Done():
		return ctx.Err()
	}

	relayURL, route, unregister := c.relay.register(repoURL, limit, res)
	defer unregister()
	args := append([]string{"fetch", "--quiet", "--no-write-fetch-head"}, flags...)
	_, err := c.run(ctx, dir, append(append(args, relayURL), refspecs...)...)
	if err != nil {
		// index-pack leaves its temporary pack behind when its input ends
		// early. Fetches into a repository run one at a time, so any left
		// now are this fetch's.
		leftovers, _ := filepath.Glob(filepath.Join(dir, "objects", "pack", "tmp_*"))
		for _, path := range leftovers {
			_ = os.Remove(path)
		}
	}
	if route.exceeded.Load() {
		return ErrTooLarge
	}
	if route.full.Load() {
		return ErrCacheFull
	}
	if err != nil && !route.answered.Load() {
		return fmt.Errorf("%w: %w", errUpstreamUnanswered, err)
	}
	return err
}

// setHead points HEAD at the upstream's default branch, when the upstream
// names one and it is among the cached branches.
func (c *Cache) setHead(ctx context.Context, dir string, remote remoteRefs, current string) error {
	if remote.head == "" || remote.head == current {
		return nil
	}
	if _, ok := remote.refs[remote.head]; !ok {
		return nil
	}
	_, err := c.run(ctx, dir, "symbolic-ref", "HEAD", remote.head)
	return err
}

// localRefs lists the cached branches and tags, and the names of the wanted
// refs.
func (c *Cache) localRefs(ctx context.Context, dir string) (map[string]string, []string, error) {
	out, err := c.run(ctx, dir, "for-each-ref", "--format=%(objectname) %(refname)", headsPrefix, tagsPrefix, wantedPrefix)
	if err != nil {
		return nil, nil, err
	}
	refs := make(map[string]string)
	var wanted []string
	for line := range strings.Lines(out) {
		oid, name, ok := strings.Cut(strings.TrimSuffix(line, "\n"), " ")
		switch {
		case !ok:
		case strings.HasPrefix(name, wantedPrefix):
			wanted = append(wanted, name)
		default:
			refs[name] = oid
		}
	}
	return refs, wanted, nil
}

func errorType(err error) string {
	switch {
	case errors.Is(err, ErrNotFound):
		return "not_found"
	case errors.Is(err, ErrRefused):
		return "refused"
	case errors.Is(err, ErrTooLarge):
		return "too_large"
	case errors.Is(err, ErrCacheFull):
		return "cache_full"
	}
	return "fetch_failed"
}

func (c *Cache) isTooLarge(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	since, ok := c.tooLarge[key]
	if ok && time.Since(since) >= tooLargeRetryAfter {
		delete(c.tooLarge, key)
		return false
	}
	return ok
}

func (c *Cache) markTooLarge(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.tooLarge[key] = time.Now()
}
