package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// gitLFSPath separates a repository from the Git LFS API below it, as
	// git-lfs derives the API URL from a remote: {remote}.git/info/lfs.
	gitLFSPath      = "/info/lfs/"
	gitLFSMediaType = "application/vnd.git-lfs+json"
	gitLFSTransfer  = "basic"
	gitLFSHashAlgo  = "sha256"
	// gitLFSObjectMediaType is what objects are served as.
	gitLFSObjectMediaType = "application/octet-stream"

	// gitLFSMaxBatchRequest bounds a client's batch request. git-lfs sends
	// 100 objects per batch by default, a few kilobytes.
	gitLFSMaxBatchRequest = 1 << 20
	// gitLFSMaxBatchResponse bounds an upstream batch response, which holds
	// an href and headers per object.
	gitLFSMaxBatchResponse = 16 << 20

	gitLFSOIDLength = 64
)

// lfsBatchRequest is a Git LFS batch API request.
type lfsBatchRequest struct {
	Operation string          `json:"operation"`
	Transfers []string        `json:"transfers,omitempty"`
	Ref       json.RawMessage `json:"ref,omitempty"`
	Objects   []lfsObject     `json:"objects"`
	HashAlgo  string          `json:"hash_algo,omitempty"`
}

type lfsObject struct {
	OID  string `json:"oid"`
	Size int64  `json:"size"`
}

// lfsBatchResponse is a Git LFS batch API response.
type lfsBatchResponse struct {
	Transfer string              `json:"transfer,omitempty"`
	Objects  []lfsObjectResponse `json:"objects"`
	HashAlgo string              `json:"hash_algo,omitempty"`
}

type lfsObjectResponse struct {
	OID           string               `json:"oid"`
	Size          int64                `json:"size"`
	Authenticated bool                 `json:"authenticated,omitempty"`
	Actions       map[string]lfsAction `json:"actions,omitempty"`
	Error         *lfsObjectError      `json:"error,omitempty"`
}

type lfsAction struct {
	Href      string            `json:"href"`
	Header    map[string]string `json:"header,omitempty"`
	ExpiresIn int64             `json:"expires_in,omitempty"`
	ExpiresAt string            `json:"expires_at,omitempty"`
}

type lfsObjectError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// lfsUpstreamError is a batch request the upstream answered with an error
// status.
type lfsUpstreamError struct {
	status int
}

func (e *lfsUpstreamError) Error() string {
	return fmt.Sprintf("LFS batch API answered %d", e.status)
}

// WithBaseURL sets the URL clients reach the proxy at, which LFS batch
// responses send them back to for each object.
func (h *GitHandler) WithBaseURL(baseURL string) *GitHandler {
	h.baseURL = strings.TrimSuffix(baseURL, "/")
	return h
}

// serveLFS answers a request to the Git LFS API of a repository: batch
// downloads, and the objects the batch responses point at. Objects are
// stored in the artifact cache, checked against their sha256 object ID.
// Uploads and locking are refused.
func (h *GitHandler) serveLFS(w http.ResponseWriter, r *http.Request, repoPath, endpoint string) {
	repo, status, msg := h.repository(repoPath)
	if status != 0 {
		lfsError(w, status, msg)
		return
	}
	if endpoint == "objects/batch" {
		if r.Method != http.MethodPost {
			lfsError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		h.serveLFSBatch(w, r, repo)
		return
	}
	oid, sizeText, ok := strings.Cut(strings.TrimPrefix(endpoint, "objects/"), "/")
	size, err := strconv.ParseInt(sizeText, 10, 64)
	if !strings.HasPrefix(endpoint, "objects/") || !ok || err != nil || size < 0 || !validLFSOID(oid) {
		lfsError(w, http.StatusNotFound, "not found")
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		lfsError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	h.serveLFSObject(w, r, repo, lfsObject{OID: oid, Size: size})
}

// serveLFSBatch forwards a download batch upstream and points every object
// the upstream offers at this proxy. The upstream answers for the
// repository, so a client only gets objects it could get from there.
func (h *GitHandler) serveLFSBatch(w http.ResponseWriter, r *http.Request, repo gitRepository) {
	var req lfsBatchRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, gitLFSMaxBatchRequest)).Decode(&req); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			lfsError(w, http.StatusRequestEntityTooLarge, "batch request too large")
			return
		}
		lfsError(w, http.StatusBadRequest, "invalid batch request")
		return
	}
	switch {
	case req.Operation != "download":
		lfsError(w, http.StatusForbidden, "pushing is not supported")
		return
	case req.HashAlgo != "" && req.HashAlgo != gitLFSHashAlgo:
		lfsError(w, http.StatusConflict, "only sha256 objects are supported")
		return
	case len(req.Transfers) > 0 && !slices.Contains(req.Transfers, gitLFSTransfer):
		lfsError(w, http.StatusUnprocessableEntity, "only the basic transfer is supported")
		return
	}
	requested := make(map[string]int64, len(req.Objects))
	for _, object := range req.Objects {
		if !validLFSOID(object.OID) || object.Size < 0 {
			lfsError(w, http.StatusUnprocessableEntity, "invalid object")
			return
		}
		requested[object.OID] = object.Size
	}

	if repo.upstream.Offline {
		writeLFSBatch(w, h.lfsCachedBatch(r, repo, req.Objects, lfsObjectError{
			Code: http.StatusNotFound, Message: lfsNotCachedOffline}))
		return
	}
	upstream, err := h.lfsBatch(r.Context(), repo, req.Ref, req.Objects)
	if err != nil {
		if resp, ok := h.lfsBatchFromCache(r, repo, req.Objects, err); ok {
			writeLFSBatch(w, resp)
			return
		}
		h.serveLFSUpstreamError(w, r, repo, err)
		return
	}
	resp := newLFSBatchResponse()
	for _, object := range upstream.Objects {
		// Only objects the client asked for, at the size it gave, so a
		// response can't point the client at anything else.
		size, ok := requested[object.OID]
		if !ok || size != object.Size {
			continue
		}
		out := lfsObjectResponse{OID: object.OID, Size: object.Size, Error: object.Error}
		if download, ok := object.Actions["download"]; ok && object.Error == nil && download.Href != "" {
			out.Authenticated = true
			out.Actions = map[string]lfsAction{"download": {Href: h.lfsObjectURL(repo, object.OID, object.Size)}}
			// Kept so the client's GET for the object needs no second
			// batch request.
			h.lfsDownloads.put(repo.key, lfsObject{OID: object.OID, Size: object.Size}, download, time.Now())
		}
		resp.Objects = append(resp.Objects, out)
	}
	writeLFSBatch(w, resp)
}

// lfsBatchFromCache answers a batch the upstream couldn't, because it was
// unreachable or failing, from the objects cached for repo. This is what a
// GET of each object would serve anyway. A refusal by the upstream is not
// overridden, and a batch none of whose objects are cached is left to fail.
func (h *GitHandler) lfsBatchFromCache(r *http.Request, repo gitRepository, objects []lfsObject, err error) (lfsBatchResponse, bool) {
	var refused *lfsUpstreamError
	if r.Context().Err() != nil || (errors.As(err, &refused) && refused.status < http.StatusInternalServerError) {
		return lfsBatchResponse{}, false
	}
	resp := h.lfsCachedBatch(r, repo, objects, lfsObjectError{
		Code: http.StatusBadGateway, Message: "upstream unavailable and object not cached"})
	cached := 0
	for _, object := range resp.Objects {
		if object.Error == nil {
			cached++
		}
	}
	if cached == 0 {
		return lfsBatchResponse{}, false
	}
	h.proxy.Logger.Warn("git: LFS batch request failed, answering from the cache",
		"repository", repo.key, "cached", cached, "objects", len(resp.Objects), "error", err)
	return resp, true
}

// lfsNotCachedOffline answers for an object not cached on an offline
// upstream.
const lfsNotCachedOffline = "object not cached, and the upstream is offline"

// lfsCachedBatch answers a batch from the objects cached for repo, each
// object not cached with missing.
func (h *GitHandler) lfsCachedBatch(r *http.Request, repo gitRepository, objects []lfsObject, missing lfsObjectError) lfsBatchResponse {
	resp := newLFSBatchResponse()
	seen := make(map[string]bool, len(objects))
	for _, object := range objects {
		if seen[object.OID] {
			continue
		}
		seen[object.OID] = true
		out := lfsObjectResponse{OID: object.OID, Size: object.Size}
		if ok, _ := h.proxy.HasCachedArtifact(r.Context(), gitEcosystem, repo.lfsName(), object.OID, object.OID); ok {
			out.Authenticated = true
			out.Actions = map[string]lfsAction{"download": {Href: h.lfsObjectURL(repo, object.OID, object.Size)}}
		} else {
			out.Error = &lfsObjectError{Code: missing.Code, Message: missing.Message}
		}
		resp.Objects = append(resp.Objects, out)
	}
	return resp
}

func newLFSBatchResponse() lfsBatchResponse {
	return lfsBatchResponse{Transfer: gitLFSTransfer, Objects: []lfsObjectResponse{}, HashAlgo: gitLFSHashAlgo}
}

func writeLFSBatch(w http.ResponseWriter, resp lfsBatchResponse) {
	w.Header().Set("Content-Type", gitLFSMediaType)
	_ = json.NewEncoder(w).Encode(resp)
}

// serveLFSObject serves an object from the artifact cache. On a miss it
// takes the download URL from the client's batch, or asks the upstream batch
// API for a fresh one, and caches the object from there, or from a copy
// already stored under another repository, once its content matches its
// object ID.
func (h *GitHandler) serveLFSObject(w http.ResponseWriter, r *http.Request, repo gitRepository, object lfsObject) {
	cached, err := h.proxy.GetCachedArtifact(r.Context(), gitEcosystem, repo.lfsName(), object.OID, object.OID)
	if err != nil {
		h.proxy.serveArtifactError(w, err, "failed to read cached LFS object")
		return
	}
	if cached == nil && repo.upstream.Offline {
		lfsError(w, http.StatusNotFound, lfsNotCachedOffline)
		return
	}
	if cached == nil {
		var ok bool
		if cached, ok = h.fetchLFSObject(w, r, repo, object); !ok {
			return
		}
	}
	serveGenericArtifact(w, r, cached)
}

// fetchLFSObject caches an object the cache doesn't have. When it can't, or
// the request is a HEAD, it answers the request and returns false.
func (h *GitHandler) fetchLFSObject(w http.ResponseWriter, r *http.Request, repo gitRepository, object lfsObject) (*CacheResult, bool) {
	download, kept := h.lfsDownloads.get(repo.key, object, time.Now())
	if !kept {
		var ok bool
		if download, ok = h.lfsFreshDownload(w, r, repo, object); !ok {
			return nil, false
		}
	}
	if r.Method == http.MethodHead {
		// The upstream has the object, and a HEAD needs no more than that:
		// fetching it is left to a GET.
		w.Header().Set("Content-Type", gitLFSObjectMediaType)
		w.Header().Set("Content-Length", strconv.FormatInt(object.Size, 10))
		return nil, false
	}
	h.lfsDownloads.drop(repo.key, object.OID)
	cached, err := h.cacheLFSObject(r.Context(), repo, object, download)
	if err != nil && kept && retryLFSDownload(r.Context(), err) {
		// The kept URL may have expired early or been revoked.
		var ok bool
		if download, ok = h.lfsFreshDownload(w, r, repo, object); !ok {
			return nil, false
		}
		cached, err = h.cacheLFSObject(r.Context(), repo, object, download)
	}
	if err != nil {
		h.proxy.serveArtifactError(w, err, "failed to fetch LFS object")
		return nil, false
	}
	return cached, true
}

// lfsFreshDownload asks the upstream batch API for object's download. When
// there is none it answers the request and returns false.
func (h *GitHandler) lfsFreshDownload(w http.ResponseWriter, r *http.Request, repo gitRepository, object lfsObject) (lfsAction, bool) {
	upstream, err := h.lfsBatch(r.Context(), repo, nil, []lfsObject{object})
	if err != nil {
		h.serveLFSUpstreamError(w, r, repo, err)
		return lfsAction{}, false
	}
	download, status, msg := lfsDownload(upstream, object)
	if status != 0 {
		lfsError(w, status, msg)
		return lfsAction{}, false
	}
	return download, true
}

// cacheLFSObject caches object, which the upstream offers as download. A
// copy already stored under another name, such as by a fork of the
// repository, saves the download.
func (h *GitHandler) cacheLFSObject(ctx context.Context, repo gitRepository, object lfsObject, download lfsAction) (*CacheResult, error) {
	header := make(http.Header, len(download.Header))
	for key, value := range download.Header {
		header.Set(key, value)
	}
	// Download URLs are often signed, granting access to the object until
	// they expire, so only the URL without its query is logged and
	// recorded. The object must have the size the client asked for.
	ctx, recorded := withSignedDownload(ctx, download.Href, object.Size)
	if !h.proxy.StreamArtifacts {
		stored, err := h.proxy.DB.FindStoredContent(object.OID)
		if err != nil {
			h.proxy.Logger.Warn("git: looking up a stored copy of an LFS object failed", "oid", object.OID, "error", err)
		}
		if stored != nil && stored.Size == object.Size {
			cached, err := h.proxy.cacheStoredCopy(ctx, gitEcosystem, repo.lfsName(), object.OID, object.OID,
				recorded, object.OID, stored)
			if err == nil {
				return cached, nil
			}
			h.proxy.Logger.Warn("git: copying a stored LFS object failed, downloading it", "oid", object.OID, "error", err)
		}
	}
	return h.proxy.getOrFetchArtifactFromURL(ctx, gitEcosystem, repo.lfsName(), object.OID, object.OID,
		recorded, header, object.OID, "")
}

// retryLFSDownload reports whether a download from a kept URL failed in a way
// a fresh URL might not.
func retryLFSDownload(ctx context.Context, err error) bool {
	return ctx.Err() == nil &&
		!errors.Is(err, ErrArtifactDigestMismatch) &&
		!errors.Is(err, errDownloadSize) &&
		!errors.Is(err, ErrVersionDenied) &&
		!errors.Is(err, ErrArtifactBlocked)
}

// lfsDownload returns the download action the upstream gave for object, or
// the status and message to answer with when it gave none.
func lfsDownload(resp *lfsBatchResponse, object lfsObject) (lfsAction, int, string) {
	for _, got := range resp.Objects {
		if got.OID != object.OID {
			continue
		}
		if got.Error != nil {
			if got.Error.Code == http.StatusNotFound || got.Error.Code == http.StatusGone {
				return lfsAction{}, http.StatusNotFound, "object not found"
			}
			return lfsAction{}, http.StatusBadGateway, "upstream refused the object"
		}
		download, ok := got.Actions["download"]
		if !ok || !validLFSHref(download.Href) {
			return lfsAction{}, http.StatusBadGateway, "upstream gave no download for the object"
		}
		return download, 0, ""
	}
	return lfsAction{}, http.StatusNotFound, "object not found"
}

// validLFSHref reports whether href is an absolute HTTP(S) URL.
func validLFSHref(href string) bool {
	u, err := url.Parse(href)
	return err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != ""
}

// lfsBatch asks the upstream's LFS batch API for download actions for
// objects. upstream.auth applies to it as to the repository.
func (h *GitHandler) lfsBatch(ctx context.Context, repo gitRepository, ref json.RawMessage, objects []lfsObject) (*lfsBatchResponse, error) {
	body, err := json.Marshal(lfsBatchRequest{
		Operation: "download",
		Transfers: []string{gitLFSTransfer},
		Ref:       ref,
		Objects:   objects,
		HashAlgo:  gitLFSHashAlgo,
	})
	if err != nil {
		return nil, err
	}
	batchURL := strings.TrimSuffix(repo.url, ".git") + ".git" + gitLFSPath + "objects/batch"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, batchURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", gitLFSMediaType)
	req.Header.Set("Content-Type", gitLFSMediaType)
	resp, err := h.proxy.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, &lfsUpstreamError{status: resp.StatusCode}
	}
	var batch lfsBatchResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, gitLFSMaxBatchResponse)).Decode(&batch); err != nil {
		return nil, fmt.Errorf("reading LFS batch response: %w", err)
	}
	if batch.Transfer != "" && batch.Transfer != gitLFSTransfer {
		return nil, fmt.Errorf("LFS batch response uses the %q transfer", batch.Transfer)
	}
	return &batch, nil
}

// serveLFSUpstreamError answers a request whose upstream batch request
// failed.
func (h *GitHandler) serveLFSUpstreamError(w http.ResponseWriter, r *http.Request, repo gitRepository, err error) {
	if r.Context().Err() != nil {
		return
	}
	h.proxy.Logger.Warn("git: LFS batch request failed", "repository", repo.key, "error", err)
	var upstream *lfsUpstreamError
	switch {
	case errors.As(err, &upstream) && upstream.status == http.StatusNotFound:
		lfsError(w, http.StatusNotFound, "repository not found")
	case errors.As(err, &upstream) && upstream.status < http.StatusInternalServerError:
		lfsError(w, http.StatusBadGateway, "upstream refused the request")
	default:
		lfsError(w, http.StatusBadGateway, "upstream unavailable")
	}
}

// lfsName names repo's LFS objects in the artifact cache. Objects are cached
// per repository: having one cached for another repository says nothing
// about whether this one has it.
func (repo gitRepository) lfsName() string {
	return repo.name + "/" + repo.repo
}

// lfsObjectURL is where this proxy serves an object of repo.
func (h *GitHandler) lfsObjectURL(repo gitRepository, oid string, size int64) string {
	return fmt.Sprintf("%s/git/%s/%s.git%sobjects/%s/%d",
		h.baseURL, url.PathEscape(repo.name), repo.escapedRepo, gitLFSPath, oid, size)
}

// validLFSOID reports whether oid is a sha256 object ID in lowercase hex.
func validLFSOID(oid string) bool {
	if len(oid) != gitLFSOIDLength {
		return false
	}
	for _, c := range oid {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// lfsError answers with a Git LFS error document, which git-lfs shows.
func lfsError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", gitLFSMediaType)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"message": msg})
}

const (
	// gitLFSKeptDownloads bounds how many download URLs from batch responses
	// are kept for the GETs that follow them.
	gitLFSKeptDownloads = 4096
	// gitLFSKeptTTL is how long a download URL is kept when the upstream
	// gives no expiry, and gitLFSKeptMaxTTL how long at most.
	gitLFSKeptTTL    = 5 * time.Minute
	gitLFSKeptMaxTTL = time.Hour
	// gitLFSExpiryMargin is how long before its expiry a URL is no longer
	// used, so a download started with it can finish.
	gitLFSExpiryMargin = 30 * time.Second
)

// lfsDownloads keeps the download actions of recent batch responses, so the
// GET for an object a client just got in a batch needs no second batch
// request. They stay in memory, never reach a client and expire with the
// URLs.
type lfsDownloads struct {
	mu      sync.Mutex
	entries map[lfsDownloadKey]lfsKeptDownload
}

type lfsDownloadKey struct {
	repo, oid string
}

type lfsKeptDownload struct {
	size    int64
	action  lfsAction
	expires time.Time
}

// put keeps action for object of repo until shortly before it expires.
func (d *lfsDownloads) put(repo string, object lfsObject, action lfsAction, now time.Time) {
	if !validLFSHref(action.Href) {
		return
	}
	expires := now.Add(gitLFSKeptTTL)
	if action.ExpiresIn > 0 || action.ExpiresAt != "" {
		expires = now.Add(gitLFSKeptMaxTTL)
	}
	if action.ExpiresIn > 0 {
		expires = minTime(expires, now.Add(time.Duration(action.ExpiresIn)*time.Second))
	}
	if at, err := time.Parse(time.RFC3339, action.ExpiresAt); err == nil {
		expires = minTime(expires, at)
	}
	expires = expires.Add(-gitLFSExpiryMargin)
	if !expires.After(now) {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.entries == nil {
		d.entries = make(map[lfsDownloadKey]lfsKeptDownload)
	}
	if len(d.entries) >= gitLFSKeptDownloads {
		for key, kept := range d.entries {
			if !kept.expires.After(now) {
				delete(d.entries, key)
			}
		}
	}
	if len(d.entries) >= gitLFSKeptDownloads {
		// Still full: make room by dropping any one.
		for key := range d.entries {
			delete(d.entries, key)
			break
		}
	}
	d.entries[lfsDownloadKey{repo, object.OID}] = lfsKeptDownload{size: object.Size, action: action, expires: expires}
}

// get returns the action kept for object of repo, if it hasn't expired.
func (d *lfsDownloads) get(repo string, object lfsObject, now time.Time) (lfsAction, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	kept, ok := d.entries[lfsDownloadKey{repo, object.OID}]
	if !ok || kept.size != object.Size || !kept.expires.After(now) {
		return lfsAction{}, false
	}
	return kept.action, true
}

// drop forgets the action kept for oid of repo, once a GET has used it.
func (d *lfsDownloads) drop(repo, oid string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.entries, lfsDownloadKey{repo, oid})
}

func minTime(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}
