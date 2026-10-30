package handler

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/git-pkgs/artifacts"
	"github.com/git-pkgs/proxy/internal/metrics"
	"github.com/git-pkgs/registries/fetch"
	"github.com/opencontainers/go-digest"
)

// On a cache miss a marked request is sent the bytes as they arrive from
// upstream while they are stored, rather than after the whole artifact is
// stored. Clients with idle timeouts need this for large artifacts: git-lfs
// gives up after 30 seconds without data, pacman after 10. The caller that
// starts the shared fetch is handed the stream; callers joining it still wait
// for the stored artifact.

type streamCacheMissKey struct{}

// StreamCacheMiss marks ctx so that on an artifact cache miss the bytes are
// sent to the client while they are being stored. The server sets it on GET
// requests without a Range header; a range of a stream that is still
// arriving can't be served.
func StreamCacheMiss(ctx context.Context) context.Context {
	return context.WithValue(ctx, streamCacheMissKey{}, true)
}

func streamsCacheMiss(ctx context.Context) bool {
	stream, _ := ctx.Value(streamCacheMissKey{}).(bool)
	return stream && !servingHead(ctx)
}

// missHandoff passes the client's stream from the shared fetch, which runs on
// its own goroutine, to the caller that started it. The caller can leave
// before a stream is offered, and the fetch must then not write to a stream
// nobody reads.
type missHandoff struct {
	mu        sync.Mutex
	ready     chan struct{}
	stream    *CacheResult
	taken     bool
	abandoned bool
	// feed is the stream's source. Set and read only on the fetch's
	// goroutine.
	feed *missFeed
}

type missHandoffKey struct{}

func handoffFrom(ctx context.Context) *missHandoff {
	h, _ := ctx.Value(missHandoffKey{}).(*missHandoff)
	return h
}

// offer hands stream to the caller and reports whether it took it.
func (h *missHandoff) offer(stream *CacheResult) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.abandoned {
		return false
	}
	h.stream, h.taken = stream, true
	close(h.ready)
	return true
}

// holdUnrecorded gives a client that fell behind, and reads the stored copy
// of u, its own hold on u until it closes, so u isn't deleted under it.
func (h *missHandoff) holdUnrecorded(u *unrecordedObject) {
	if h.feed != nil {
		h.feed.holdUntilClose(u)
	}
}

// handedOver reports whether the caller took a stream.
func (h *missHandoff) handedOver() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.taken
}

// runSharedFetchStreaming runs the shared fetch on its own goroutine and
// returns as soon as it offers a stream, or with its result if it finishes
// without one: on a recheck hit, an artifact another instance stored, an
// error, or an artifact a block-mode scanner has to see first. Waiters and
// the fetch lease still see the fetch end only once the artifact is stored.
func (p *Proxy) runSharedFetchStreaming(ctx context.Context, key string, f *inflightFetch, recheck func() (artifacts.Artifact, string, bool), commit func(context.Context) (artifacts.Artifact, string, error)) (*CacheResult, error) {
	h := &missHandoff{ready: make(chan struct{})}
	type outcome struct {
		res *CacheResult
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := p.runSharedFetch(context.WithValue(ctx, missHandoffKey{}, h), key, f, recheck, commit)
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.taken || h.abandoned {
			// The caller has the stream, or has gone; nobody reads this.
			if res != nil && res.Reader != nil {
				_ = res.Reader.Close()
			}
			return
		}
		done <- outcome{res, err}
	}()

	select {
	case <-h.ready:
		return h.stream, nil
	case o := <-done:
		return o.res, o.err
	case <-ctx.Done():
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.taken {
			return h.stream, nil
		}
		h.abandoned = true
		select {
		case o := <-done:
			if o.res != nil && o.res.Reader != nil {
				_ = o.res.Reader.Close()
			}
		default:
		}
		return nil, ctx.Err()
	}
}

// storeFetched stores an artifact fetched from upstream. When the caller
// that started the fetch is waiting for a stream, it is sent the bytes as
// they are stored; see storeWhileServing.
func (p *Proxy) storeFetched(ctx context.Context, ecosystem, name, version, filename, pkgPURL, versionPURL, upstreamURL, upstreamHash, declared string, artifact *fetch.Artifact) (artifacts.Artifact, string, error) {
	h := handoffFrom(ctx)
	if h == nil || p.Scanners.Blocks(ecosystem) {
		return p.storeArtifact(ctx, ecosystem, name, version, filename, pkgPURL, versionPURL, upstreamURL, upstreamHash, declared, artifact)
	}
	return p.storeWhileServing(ctx, h, ecosystem, name, version, filename, pkgPURL, versionPURL, upstreamURL, upstreamHash, declared, artifact)
}

// errNoReaderLeft stops reading upstream once neither storage nor the
// client takes the bytes any more.
var errNoReaderLeft = errors.New("neither storage nor the client is reading the artifact")

// storeWhileServing copies the upstream body to storage and to the client at
// once, at the upstream's speed; see missFeed for a client that can't keep up.
// The client's copy is checked before its last bytes, so a mismatch aborts
// the response. A storage failure leaves the client's copy running.
func (p *Proxy) storeWhileServing(ctx context.Context, h *missHandoff, ecosystem, name, version, filename, pkgPURL, versionPURL, upstreamURL, upstreamHash, declared string, artifact *fetch.Artifact) (artifacts.Artifact, string, error) {
	hash := strings.ToLower(upstreamHash)
	checks, err := newIntegrityChecks(hash, declared)
	if err != nil {
		_ = artifact.Body.Close()
		return artifacts.Artifact{}, "", err
	}

	feed := newMissFeed(clientFeedLimit)
	var client io.ReadCloser = feed
	if hash != "" || declared != "" {
		if client, err = checks.wrapFailOnMismatch(feed, func(reason string) {
			p.Logger.Warn("artifact sent on a cache miss failed its integrity check; aborting the response",
				"purl", versionPURL, "filename", filename, "reason", reason)
		}); err != nil {
			_ = artifact.Body.Close()
			return artifacts.Artifact{}, "", err
		}
	}
	stream := &CacheResult{
		Reader: client,
		Artifact: artifacts.Artifact{
			PURL:      versionPURL,
			Filename:  filename,
			MediaType: artifact.ContentType,
			// Unknown until stored, so the response goes out chunked and
			// can still be aborted.
			Size: -1,
		},
	}
	if hash != "" {
		stream.Artifact.Digest = digest.Digest("sha256:" + hash)
	}
	if !h.offer(stream) {
		_ = feed.Close()
		return p.storeArtifact(ctx, ecosystem, name, version, filename, pkgPURL, versionPURL, upstreamURL, upstreamHash, declared, artifact)
	}
	h.feed = feed

	storeR, storeW := io.Pipe()
	type stored struct {
		artifact artifacts.Artifact
		path     string
		err      error
	}
	storeDone := make(chan stored, 1)
	go func() {
		// Not cancelled with ctx: the client's request ends once it has the
		// last byte, while storage may still be finishing the upload and the
		// database write. Reading the upstream still stops with ctx, which
		// ends the copy to storage too.
		a, path, err := p.storeArtifact(context.WithoutCancel(ctx), ecosystem, name, version, filename, pkgPURL, versionPURL, upstreamURL, upstreamHash, declared,
			&fetch.Artifact{Body: storeR, Size: artifact.Size, ContentType: artifact.ContentType, ETag: artifact.ETag})
		// storeArtifact closes storeR; this covers a return before it read
		// anything, so the copy below never blocks on it.
		_ = storeR.Close()
		storeDone <- stored{a, path, err}
	}()

	readErr := copyToBoth(artifact.Body, storeW, feed)
	_ = artifact.Body.Close()
	_ = storeW.CloseWithError(readErr)
	feed.end(readErr)

	out := <-storeDone
	if feed.lagging() {
		// Opened before returning, so an unrecorded object is still there;
		// runSharedFetch then gives the client a hold on it.
		feed.resolve(p.openStoredForFeed(ctx, out.artifact, out.path, out.err))
	}
	if out.err != nil && readErr == nil && feed.fedFully() {
		p.Logger.Error("artifact sent to the client but not cached",
			"purl", versionPURL, "filename", filename, "url", upstreamURL, "error", out.err)
		metrics.RecordArtifactServedUncached(ecosystem, metrics.FailureDetail{
			PURL: versionPURL, Filename: filename, URL: upstreamURL, Err: out.err})
	}
	return out.artifact, out.path, out.err
}

// openStoredForFeed opens the stored copy for a client that fell behind. An
// unrecorded object is opened too: it is deleted only after this returns, and
// the client holds it from then on (missHandoff.holdUnrecorded).
func (p *Proxy) openStoredForFeed(ctx context.Context, a artifacts.Artifact, path string, storeErr error) (io.ReadCloser, error) {
	if storeErr != nil && !errors.Is(storeErr, errUnrecorded) {
		return nil, storeErr
	}
	res, err := p.openStoredArtifact(ctx, a, path)
	if err != nil {
		return nil, err
	}
	return res.Reader, nil
}

// copyToBoth copies src to store and to the client's feed until src ends,
// dropping the store once a write to it fails. It returns the error that
// ended the copy, nil at the end of src. Feeding the client never blocks.
func copyToBoth(src io.Reader, store io.Writer, client *missFeed) error {
	buffer := artifactCopyBufferPool.Get().(*[]byte)
	defer artifactCopyBufferPool.Put(buffer)
	buf := *buffer
	storeOK := true
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if storeOK {
				if _, werr := store.Write(buf[:n]); werr != nil {
					storeOK = false
				}
			}
			// A client that has gone, or that waits for the stored copy,
			// has no use for the rest once storing has failed.
			if !client.push(buf[:n]) && !storeOK {
				return errNoReaderLeft
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// clientFeedLimit bounds the bytes held for a client that reads a streamed
// miss slower than they arrive. A variable so tests can lower it.
var clientFeedLimit = 8 << 20

// missFeed is the client's side of a streamed miss. The upstream read pushes
// into it without blocking, so a slow client can't hold back the copy to
// storage, the fetch lease or the callers waiting on the fetch. Once the
// client is more than the limit behind, it stops being fed: it reads what
// is held, then continues from the stored copy where it left off.
type missFeed struct {
	mu     sync.Mutex
	cond   *sync.Cond
	chunks [][]byte
	held   int
	limit  int
	// fed counts the bytes pushed before the client fell behind, where it
	// continues in the stored copy.
	fed    int64
	ended  bool
	endErr error
	behind bool
	closed bool
	early  bool // closed before reading everything it was fed

	stored    io.ReadCloser
	storedErr error
	resolved  bool
	skipped   bool
	// release drops the client's hold on an unrecorded stored copy.
	release func()
}

func newMissFeed(limit int) *missFeed {
	m := &missFeed{limit: limit}
	m.cond = sync.NewCond(&m.mu)
	return m
}

// push queues a copy of b and reports whether the client is still fed.
func (m *missFeed) push(b []byte) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.behind {
		return false
	}
	if m.held+len(b) > m.limit {
		m.behind = true
		return false
	}
	m.chunks = append(m.chunks, append([]byte(nil), b...))
	m.held += len(b)
	m.fed += int64(len(b))
	m.cond.Broadcast()
	return true
}

// end records how the upstream read ended.
func (m *missFeed) end(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ended, m.endErr = true, err
	m.cond.Broadcast()
}

func (m *missFeed) lagging() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.behind && !m.closed
}

// fedFully reports whether the client was sent every byte from upstream.
func (m *missFeed) fedFully() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return !m.behind && !m.early
}

// resolve gives a client that fell behind the stored copy, or the error
// that left none.
func (m *missFeed) resolve(r io.ReadCloser, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		if r != nil {
			_ = r.Close()
		}
		return
	}
	m.stored, m.storedErr, m.resolved = r, err, true
	m.cond.Broadcast()
}

// holdUntilClose takes a hold on u for a client reading its stored copy and
// releases it when the client closes.
func (m *missFeed) holdUntilClose(u *unrecordedObject) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.stored == nil {
		return
	}
	u.refs.Add(1)
	m.release = u.release
}

func (m *missFeed) Read(p []byte) (int, error) {
	m.mu.Lock()
	for {
		switch {
		case m.closed:
			m.mu.Unlock()
			return 0, io.ErrClosedPipe
		case len(m.chunks) > 0:
			n := copy(p, m.chunks[0])
			if m.chunks[0] = m.chunks[0][n:]; len(m.chunks[0]) == 0 {
				m.chunks[0] = nil
				m.chunks = m.chunks[1:]
			}
			m.held -= n
			m.mu.Unlock()
			return n, nil
		case m.behind && m.resolved:
			r, err, skipped := m.stored, m.storedErr, m.skipped
			m.skipped = true
			m.mu.Unlock()
			if err != nil {
				return 0, err
			}
			if !skipped {
				if err := skipTo(r, m.fed); err != nil {
					return 0, err
				}
			}
			return r.Read(p)
		case !m.behind && m.ended:
			err := m.endErr
			m.mu.Unlock()
			if err == nil {
				err = io.EOF
			}
			return 0, err
		}
		m.cond.Wait()
	}
}

func (m *missFeed) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	m.early = !m.ended || len(m.chunks) > 0
	m.chunks, m.held = nil, 0
	if m.stored != nil {
		_ = m.stored.Close()
	}
	release := m.release
	m.release = nil
	m.cond.Broadcast()
	m.mu.Unlock()
	if release != nil {
		// Outside the lock: the last release deletes the object.
		release()
	}
	return nil
}

// skipTo moves r past the first n bytes.
func skipTo(r io.Reader, n int64) error {
	if s, ok := r.(io.Seeker); ok {
		_, err := s.Seek(n, io.SeekStart)
		return err
	}
	if _, err := io.CopyN(io.Discard, r, n); err != nil {
		return fmt.Errorf("skipping to byte %d of the stored artifact: %w", n, err)
	}
	return nil
}
