// Package gitcache serves Git repositories read-only over smart HTTP from a
// local cache of bare repositories, refreshed from configured upstreams.
package gitcache

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/git-pkgs/proxy/internal/metrics"
)

const (
	// tmpDirName holds clones in progress and evicted repositories being
	// deleted. Upstream names cannot start with a dot, so it cannot clash
	// with an upstream's directory, and the name is specific in case
	// cache_dir is shared with other data.
	tmpDirName = ".gitcache-tmp"
	// lockFileName is locked while the cache is open, so a second process
	// given the same cache_dir fails to start instead of deleting the first
	// one's clones in progress.
	lockFileName = ".gitcache-lock"
	// usedStampInterval limits how often a repository's last use is written
	// to disk.
	usedStampInterval = time.Minute
	// defaultLowSpeedTime is how many seconds a git transfer may receive
	// nothing before it is abandoned. Fetches run with --quiet, so the
	// upstream sends nothing while it counts objects; this is far longer.
	defaultLowSpeedTime = 600
	ecosystem           = "git"
	upstreamHashLen     = 8
	checkedStamp        = "proxy-checked"
	usedStamp           = "proxy-used"
	dirPermissions      = 0o750
	lockFilePermissions = 0o600
)

// Options configures a Cache.
type Options struct {
	CacheDir    string
	MaxSize     int64
	MaxRepoSize int64
	RefreshWait time.Duration
	StaleMaxAge time.Duration
	// Client makes every upstream request, including those of the git
	// processes the cache runs. Redirects are never followed.
	Client *http.Client
	Logger *slog.Logger
	// Upstreams maps each configured upstream's name to its directory in
	// CacheDir, as UpstreamDir names it. Only those directories are loaded
	// at startup. A directory UpstreamDir would name for one of these names
	// at another URL, left by repointing the upstream, is removed. Nothing
	// else in CacheDir is touched, so it may hold other data.
	Upstreams map[string]string

	// refCheckTimeout, lowSpeedTime, maxRequestBody and maxUploadPacks
	// override their defaults in tests.
	refCheckTimeout time.Duration
	lowSpeedTime    int
	maxRequestBody  int64
	maxUploadPacks  int
}

// Cache is the Git repository cache. internal/handler serves it at /git.
type Cache struct {
	opts            Options
	client          *http.Client
	logger          *slog.Logger
	gitPath         string
	dir             string
	lock            *os.File
	relay           *relay
	fetches         chan struct{}
	uploadPacks     chan struct{}
	refCheckTimeout time.Duration
	lowSpeedTime    int
	maxRequestBody  int64

	ctx    context.Context
	cancel context.CancelFunc
	// refreshes counts the refreshes running, which Close waits for.
	refreshes sync.WaitGroup

	mu sync.Mutex
	// closed stops refreshes from starting once Close has begun.
	closed   bool
	repos    map[string]*repo
	tooLarge map[string]time.Time
	// overBudget records that eviction left the cache over MaxSize because
	// the rest of it was in use.
	overBudget bool
	// reserved is the space reservations hold for fetches in progress, and
	// cutPending counts those cut off and not yet released. freed is closed
	// and replaced whenever a reservation gives back space or one cut off is
	// released.
	reserved   int64
	cutPending int
	freed      chan struct{}
	// failedWants records, by repository key and object ID, wants the
	// upstream could not provide.
	failedWants map[string]time.Time
}

// repo is a cached repository, or one being cloned. users, refreshing and
// the map entry itself are guarded by Cache.mu.
type repo struct {
	key string
	dir string

	users      int
	refreshing *refreshCall
	// writes serializes git fetches into the repository.
	writes chan struct{}

	size        atomic.Int64
	lastUsed    atomic.Int64
	lastChecked atomic.Int64
	usedWritten atomic.Int64
}

var errLocked = errors.New("locked by another process")

// maxConcurrentFetches caps clones and fetches running at once. Refreshes
// outlive the requests that start them, so without a cap a client could
// start any number and hang up.
const maxConcurrentFetches = 16

// uploadPacksPerCPU caps the upload-pack processes building packs at once,
// so a burst of clones cannot run the proxy out of memory. Each uses one
// delta-search thread (servingConfig); twice the CPUs keeps them busy while
// some packs wait on slow clients.
const uploadPacksPerCPU = 2

// New creates the cache, loading repositories already in CacheDir.
func New(opts Options) (*Cache, error) {
	gitPath, err := exec.LookPath("git")
	if err != nil {
		return nil, fmt.Errorf("upstream.git needs git installed: %w", err)
	}
	dir, err := filepath.Abs(opts.CacheDir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, dirPermissions); err != nil {
		return nil, err
	}
	lock, err := lockFile(filepath.Join(dir, lockFileName))
	if errors.Is(err, errLocked) {
		return nil, fmt.Errorf("git.cache_dir %s is in use by another process; each needs its own", dir)
	}
	if err != nil {
		return nil, fmt.Errorf("locking git.cache_dir %s: %w", dir, err)
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	// A repository larger than the whole cache would be evicted once
	// released and cloned again by the next request.
	if opts.MaxSize > 0 && (opts.MaxRepoSize <= 0 || opts.MaxRepoSize > opts.MaxSize) {
		opts.MaxRepoSize = opts.MaxSize
	}
	client := *opts.Client
	client.Timeout = 0
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	uploadPacks := uploadPacksPerCPU * runtime.GOMAXPROCS(0)
	if opts.maxUploadPacks > 0 {
		uploadPacks = opts.maxUploadPacks
	}
	c := &Cache{
		opts:            opts,
		client:          &client,
		logger:          logger,
		gitPath:         gitPath,
		dir:             dir,
		lock:            lock,
		fetches:         make(chan struct{}, maxConcurrentFetches),
		uploadPacks:     make(chan struct{}, uploadPacks),
		refCheckTimeout: defaultRefCheckTimeout,
		lowSpeedTime:    defaultLowSpeedTime,
		maxRequestBody:  defaultMaxRequestBody,
		repos:           make(map[string]*repo),
		tooLarge:        make(map[string]time.Time),
		failedWants:     make(map[string]time.Time),
		freed:           make(chan struct{}),
	}
	if opts.refCheckTimeout > 0 {
		c.refCheckTimeout = opts.refCheckTimeout
	}
	if opts.lowSpeedTime > 0 {
		c.lowSpeedTime = opts.lowSpeedTime
	}
	if opts.maxRequestBody > 0 {
		c.maxRequestBody = opts.maxRequestBody
	}
	if err := c.load(); err != nil {
		_ = lock.Close()
		return nil, err
	}
	if c.relay, err = newRelay(c.client, logger); err != nil {
		_ = lock.Close()
		return nil, fmt.Errorf("starting git relay: %w", err)
	}
	c.ctx, c.cancel = context.WithCancel(context.Background())
	// Applies a lowered git.max_size straight away.
	c.evict()
	return c, nil
}

// Close stops the refreshes in progress and waits for them to finish, so
// none is still writing to the cache when it returns, then stops the relay.
// Fetches by ID run on request goroutines; stop the HTTP server first.
// Closing again does nothing.
func (c *Cache) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	c.mu.Unlock()
	c.cancel()
	c.refreshes.Wait()
	return errors.Join(c.relay.close(), c.lock.Close())
}

// UpstreamDir names the top-level cache directory of the upstream called
// name at baseURL. It includes a hash of the URL, so repointing an upstream
// starts a fresh cache rather than serving the old host's repositories.
func UpstreamDir(name, baseURL string) string {
	digest := sha256.Sum256([]byte(strings.TrimSuffix(baseURL, "/")))
	return name + "-" + hex.EncodeToString(digest[:])[:upstreamHashLen]
}

func (c *Cache) tmpDir() string {
	return filepath.Join(c.dir, tmpDirName)
}

// load clears what an earlier run left in the temporary directory and
// registers the repositories already cached.
func (c *Cache) load() error {
	if err := os.RemoveAll(c.tmpDir()); err != nil {
		return fmt.Errorf("clearing %s: %w", c.tmpDir(), err)
	}
	if err := os.MkdirAll(c.tmpDir(), dirPermissions); err != nil {
		return err
	}
	c.removeRepointed()
	for _, upstreamDir := range c.opts.Upstreams {
		root := filepath.Join(c.dir, upstreamDir)
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || !d.IsDir() || !strings.HasSuffix(d.Name(), ".git") || path == root {
				return err
			}
			rel, _ := filepath.Rel(c.dir, path)
			rp := newRepo(filepath.ToSlash(rel), path)
			rp.size.Store(dirSize(path))
			rp.lastChecked.Store(stampTime(path, checkedStamp))
			rp.lastUsed.Store(stampTime(path, usedStamp))
			rp.usedWritten.Store(rp.lastUsed.Load())
			c.repos[rp.key] = rp
			return filepath.SkipDir
		})
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

// upstreamDirPattern matches a directory UpstreamDir names, capturing the
// upstream's name.
var upstreamDirPattern = regexp.MustCompile(`^(.+)-[0-9a-f]{8}$`)

// removeRepointed removes the directories of configured upstreams at URLs
// they no longer have. Each is moved aside first, so nothing can be read
// from it half deleted. Directories of upstreams no longer configured are
// only logged: nothing else says they are the cache's.
func (c *Cache) removeRepointed() {
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		c.logger.Warn("git cache: cannot list the cache directory", "error", err)
		return
	}
	current := make(map[string]bool, len(c.opts.Upstreams))
	for _, upstreamDir := range c.opts.Upstreams {
		current[upstreamDir] = true
	}
	var unconfigured []string
	for _, entry := range entries {
		name := entry.Name()
		match := upstreamDirPattern.FindStringSubmatch(name)
		if !entry.IsDir() || match == nil || current[name] {
			continue
		}
		if _, configured := c.opts.Upstreams[match[1]]; !configured {
			unconfigured = append(unconfigured, name)
			continue
		}
		trash, err := os.MkdirTemp(c.tmpDir(), "repointed-")
		if err == nil {
			err = os.Rename(filepath.Join(c.dir, name), filepath.Join(trash, name))
		}
		if err != nil {
			c.logger.Warn("git cache: failed to remove the cache of a repointed upstream", "directory", name, "error", err)
			continue
		}
		c.logger.Info("git cache: removed the cache of a repointed upstream", "directory", name)
		if err := os.RemoveAll(trash); err != nil {
			c.logger.Warn("git cache: failed to delete a removed cache", "path", trash, "error", err)
		}
	}
	if len(unconfigured) > 0 {
		c.logger.Info("git cache: leaving directories that look like caches of upstreams no longer configured; delete them if they are",
			"directories", unconfigured)
	}
}

// acquire returns the repository for key, counting the caller as a user
// until release, so it is not evicted while in use.
func (c *Cache) acquire(key string) *repo {
	c.mu.Lock()
	defer c.mu.Unlock()
	rp := c.repos[key]
	if rp == nil {
		rp = newRepo(key, filepath.Join(c.dir, filepath.FromSlash(key)))
		c.repos[key] = rp
	}
	rp.users++
	return rp
}

func (c *Cache) release(rp *repo) {
	now := time.Now()
	rp.lastUsed.Store(now.UnixNano())
	exists := rp.exists()
	if exists && now.Sub(time.Unix(0, rp.usedWritten.Load())) >= usedStampInterval {
		rp.usedWritten.Store(now.UnixNano())
		touch(rp.dir, usedStamp, now)
	}

	c.mu.Lock()
	rp.users--
	idle := rp.users == 0 && c.repos[rp.key] == rp
	// Forget repositories that never got cloned, such as ones missing upstream.
	if idle && !exists {
		delete(c.repos, rp.key)
	}
	_, tooLarge := c.tooLarge[rp.key]
	retry := tooLarge || c.overBudget
	c.mu.Unlock()
	if idle && exists && retry {
		c.evict()
	}
}

func newRepo(key, dir string) *repo {
	return &repo{key: key, dir: dir, writes: make(chan struct{}, 1)}
}

// lockWrites waits for other fetches into the repository to finish.
func (rp *repo) lockWrites(ctx context.Context) (func(), error) {
	select {
	case rp.writes <- struct{}{}:
		return func() { <-rp.writes }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (rp *repo) exists() bool {
	_, err := os.Stat(rp.dir)
	return err == nil
}

// evict removes repositories nobody is using: the least recently used until
// the cache fits in MaxSize, and any that could not be refreshed within
// MaxRepoSize, so the next request clones them again with only their
// branches and tags. It runs after every fetch, and when a repository it had
// to skip is released: one too large to refresh, or any while the cache is
// over MaxSize. Fetches also evict as they receive bytes (reservation.cover).
func (c *Cache) evict() {
	c.mu.Lock()
	removed, _ := c.evictLocked(0)
	c.mu.Unlock()
	c.removeEvicted(removed)
}

// evictLocked evicts until the cache, counting reservations, has room for
// extra more bytes, and returns the evicted repositories' directories, to be
// deleted once c.mu is released, and the size left. When evicting every idle
// repository would not make that room, it evicts only as evict does. A
// repository is moved aside under the lock, so a request that comes after it
// clones it again instead of reading it while it is deleted.
func (c *Cache) evictLocked(extra int64) ([]string, int64) {
	total := c.reserved
	var idleTotal int64
	idle := make([]*repo, 0, len(c.repos))
	for _, rp := range c.repos {
		total += rp.size.Load()
		if rp.users == 0 {
			idle = append(idle, rp)
			idleTotal += rp.size.Load()
		}
	}
	if total-idleTotal+extra > c.opts.MaxSize {
		extra = 0
	}
	slices.SortFunc(idle, func(a, b *repo) int {
		return cmp.Compare(a.lastUsed.Load(), b.lastUsed.Load())
	})
	var removed []string
	for _, rp := range idle {
		_, marked := c.tooLarge[rp.key]
		tooLarge := marked && rp.exists()
		if !tooLarge && (c.opts.MaxSize <= 0 || total+extra <= c.opts.MaxSize) {
			continue
		}
		trash, err := os.MkdirTemp(c.tmpDir(), "evicted-")
		if err == nil {
			err = os.Rename(rp.dir, filepath.Join(trash, "repo"))
		}
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			c.logger.Warn("git cache: failed to evict repository", "repository", rp.key, "error", err)
			continue
		}
		delete(c.repos, rp.key)
		total -= rp.size.Load()
		removed = append(removed, trash)
		reason := "lru"
		if tooLarge {
			// The clone that replaces it enforces the limit itself.
			delete(c.tooLarge, rp.key)
			reason = "too_large"
		}
		metrics.RecordArtifactEvicted(reason, ecosystem)
		c.logger.Info("git cache: evicted repository", "repository", rp.key, "size", rp.size.Load(), "reason", reason)
	}
	c.overBudget = c.opts.MaxSize > 0 && total > c.opts.MaxSize
	return removed, total
}

func (c *Cache) removeEvicted(removed []string) {
	for _, trash := range removed {
		if err := os.RemoveAll(trash); err != nil {
			c.logger.Warn("git cache: failed to delete evicted repository", "path", trash, "error", err)
		}
	}
}

func dirSize(dir string) int64 {
	var size int64
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			if info, err := d.Info(); err == nil {
				size += info.Size()
			}
		}
		return nil
	})
	return size
}

// Stamps are empty files whose modification time records an event, so it
// survives restarts.
func stampTime(dir, name string) int64 {
	info, err := os.Stat(filepath.Join(dir, name))
	if err != nil {
		return 0
	}
	return info.ModTime().UnixNano()
}

func touch(dir, name string, t time.Time) {
	path := filepath.Join(dir, name)
	if err := os.Chtimes(path, t, t); errors.Is(err, fs.ErrNotExist) {
		if f, err := os.Create(path); err == nil {
			_ = f.Close()
			_ = os.Chtimes(path, t, t)
		}
	}
}
