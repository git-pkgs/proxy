package gitcache

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const (
	advertisementType = "application/x-git-upload-pack-advertisement"
	requestType       = "application/x-git-upload-pack-request"
	resultType        = "application/x-git-upload-pack-result"
	// Some servers only speak the smart protocol to clients that look like git.
	gitUserAgent = "git/2 (git-pkgs-proxy)"
	// maxRefsResponse bounds a ref listing; branches and tags of even large
	// repositories take a few megabytes.
	maxRefsResponse = 64 << 20

	headsPrefix = "refs/heads/"
	tagsPrefix  = "refs/tags/"
)

var (
	// ErrNotFound is returned when the upstream has no such repository.
	ErrNotFound = errors.New("repository not found upstream")
	// ErrRefused covers authentication failures and redirects. The cached
	// copy is not served after one, since the upstream may have withdrawn
	// access to it.
	ErrRefused = errors.New("upstream refused the request")
	// ErrTooLarge is returned for a repository larger than git.max_repo_size,
	// or than git.max_size where that is smaller.
	ErrTooLarge = errors.New("repository is larger than git.max_repo_size or git.max_size")
	// ErrCacheFull is returned for a fetch cut off because repositories in
	// use take what git.max_size leaves.
	ErrCacheFull = errors.New("git cache is full")
)

// remoteRefs is what a repository advertises: its branch and tag tips, and
// the branch HEAD points at.
type remoteRefs struct {
	refs map[string]string
	head string
}

// listUpstreamRefs asks the upstream for its branches, tags and HEAD. With
// protocol v2 the server filters by prefix, which matters for Gerrit, where
// every patch set is a ref. Servers without v2 send every ref, and the rest
// are dropped here.
func (c *Cache) listUpstreamRefs(ctx context.Context, repoURL string) (remoteRefs, error) {
	resp, err := c.upstreamRequest(ctx, http.MethodGet, repoURL+"/info/refs?service=git-upload-pack", "")
	if err != nil {
		return remoteRefs{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, advertisementType) {
		return remoteRefs{}, fmt.Errorf("upstream did not answer as a smart HTTP Git server (content type %q)", ct)
	}

	pkts := newPktReader(io.LimitReader(resp.Body, maxRefsResponse))
	kind, line, err := pkts.next()
	if err == nil && kind == pktData && strings.HasPrefix(line, "# service=") {
		if kind, _, err = pkts.next(); err == nil && kind != pktFlushKind {
			err = errors.New("no flush after the service line")
		}
		if err == nil {
			kind, line, err = pkts.next()
		}
	}
	if err != nil {
		return remoteRefs{}, fmt.Errorf("reading ref advertisement: %w", err)
	}

	if kind == pktData && line == "version 2" {
		return c.lsRefs(ctx, repoURL)
	}
	if kind == pktData && line == "version 1" {
		if kind, line, err = pkts.next(); err != nil {
			return remoteRefs{}, fmt.Errorf("reading ref advertisement: %w", err)
		}
	}
	return parseV0Advertisement(pkts, kind, line)
}

// lsRefs runs the protocol v2 ls-refs command.
func (c *Cache) lsRefs(ctx context.Context, repoURL string) (remoteRefs, error) {
	body := pktLine("command=ls-refs\n") + pktDelim +
		pktLine("symrefs\n") +
		pktLine("ref-prefix HEAD\n") +
		pktLine("ref-prefix "+headsPrefix+"\n") +
		pktLine("ref-prefix "+tagsPrefix+"\n") +
		pktFlush
	resp, err := c.upstreamRequest(ctx, http.MethodPost, repoURL+"/git-upload-pack", body)
	if err != nil {
		return remoteRefs{}, err
	}
	defer func() { _ = resp.Body.Close() }()

	remote := remoteRefs{refs: make(map[string]string)}
	pkts := newPktReader(io.LimitReader(resp.Body, maxRefsResponse))
	for {
		kind, line, err := pkts.next()
		if err != nil {
			return remoteRefs{}, fmt.Errorf("reading ls-refs response: %w", err)
		}
		if kind == pktFlushKind {
			return remote, nil
		}
		if kind != pktData {
			continue
		}
		fields := strings.Split(line, " ")
		if len(fields) < 2 { //nolint:mnd // object id and ref name
			return remoteRefs{}, fmt.Errorf("invalid ls-refs line %q", line)
		}
		if fields[1] == "HEAD" {
			for _, attr := range fields[2:] {
				if target, ok := strings.CutPrefix(attr, "symref-target:"); ok {
					remote.head = target
				}
			}
			continue
		}
		remote.add(fields[1], fields[0])
	}
}

// parseV0Advertisement reads a protocol v0 ref advertisement whose first
// packet has already been read. The first ref carries the capabilities,
// including where HEAD points.
func parseV0Advertisement(pkts *pktReader, kind pktKind, line string) (remoteRefs, error) {
	remote := remoteRefs{refs: make(map[string]string)}
	first := true
	for kind != pktFlushKind {
		if kind == pktData {
			ref, caps, _ := strings.Cut(line, "\x00")
			if first {
				remote.head = symrefTarget(caps, "HEAD")
				first = false
			}
			oid, name, ok := strings.Cut(ref, " ")
			if !ok {
				return remoteRefs{}, fmt.Errorf("invalid ref advertisement line %q", ref)
			}
			remote.add(name, oid)
		}
		var err error
		if kind, line, err = pkts.next(); err != nil {
			return remoteRefs{}, fmt.Errorf("reading ref advertisement: %w", err)
		}
	}
	return remote, nil
}

// add keeps branches and tags, leaving out peeled tag entries.
func (r remoteRefs) add(name, oid string) {
	if (strings.HasPrefix(name, headsPrefix) || strings.HasPrefix(name, tagsPrefix)) && !strings.HasSuffix(name, "^{}") {
		r.refs[name] = oid
	}
}

func symrefTarget(caps, ref string) string {
	for _, capability := range strings.Fields(caps) {
		if value, ok := strings.CutPrefix(capability, "symref="+ref+":"); ok {
			return value
		}
	}
	return ""
}

// upstreamRequest sends a smart HTTP request through the upstream client and
// classifies refusals. A non-empty body is sent as an upload-pack request.
func (c *Cache) upstreamRequest(ctx context.Context, method, target, body string) (*http.Response, error) {
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Git-Protocol", "version=2")
	req.Header.Set("User-Agent", gitUserAgent)
	if body != "" {
		req.Header.Set("Content-Type", requestType)
		req.Header.Set("Accept", resultType)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("upstream request: %w", err)
	}
	if err := upstreamStatusError(resp.StatusCode); err != nil {
		_ = resp.Body.Close()
		return nil, err
	}
	return resp, nil
}

func upstreamStatusError(status int) error {
	switch {
	case status == http.StatusOK:
		return nil
	case status == http.StatusNotFound || status == http.StatusGone:
		return fmt.Errorf("%w: status %d", ErrNotFound, status)
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return fmt.Errorf("%w: status %d", ErrRefused, status)
	case status >= http.StatusMultipleChoices && status < http.StatusBadRequest:
		return fmt.Errorf("%w: redirect (status %d) not followed", ErrRefused, status)
	default:
		return fmt.Errorf("upstream returned status %d", status)
	}
}
