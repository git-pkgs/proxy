package handler

import (
	"bytes"
	"context"
	"errors"
	"io"
	"time"

	"github.com/git-pkgs/proxy/internal/metadata/npm"
)

const npmMissingVersionRefreshInterval = time.Minute

// refreshMissingPackument shares one refresh across requested versions of a
// routed package. Attempts, including failed ones, are limited per handler.
// The returned document stays in storage when streaming is available.
func (h *NPMHandler) refreshMissingPackument(ctx context.Context, packumentURL, cacheKey string) metadataResult {
	results := h.refreshFlight.DoChan(cacheKey, func() (any, error) {
		h.refreshMu.Lock()
		if time.Now().Before(h.refreshAfter[cacheKey]) {
			res := h.refreshResult[cacheKey]
			h.refreshMu.Unlock()
			return res, nil
		}
		if h.refreshAfter == nil {
			h.refreshAfter = make(map[string]time.Time)
			h.refreshResult = make(map[string]metadataResult)
		}
		h.refreshMu.Unlock()

		fetchCtx := context.WithoutCancel(ctx)
		res := h.storedPackument(fetchCtx, packumentURL, h.packumentAccept(), cacheKey, true)
		if errors.Is(res.err, errInMemory) {
			res = h.proxy.refreshMetadata(fetchCtx, "npm", cacheKey, packumentURL, h.packumentAccept(), "", h.packumentValidator())
		}
		if res.err == nil && res.stale {
			res.err = errRefetchFailed(res)
		}
		h.refreshMu.Lock()
		deadline := time.Now().Add(npmMissingVersionRefreshInterval)
		h.refreshAfter[cacheKey] = deadline
		h.refreshResult[cacheKey] = res
		time.AfterFunc(npmMissingVersionRefreshInterval, func() {
			h.refreshMu.Lock()
			if h.refreshAfter[cacheKey].Equal(deadline) {
				delete(h.refreshAfter, cacheKey)
				delete(h.refreshResult, cacheKey)
			}
			h.refreshMu.Unlock()
		})
		h.refreshMu.Unlock()
		return res, nil
	})
	select {
	case <-ctx.Done():
		return metadataResult{err: ctx.Err()}
	case result := <-results:
		res, ok := result.Val.(metadataResult)
		if !ok {
			return metadataResult{err: errSharedFetchAbandoned}
		}
		return res
	}
}

// readRefreshedNPMVersion reads each waiter's own version from the shared
// refresh, not the version the caller that started it requested.
func (h *NPMHandler) readRefreshedNPMVersion(ctx context.Context, res metadataResult, version string) (npmVersionData, error) {
	if res.err != nil {
		return npmVersionData{}, res.err
	}
	var found npm.VersionEntry
	read := func(in io.Reader) (err error) {
		found, err = npm.LookupVersion(in, version)
		return err
	}
	var err error
	if res.entry != nil {
		err = readStored(func() (io.ReadCloser, error) { return h.proxy.openStoredMetadata(ctx, res.entry) }, read)
	} else {
		err = read(bytes.NewReader(res.body))
	}
	return npmVersionData{entry: found.Entry, published: found.Published}, err
}
