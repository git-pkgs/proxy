package gitcache

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

var (
	// ErrNotCached is returned by Remove for a repository the cache does
	// not hold.
	ErrNotCached = errors.New("repository is not cached")
	// ErrInUse is returned by Remove for a repository a clone, a fetch or
	// a client is using.
	ErrInUse = errors.New("repository is in use")
)

// Remove deletes the cached repository under key, as eviction does, and
// returns its size. The next request for it clones it again. A repository
// in use is not removed and returns ErrInUse, so nothing reads it while it
// is deleted.
func (c *Cache) Remove(key string) (int64, error) {
	c.mu.Lock()
	rp := c.repos[c.repoKey(key)]
	if rp == nil {
		c.mu.Unlock()
		return 0, ErrNotCached
	}
	// Before the directory check: a first clone is built elsewhere and
	// moved into place once done.
	if rp.users > 0 {
		c.mu.Unlock()
		return 0, ErrInUse
	}
	if !rp.exists() {
		c.mu.Unlock()
		return 0, ErrNotCached
	}
	// Moved aside under the lock, so a request that comes after it clones
	// the repository again instead of reading it while it is deleted.
	trash, err := os.MkdirTemp(c.tmpDir(), "removed-")
	if err == nil {
		err = os.Rename(rp.dir, filepath.Join(trash, "repo"))
	}
	if err != nil {
		c.mu.Unlock()
		if trash != "" {
			_ = os.Remove(trash)
		}
		return 0, err
	}
	delete(c.repos, rp.key)
	delete(c.tooLarge, rp.key)
	// A want the old copy could not fit may fit in the next clone.
	for k := range c.failedWants {
		if strings.HasPrefix(k, rp.key+" ") {
			delete(c.failedWants, k)
		}
	}
	size := rp.size.Load()
	c.mu.Unlock()

	c.logger.Info("git cache: removed repository", "repository", rp.key, "size", size)
	c.removeEvicted([]string{trash})
	return size, nil
}
