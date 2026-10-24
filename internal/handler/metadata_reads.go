package handler

import (
	"context"
	"runtime"
	"slices"
	"sync"
	"unsafe"
	"weak"

	"github.com/git-pkgs/proxy/internal/database"
)

// metadataReads shares the bytes of cached metadata documents among the
// requests that read them, so concurrent cache hits on one document hold a
// single copy rather than one each. Requests that arrive while a read is
// running wait for it, and requests that arrive later reuse its bytes for as
// long as any earlier request still holds them. Nothing here keeps a document
// alive: the map holds weak pointers, and an entry goes away once its bytes
// are collected. The zero value is ready to use.
type metadataReads struct {
	mu    sync.Mutex
	reads map[metadataReadKey]*sharedMetadataRead
}

// metadataReadKey identifies one stored document. The digest is the hash of
// the stored bytes, so a refetch that changes the document changes the key.
type metadataReadKey struct {
	path   string
	digest string
}

// sharedMetadataRead is one read of a cached document. data, n and err are
// written before done closes and read only after.
type sharedMetadataRead struct {
	done chan struct{}
	data weak.Pointer[byte]
	n    int
	err  error

	// waiters counts callers that waited for the read, guarded by the map's
	// mutex. Tests read it to know every caller has joined.
	waiters int
}

// bytes returns the document if it has not been collected.
func (r *sharedMetadataRead) bytes() ([]byte, bool) {
	if r.n == 0 {
		return []byte{}, true
	}
	ptr := r.data.Value()
	if ptr == nil {
		return nil, false
	}
	return unsafe.Slice(ptr, r.n), true
}

// read returns the bytes of entry's stored document, running load only when
// no other request is reading or holding them. Rows without a digest are
// read on their own. The returned bytes may be shared and must not be
// modified.
func (m *metadataReads) read(ctx context.Context, entry *database.MetadataCacheEntry, load func(context.Context) ([]byte, error)) ([]byte, error) {
	if !entry.ContentDigest.Valid || entry.ContentDigest.String == "" {
		return load(ctx)
	}
	key := metadataReadKey{path: entry.StoragePath, digest: entry.ContentDigest.String}
	for {
		m.mu.Lock()
		if m.reads == nil {
			m.reads = make(map[metadataReadKey]*sharedMetadataRead)
		}
		r, ok := m.reads[key]
		if !ok {
			r = &sharedMetadataRead{done: make(chan struct{}), err: errSharedFetchAbandoned}
			m.reads[key] = r
			m.mu.Unlock()
			return m.load(ctx, key, r, load)
		}
		r.waiters++
		m.mu.Unlock()

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-r.done:
		}
		if r.err != nil {
			return nil, r.err
		}
		if data, ok := r.bytes(); ok {
			return data, nil
		}
		// Collected before we got it: drop the entry and read again.
		m.forget(key, r)
	}
}

// load runs load for r and publishes the result. The read runs detached from
// the caller's cancellation, as waiters depend on it.
func (m *metadataReads) load(ctx context.Context, key metadataReadKey, r *sharedMetadataRead, load func(context.Context) ([]byte, error)) ([]byte, error) {
	var data []byte
	defer func() {
		// A failed or empty read is not kept: the next request tries again.
		if r.err != nil || r.n == 0 {
			m.forget(key, r)
		}
		close(r.done)
	}()
	data, r.err = load(context.WithoutCancel(ctx))
	if r.err != nil {
		return nil, r.err
	}
	// Every caller gets the bytes without spare capacity, so an append by
	// one never writes where another could see it.
	data = slices.Clip(data)
	r.n = len(data)
	if r.n > 0 {
		r.data = weak.Make(&data[0])
		runtime.AddCleanup(&data[0], func(key metadataReadKey) { m.forget(key, r) }, key)
	}
	return data, nil
}

// forget removes r from the map if it is still the entry for key.
func (m *metadataReads) forget(key metadataReadKey, r *sharedMetadataRead) {
	m.mu.Lock()
	if m.reads[key] == r {
		delete(m.reads, key)
	}
	m.mu.Unlock()
}
