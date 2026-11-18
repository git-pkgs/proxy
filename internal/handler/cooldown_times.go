package handler

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"time"
)

// publishTimes maps each version a times document lists to its publish
// time. A version whose time didn't parse maps to the zero time and is let
// through.
type publishTimes map[string]time.Time

// lookupPublishTime reads version's publish time for a download. A
// document clients read, cached within metadata_ttl or served stale, that
// doesn't list the version can predate it, so both documents are fetched
// again, as readListed does. One that lists it brought the times document up
// to date already (fetchTimedMetadata).
func (p *Proxy) lookupPublishTime(ctx context.Context, t timedMetadata, version string, listed func(metadataResult) ([]string, error)) (time.Time, bool, bool, error) {
	published, ok, withholdUnknown, docs, err := p.readPublishTime(ctx, t, version, listed, false)
	if err != nil || ok || withholdUnknown || slices.Contains(docs.versions, version) || !docs.doc.fromCache && !docs.doc.stale {
		return published, ok, withholdUnknown, err
	}
	published, ok, withholdUnknown, _, err = p.readPublishTime(ctx, t, version, listed, true)
	return published, ok, withholdUnknown, err
}

func (p *Proxy) readPublishTime(ctx context.Context, t timedMetadata, version string, listed func(metadataResult) ([]string, error), refetch bool) (time.Time, bool, bool, timedDocuments, error) {
	var versions []string
	docs, err := p.fetchTimedMetadataFor(ctx, t, func(doc metadataResult) ([]string, error) {
		var err error
		versions, err = listed(doc)
		return versions, err
	}, refetch)
	docs.versions = versions
	switch {
	case docs.doc.err != nil:
		return time.Time{}, false, false, docs, docs.doc.err
	case docs.times.err != nil:
		return time.Time{}, false, false, docs, docs.times.err
	case err != nil:
		return time.Time{}, false, false, docs, err
	}
	published, ok := docs.published[version]
	withholdUnknown := !ok && docs.withholdUnknown && slices.Contains(versions, version)
	return published, ok && !published.IsZero(), withholdUnknown, docs, nil
}

// timedMetadata describes metadata whose publish times live in a second
// document, as with the RubyGems compact index and the versions API, or a
// Hex package and the Hex HTTP API. Both are read through the metadata cache.
type timedMetadata struct {
	ecosystem string
	// docKey and docURL are the cache key and upstream URL of the document
	// clients read. They match the plain metadata path's, so both share one
	// cached copy.
	docKey, docURL string
	// timesKey and timesURL are those of the document with publish times.
	timesKey, timesURL string
	timesAccept        string
	parseTimes         func([]byte) (publishTimes, error)
}

// timedDocuments is what fetchTimedMetadata hands back. published is read
// from times and is nil when times.err is set.
type timedDocuments struct {
	doc       metadataResult
	times     metadataResult
	published publishTimes
	// withholdUnknown is set when doc lists versions the times document
	// doesn't, and that document is a stale copy: those versions are newer
	// than the copy, so they are left out rather than let through.
	withholdUnknown bool
	// versions are the versions doc lists, as readPublishTime read them.
	versions []string
}

// fetchTimedMetadata fetches both documents concurrently. The document
// clients read is fetched as the plain metadata path fetches it, with
// identity encoding and Accept */*.
//
// Each document has its own TTL, so a cached times document can be older
// than the document it is used to filter, and miss versions published in
// between. Those versions would be let through as having no publish time.
// listed returns the versions the document lists; when some are missing
// from a times document read from the cache, the times document is fetched
// again before filtering.
func (p *Proxy) fetchTimedMetadata(ctx context.Context, t timedMetadata, listed func(metadataResult) ([]string, error)) (timedDocuments, error) {
	return p.fetchTimedMetadataFor(ctx, t, listed, false)
}

// fetchTimedMetadataFor is fetchTimedMetadata, fetching both documents again
// from upstream when refetch is set (refetchLacking).
func (p *Proxy) fetchTimedMetadataFor(ctx context.Context, t timedMetadata, listed func(metadataResult) ([]string, error), refetch bool) (timedDocuments, error) {
	var res timedDocuments
	var published publishTimes
	validate := func(body []byte) error {
		parsed, err := t.parseTimes(body)
		if err == nil {
			published = parsed
		}
		return err
	}

	done := make(chan struct{})
	lookup := p.lookupMetadata
	if refetch {
		lookup = p.refetchLacking
	}
	go func() {
		defer close(done)
		res.times = lookup(ctx, t.ecosystem, t.timesKey, t.timesURL, t.timesAccept, "", validate)
	}()
	res.doc = lookup(ctx, t.ecosystem, t.docKey, t.docURL, "*/*", "identity", nil)
	<-done

	if res.doc.err != nil || res.times.err != nil {
		return res, nil
	}
	versions, err := listed(res.doc)
	if err != nil {
		return res, err
	}
	unknown := func() bool {
		for _, v := range versions {
			if _, ok := published[v]; !ok {
				return true
			}
		}
		return false
	}
	if unknown() && res.times.fromCache {
		p.Logger.Info("cooldown: publish times older than metadata, fetching them again",
			"ecosystem", t.ecosystem, "key", t.timesKey)
		res.times = p.refreshMetadata(ctx, t.ecosystem, t.timesKey, t.timesURL, t.timesAccept, "", validate)
		if res.times.err != nil {
			return res, nil
		}
	}
	res.published = published
	res.withholdUnknown = res.times.stale && unknown()
	return res, nil
}

// rewriteDigest identifies the inputs of a cooldown rewrite for the rewrite
// cache: both documents and whether unknown versions are withheld.
func (d timedDocuments) rewriteDigest() string {
	h := sha256.New()
	for _, res := range []metadataResult{d.doc, d.times} {
		digest := res.digest
		if digest == "" {
			sum := sha256.Sum256(res.body)
			digest = "sha256:" + hex.EncodeToString(sum[:])
		}
		_, _ = fmt.Fprintf(h, "%d:%s", len(digest), digest)
	}
	_, _ = fmt.Fprintf(h, "%t", d.withholdUnknown)
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// withheld reports whether cooldown leaves version out, given the
// package's publish times, and records a version it leaves out.
func (d timedDocuments) withheld(p *Proxy, ecosystem, name, version string) bool {
	published, ok := d.published[version]
	if !ok {
		return d.withholdUnknown
	}
	if published.IsZero() {
		return false
	}
	if p.cooldown().IsAllowed(ecosystem, canonicalPackagePURL(ecosystem, name), published) {
		return false
	}
	p.recordWithheld(ecosystem, name, version, published)
	return true
}

// decodedMetadataBody returns res's body without its Content-Encoding. The
// documents are fetched with identity encoding, so an upstream that sends
// gzip anyway is the only case to handle.
func (p *Proxy) decodedMetadataBody(res metadataResult) ([]byte, error) {
	switch strings.ToLower(strings.TrimSpace(res.contentEncoding)) {
	case "", "identity":
		return res.body, nil
	case "gzip", "x-gzip":
		zr, err := gzip.NewReader(bytes.NewReader(res.body))
		if err != nil {
			return nil, err
		}
		defer func() { _ = zr.Close() }()
		return p.ReadMetadata(zr)
	default:
		return nil, fmt.Errorf("unsupported content encoding %q", res.contentEncoding)
	}
}
