package gitcache

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
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
	c := l.cache
	if !takeSlot(r.Context(), c.refListings) {
		return
	}
	defer func() { <-c.refListings }()
	protocol := gitProtocol(r)
	setNoCache(w)
	w.Header().Set("Content-Type", advertisementType)
	// Protocol v2 starts with its version line; earlier versions with the
	// service name.
	if !isProtocolV2(protocol) {
		_, _ = io.WriteString(w, pktLine("# service=git-upload-pack\n")+pktFlush)
	}
	c.uploadPack(w, r, l.repo, protocol, nil, "--advertise-refs")
}

// ServeUploadPack answers POST git-upload-pack with req, the request's body
// as ReadRequest read it. With OpenOptions.FetchWants, commits the request
// wants that the cache lacks are fetched from the upstream first.
func (l *Lease) ServeUploadPack(w http.ResponseWriter, r *http.Request, req *Request) {
	c := l.cache
	body := req.reader()
	if req.gzip {
		gz, err := gzip.NewReader(body)
		if err != nil {
			http.Error(w, "invalid gzip body", http.StatusBadRequest)
			return
		}
		defer func() { _ = gz.Close() }()
		// A gzip stream of any length can decode to almost nothing.
		decoded := &bodyLimit{r: gz, remaining: c.maxRequestBody}
		defer func() {
			if decoded.exceeded.Load() {
				c.logger.Warn("git cache: request body passed the limit",
					"repository", l.repo.key, "limit", c.maxRequestBody)
			}
		}()
		body = decoded
	}
	if l.fetchWants {
		var wants []string
		wants, body = readWants(body)
		if err := c.fetchMissingWants(r.Context(), l.repo, l.repoURL, wants); err != nil && r.Context().Err() == nil {
			// upload-pack reports the missing objects to the client.
			c.logger.Warn("git cache: fetching wanted objects failed", "repository", l.repo.key, "error", err)
		}
	}
	var command string
	command, body = requestCommand(body)
	// ls-refs builds no pack, so it waits for a ref listing slot rather than
	// behind clones.
	slots := c.uploadPacks
	if command == "ls-refs" {
		slots = c.refListings
	}
	if !takeSlot(r.Context(), slots) {
		return
	}
	defer func() { <-slots }()
	setNoCache(w)
	w.Header().Set("Content-Type", resultType)
	c.uploadPack(w, r, l.repo, gitProtocol(r), body)
}

// takeSlot takes a slot from slots, or reports false once ctx is done.
func takeSlot(ctx context.Context, slots chan struct{}) bool {
	select {
	case slots <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}

// requestCommand returns the protocol v2 command an upload-pack request
// starts with, such as "fetch", or "" for an earlier protocol, with a reader
// that replays the whole body.
func requestCommand(body io.Reader) (string, io.Reader) {
	var consumed bytes.Buffer
	_, line, _ := newPktReader(io.TeeReader(body, &consumed)).next()
	command, ok := strings.CutPrefix(line, "command=")
	if !ok {
		command = ""
	}
	return command, io.MultiReader(&consumed, body)
}

// servingConfig gives each pack-objects that upload-pack runs one
// delta-search thread with a bounded window, where git defaults to a thread
// per CPU and no bound.
var servingConfig = []string{"-c", "pack.threads=1", "-c", "pack.windowMemory=64m"}

// uploadPack runs git upload-pack on the cached repository in stateless mode,
// the way git http-backend does.
func (c *Cache) uploadPack(w http.ResponseWriter, r *http.Request, rp *repo, protocol string, stdin io.Reader, args ...string) {
	var env []string
	if protocol != "" {
		env = append(env, "GIT_PROTOCOL="+protocol)
	}
	args = slices.Concat(servingConfig, []string{"upload-pack", "--stateless-rpc"}, args, []string{rp.dir})
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

// defaultMaxRequestBody bounds an upload-pack request far above any real
// negotiation, so a small gzip body cannot feed upload-pack for as long as
// the request may run.
const defaultMaxRequestBody = 100 << 20

// maxMemoryRequest is the most of a request body kept in memory; the rest of
// a larger one goes to a temporary file.
const maxMemoryRequest = 1 << 20

// ErrRequestTooLarge is returned by ReadRequest for a body past the limit.
var ErrRequestTooLarge = errors.New("request body too large")

// Request is the body of a POST git-upload-pack request, read in full
// before anything is started for it. Stateless requests are complete once
// sent, so a client that sends one slowly, or not at all, holds no
// upload-pack process and is bound by the server's read timeout.
type Request struct {
	gzip bool
	data []byte
	file *os.File
}

// ReadRequest reads the body of a POST git-upload-pack request, still
// encoded. A body past the limit fails with ErrRequestTooLarge. Close the
// request once it is served.
func (c *Cache) ReadRequest(r *http.Request) (*Request, error) {
	req := &Request{gzip: r.Header.Get("Content-Encoding") == "gzip"}
	body := &bodyLimit{r: r.Body, remaining: c.maxRequestBody}
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(io.LimitReader(body, maxMemoryRequest)); err != nil {
		return nil, err
	}
	if buf.Len() < maxMemoryRequest {
		req.data = buf.Bytes()
		return req, nil
	}
	file, err := os.CreateTemp("", "git-upload-pack-*")
	if err != nil {
		return nil, err
	}
	req.file = file
	if _, err := buf.WriteTo(file); err != nil {
		req.Close()
		return nil, err
	}
	if _, err := io.Copy(file, body); err != nil {
		req.Close()
		return nil, err
	}
	return req, nil
}

// Close removes any temporary file holding the request.
func (req *Request) Close() {
	if req.file != nil {
		_ = req.file.Close()
		_ = os.Remove(req.file.Name())
	}
}

func (req *Request) reader() io.Reader {
	if req.file != nil {
		return io.NewSectionReader(req.file, 0, 1<<63-1)
	}
	return bytes.NewReader(req.data)
}

// bodyLimit fails reads past remaining bytes, where io.LimitReader would end
// the body quietly and leave upload-pack a request cut short.
type bodyLimit struct {
	r         io.Reader
	remaining int64
	exceeded  atomic.Bool
}

func (b *bodyLimit) Read(p []byte) (int, error) {
	if b.exceeded.Load() {
		return 0, ErrRequestTooLarge
	}
	// One byte past the limit tells a longer body from one that ends at it.
	if int64(len(p)) > b.remaining+1 {
		p = p[:b.remaining+1]
	}
	n, err := b.r.Read(p)
	if int64(n) <= b.remaining {
		b.remaining -= int64(n)
		return n, err
	}
	n = int(b.remaining)
	b.remaining = 0
	b.exceeded.Store(true)
	return n, ErrRequestTooLarge
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
