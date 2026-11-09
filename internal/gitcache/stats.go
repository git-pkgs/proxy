package gitcache

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"
)

// RepositoryStats describes a cached repository.
type RepositoryStats struct {
	// Key is the repository's path in the cache, such as
	// github-1a2b3c4d/acme/widgets.git.
	Key  string
	Size int64
	// LastChecked is when the upstream's refs were last compared, and
	// LastUsed when a client last read the repository.
	LastChecked, LastUsed time.Time
	// Requests counts clones and fetches since the cache was opened.
	Requests int64
	// LastError names the kind of error the last refresh failed with, such
	// as fetch_failed or too_large, and LastErrorAt when. Both are empty
	// once a refresh succeeds.
	LastError   string
	LastErrorAt time.Time
	// Refused is set while the upstream's last answer refused the
	// repository or reported it missing, so it isn't served.
	Refused bool
}

// refreshError is the last failed refresh of a repository.
type refreshError struct {
	kind string
	at   time.Time
}

// Repositories describes the repositories in the cache, sorted by key.
func (c *Cache) Repositories() []RepositoryStats {
	c.mu.Lock()
	repos := make([]*repo, 0, len(c.repos))
	refused := make(map[*repo]bool)
	for _, rp := range c.repos {
		repos = append(repos, rp)
		refused[rp] = rp.refused != nil
	}
	c.mu.Unlock()

	var out []RepositoryStats
	for _, rp := range repos {
		if !rp.exists() {
			continue
		}
		s := RepositoryStats{
			Key:         rp.key,
			Size:        rp.size.Load(),
			LastChecked: unixNanoTime(rp.lastChecked.Load()),
			LastUsed:    unixNanoTime(rp.lastUsed.Load()),
			Requests:    rp.requests.Load(),
			Refused:     refused[rp],
		}
		if e := rp.lastError.Load(); e != nil {
			s.LastError, s.LastErrorAt = e.kind, e.at
		}
		out = append(out, s)
	}
	slices.SortFunc(out, func(a, b RepositoryStats) int { return strings.Compare(a.Key, b.Key) })
	return out
}

func unixNanoTime(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n)
}

// RepositoryState is what a cached repository holds: the branch HEAD names
// with its newest commit, and how many branches and tags there are.
type RepositoryState struct {
	Branch     string
	Commit     string
	CommitTime time.Time
	// Subject is the first line of the commit message, at most
	// maxSubjectLength bytes.
	Subject        string
	Branches, Tags int
}

// maxSubjectLength bounds RepositoryState.Subject.
const maxSubjectLength = 200

// headFields are the commit ID, time and subject readState asks git log for.
const headFields = 3

// stateRetryDelay is how long a failed state read is kept.
const stateRetryDelay = time.Minute

// cachedState is a repository's state, or the error reading it failed
// with, as read after the check at checked. A failed read is kept until
// retryAt.
type cachedState struct {
	checked int64
	state   RepositoryState
	err     error
	retryAt time.Time
}

// RepositoryState reads the state of the repository cached under key. It is
// read again only after the repository's next check of the upstream, so a
// page listing many repositories runs git for those that changed. A read
// that failed is kept for stateRetryDelay.
func (c *Cache) RepositoryState(ctx context.Context, key string) (RepositoryState, error) {
	c.mu.Lock()
	rp := c.repos[c.repoKey(key)]
	c.mu.Unlock()
	if rp == nil || !rp.exists() {
		return RepositoryState{}, ErrNotFound
	}
	checked := rp.lastChecked.Load()
	if cached := rp.state.Load(); cached != nil && cached.checked == checked &&
		(cached.err == nil || time.Now().Before(cached.retryAt)) {
		return cached.state, cached.err
	}
	state, err := c.readState(ctx, rp.dir)
	// A read the request gave up on says nothing about the repository.
	if ctx.Err() == nil {
		rp.state.Store(&cachedState{checked: checked, state: state, err: err, retryAt: time.Now().Add(stateRetryDelay)})
	}
	return state, err
}

func (c *Cache) readState(ctx context.Context, dir string) (RepositoryState, error) {
	var state RepositoryState
	refs, err := c.run(ctx, dir, "for-each-ref", "--format=%(refname)", "refs/heads", "refs/tags")
	if err != nil {
		return state, err
	}
	for ref := range strings.Lines(refs) {
		if strings.HasPrefix(ref, "refs/heads/") {
			state.Branches++
		} else {
			state.Tags++
		}
	}
	// Both exit 1, and print nothing, for a detached HEAD and for one with
	// no commit yet; any other failure is an error.
	branch, err := c.run(ctx, dir, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil && !exitedWith(err, 1) {
		return state, err
	}
	state.Branch = strings.TrimSpace(branch)
	if _, err := c.run(ctx, dir, "rev-parse", "--verify", "--quiet", "HEAD"); exitedWith(err, 1) {
		return state, nil
	} else if err != nil {
		return state, err
	}
	head, err := c.run(ctx, dir, "log", "-1", "--format=%H%x00%ct%x00%s", "HEAD", "--")
	if err != nil {
		return state, err
	}
	fields := strings.SplitN(strings.TrimSpace(head), "\x00", headFields)
	if len(fields) != headFields {
		return state, fmt.Errorf("git log: unexpected output %q", head)
	}
	state.Commit, state.Subject = fields[0], fields[2]
	if unix, err := strconv.ParseInt(fields[1], 10, 64); err == nil {
		state.CommitTime = time.Unix(unix, 0)
	}
	if len(state.Subject) > maxSubjectLength {
		state.Subject = strings.ToValidUTF8(state.Subject[:maxSubjectLength], "") + "…"
	}
	return state, nil
}

// exitedWith reports whether err is from a git process that exited with code.
func exitedWith(err error, code int) bool {
	var exit *exec.ExitError
	return errors.As(err, &exit) && exit.ExitCode() == code
}
