package gitcache

import (
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
)

// gitProtocolPattern is what a Git-Protocol header may hold, such as
// "version=2". It is passed to upload-pack in its environment.
var gitProtocolPattern = regexp.MustCompile(`^[A-Za-z0-9=:._-]*$`)

var errInvalidKey = errors.New("invalid repository key")

// Lease is a use of a cached repository. The repository is not evicted
// until the lease is released.
type Lease struct {
	cache      *Cache
	repo       *repo
	repoURL    string
	hit        bool
	fetchWants bool
}

// OpenOptions says what Open does besides returning the repository.
type OpenOptions struct {
	// Refresh brings a cached repository up to date with the upstream first,
	// or leaves it as it is when the upstream fails or is slow and the copy
	// is recent enough (see Options).
	Refresh bool
	// FetchWants lets ServeUploadPack fetch commits a client asks for by ID
	// that the cache lacks. Leave it off where a fetch by ID could reach
	// commits the client may not read through this repository, such as
	// GitHub forks with credentials configured.
	FetchWants bool
}

// Open returns a lease on the repository cached under key, a relative path
// such as github-1a2b3c4d/acme/widgets.git, whose upstream is repoURL. A
// repository not cached yet is cloned first.
func (c *Cache) Open(ctx context.Context, key, repoURL string, opts OpenOptions) (*Lease, error) {
	if !strings.HasSuffix(key, ".git") || !filepath.IsLocal(filepath.FromSlash(key)) {
		return nil, errInvalidKey
	}
	rp := c.acquire(key)
	hit := rp.exists()
	if opts.Refresh || !hit {
		if err := c.ensureFresh(ctx, rp, repoURL); err != nil {
			c.release(rp)
			return nil, err
		}
	}
	return &Lease{cache: c, repo: rp, repoURL: repoURL, hit: hit, fetchWants: opts.FetchWants}, nil
}

// Hit reports whether the repository was already cached when it was opened.
func (l *Lease) Hit() bool {
	return l.hit
}

// Release ends the lease.
func (l *Lease) Release() {
	l.cache.release(l.repo)
}

// ServeInfoRefs answers GET info/refs?service=git-upload-pack.
func (l *Lease) ServeInfoRefs(w http.ResponseWriter, r *http.Request) {
	protocol := gitProtocol(r)
	setNoCache(w)
	w.Header().Set("Content-Type", advertisementType)
	// Protocol v2 starts with its version line; earlier versions with the
	// service name.
	if !isProtocolV2(protocol) {
		_, _ = io.WriteString(w, pktLine("# service=git-upload-pack\n")+pktFlush)
	}
	l.cache.uploadPack(w, r, l.repo, protocol, nil, "--advertise-refs")
}

// ServeUploadPack answers POST git-upload-pack. With OpenOptions.FetchWants,
// commits the request wants that the cache lacks are fetched from the
// upstream first.
func (l *Lease) ServeUploadPack(w http.ResponseWriter, r *http.Request) {
	c := l.cache
	body := io.Reader(r.Body)
	if r.Header.Get("Content-Encoding") == "gzip" {
		gz, err := gzip.NewReader(r.Body)
		if err != nil {
			http.Error(w, "invalid gzip body", http.StatusBadRequest)
			return
		}
		defer func() { _ = gz.Close() }()
		body = gz
	}
	if l.fetchWants {
		var wants []string
		wants, body = readWants(body)
		if err := c.fetchMissingWants(r.Context(), l.repo, l.repoURL, wants); err != nil && r.Context().Err() == nil {
			// upload-pack reports the missing objects to the client.
			c.logger.Warn("git cache: fetching wanted objects failed", "repository", l.repo.key, "error", err)
		}
	}
	setNoCache(w)
	w.Header().Set("Content-Type", resultType)
	c.uploadPack(w, r, l.repo, gitProtocol(r), body)
}

// uploadPack runs git upload-pack on the cached repository in stateless mode,
// the way git http-backend does.
func (c *Cache) uploadPack(w http.ResponseWriter, r *http.Request, rp *repo, protocol string, stdin io.Reader, args ...string) {
	var env []string
	if protocol != "" {
		env = append(env, "GIT_PROTOCOL="+protocol)
	}
	args = append(append([]string{"upload-pack", "--stateless-rpc"}, args...), rp.dir)
	cmd := c.command(r.Context(), env, args...)
	cmd.Stdin = stdin
	cmd.Stdout = w
	stderr := &limitedBuffer{max: maxStderr}
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil && r.Context().Err() == nil {
		c.logger.Warn("git cache: upload-pack failed",
			"repository", rp.key, "error", err, "stderr", strings.TrimSpace(stderr.String()))
	}
}

func gitProtocol(r *http.Request) string {
	if protocol := r.Header.Get("Git-Protocol"); gitProtocolPattern.MatchString(protocol) {
		return protocol
	}
	return ""
}

func isProtocolV2(protocol string) bool {
	for field := range strings.SplitSeq(protocol, ":") {
		if field == "version=2" {
			return true
		}
	}
	return false
}

func setNoCache(w http.ResponseWriter) {
	w.Header().Set("Expires", "Fri, 01 Jan 1980 00:00:00 GMT")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Cache-Control", "no-cache, max-age=0, must-revalidate")
}
