package handler

import (
	"bytes"
	"context"
	"errors"
	"io"
	"time"

	"github.com/git-pkgs/proxy/internal/metadata/npm"
	"github.com/git-pkgs/proxy/internal/storage"
)

const npmMissingVersionRefreshInterval = time.Minute

// refreshMissingPackument shares one refresh across requested versions of a
// routed package. Without cooldown, attempts (including failures) are limited
// per package in each proxy process. Under cooldown every non-concurrent
// request checks upstream again, so a new publish cannot bypass cooldown.
func (h *NPMHandler) refreshMissingPackument(ctx context.Context, packumentURL, cacheKey string, retain bool) metadataResult {
	results := h.refreshFlight.DoChan(cacheKey, func() (any, error) {
		if retain {
			if res, limited := h.limitedPackumentRefresh(cacheKey); limited {
				return res, nil
			}
		}
		fetchCtx := ctx
		if h.proxy.acquireMetadataDetachSlot() {
			defer h.proxy.releaseMetadataDetachSlot()
			fetchCtx = context.WithoutCancel(ctx)
		}
		res := h.storedPackument(fetchCtx, packumentURL, h.packumentAccept(), cacheKey, true)
		if errors.Is(res.err, errInMemory) {
			res = h.proxy.refreshMetadata(fetchCtx, "npm", cacheKey, packumentURL, h.packumentAccept(), "", h.packumentValidator())
		}
		if res.err == nil && res.stale {
			res.err = errRefetchFailed(res)
		}
		if retain {
			h.retainPackumentRefresh(cacheKey, res.err)
		}
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

func (h *NPMHandler) limitedPackumentRefresh(cacheKey string) (metadataResult, bool) {
	h.refreshMu.Lock()
	defer h.refreshMu.Unlock()
	if time.Now().Before(h.refreshAfter[cacheKey]) {
		return h.refreshResult[cacheKey], true
	}
	return metadataResult{}, false
}

// Retain only the outcome, not a document or a row pointing to bytes that may
// be replaced. Later successful waiters read the current cached packument.
func (h *NPMHandler) retainPackumentRefresh(cacheKey string, err error) {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return
	}
	h.refreshMu.Lock()
	defer h.refreshMu.Unlock()
	if h.refreshAfter == nil {
		h.refreshAfter = make(map[string]time.Time)
		h.refreshResult = make(map[string]metadataResult)
	}
	deadline := time.Now().Add(npmMissingVersionRefreshInterval)
	h.refreshAfter[cacheKey] = deadline
	h.refreshResult[cacheKey] = metadataResult{err: err}
	time.AfterFunc(npmMissingVersionRefreshInterval, func() {
		h.refreshMu.Lock()
		if h.refreshAfter[cacheKey].Equal(deadline) {
			delete(h.refreshAfter, cacheKey)
			delete(h.refreshResult, cacheKey)
		}
		h.refreshMu.Unlock()
	})
}

// readRefreshedNPMVersion reads each waiter's own version from the shared
// refresh, not the version the caller that started it requested.
func (h *NPMHandler) readRefreshedNPMVersion(ctx context.Context, res metadataResult, packumentURL, cacheKey, version string, retain bool) (npmVersionData, error) {
	if res.err != nil {
		return npmVersionData{}, res.err
	}
	if res.entry == nil && res.body == nil {
		return h.recoverRefreshedNPMVersion(ctx, packumentURL, cacheKey, version, retain)
	}
	data, err := h.readNPMVersionResult(ctx, res, version)
	if errors.Is(err, errStoredMetadataChanged) || errors.Is(err, storage.ErrNotFound) {
		return h.recoverRefreshedNPMVersion(ctx, packumentURL, cacheKey, version, retain)
	}
	return data, err
}

func (h *NPMHandler) recoverRefreshedNPMVersion(ctx context.Context, packumentURL, cacheKey, version string, retain bool) (npmVersionData, error) {
	if retain {
		// Stay inside the non-cooldown limiter window: use the current cache
		// row instead of forcing another missing-version refresh.
		data, _, err := h.readVersionData(ctx, packumentURL, cacheKey, version)
		return data, err
	}
	// Under cooldown a replacement may be older and lack the publish time.
	// Fetch again, as for every other non-concurrent missing-version check.
	res := h.refreshMissingPackument(ctx, packumentURL, cacheKey, false)
	return h.readNPMVersionResult(ctx, res, version)
}

func (h *NPMHandler) readNPMVersionResult(ctx context.Context, res metadataResult, version string) (npmVersionData, error) {
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
