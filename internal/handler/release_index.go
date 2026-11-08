package handler

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"net/http"
	"strings"
	"time"
)

// APT and RPM repositories have a release file (InRelease, Release,
// repomd.xml) that lists the sha256 of each index file. Clients fetch the
// release file first and reject an index that doesn't match it. Each file
// has its own metadata cache entry and TTL, so without care a client could
// get a cached release file and a freshly fetched index, or the reverse.
//
// An index the cached release file lists is therefore served by that
// listing rather than by its own TTL: the cached copy when it matches,
// whatever its age, and otherwise a copy fetched from upstream. If upstream
// only has an index the cached release file doesn't list, upstream has moved
// on; that index is served and the release file is expired, so the next
// update gets a matching pair.

// cachedRelease is a release file in the metadata cache.
type cachedRelease struct {
	ecosystem string
	cacheKey  string
	body      []byte
	fetchedAt time.Time
}

// readCachedRelease returns the cached release file stored under cacheKey,
// or nil.
func (p *Proxy) readCachedRelease(ctx context.Context, ecosystem, cacheKey string) *cachedRelease {
	if !p.CacheMetadata || p.DB == nil || p.Storage == nil {
		return nil
	}
	entry, err := p.DB.GetMetadataCache(ecosystem, cacheKey)
	if err != nil || entry == nil || entry.ContentEncoding.String != "" {
		return nil
	}
	body, _, err := p.readCachedMetadata(ctx, entry, nil)
	// Bytes another instance replaced since the row was read don't describe
	// the release the row records.
	if err != nil || !matchesEntryDigest(entry, body) {
		return nil
	}
	return &cachedRelease{ecosystem: ecosystem, cacheKey: cacheKey, body: body, fetchedAt: entry.FetchedAt.Time}
}

// proxyListedIndex serves an index file that release lists with the sha256
// hex want. Without a listing it is served as other metadata.
func (p *Proxy) proxyListedIndex(w http.ResponseWriter, r *http.Request, upstreamURL, ecosystem, cacheKey string, release *cachedRelease, want string) {
	if release == nil || want == "" {
		p.ProxyCached(w, r, upstreamURL, ecosystem, cacheKey, "*/*")
		return
	}
	if entry, _ := p.DB.GetMetadataCache(ecosystem, cacheKey); entry != nil && p.streamableMetadata(entry) &&
		entry.ContentDigest.String == "sha256:"+want && entry.ContentEncoding.String == "" {
		if p.serveMetadataEntry(w, r, ecosystem, cacheKey, entry, true) {
			return
		}
	}
	p.recordMetadataCacheMiss(ecosystem)
	res := p.refreshMetadata(r.Context(), ecosystem, cacheKey, upstreamURL, "*/*", "identity", nil)
	if res.err != nil {
		p.writeMetadataFetchError(w, res.err)
		return
	}
	if sum := sha256.Sum256(res.body); !res.stale && hex.EncodeToString(sum[:]) != want {
		p.Logger.Info("index does not match the cached release file, expiring it",
			"ecosystem", ecosystem, "url", upstreamURL)
		if _, err := p.DB.ExpireMetadataCache(ecosystem, []string{release.cacheKey}, time.Now()); err != nil {
			p.Logger.Warn("failed to expire cached release file", "ecosystem", ecosystem, "error", err)
		}
	}
	writeMetadataResult(w, r, res)
}

// releaseLineMax bounds a line of a Release file.
const releaseLineMax = 1 << 20

// debianReleaseSHA256 returns the sha256 an APT Release or InRelease file
// lists for rel, a path relative to the release file's directory, or "".
func debianReleaseSHA256(release []byte, rel string) string {
	inSection := false
	scanner := bufio.NewScanner(bytes.NewReader(release))
	scanner.Buffer(nil, releaseLineMax)
	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r")
		if !strings.HasPrefix(line, " ") {
			inSection = line == "SHA256:"
			continue
		}
		if !inSection {
			continue
		}
		if f := strings.Fields(line); len(f) == 3 && f[2] == rel {
			return strings.ToLower(f[0])
		}
	}
	return ""
}

// repomdSHA256 returns the sha256 an RPM repomd.xml lists for href, a path
// relative to the repository root, or "" when it lists none or another
// checksum type.
func repomdSHA256(repomd []byte, href string) string {
	var doc struct {
		Data []struct {
			Checksum struct {
				Type  string `xml:"type,attr"`
				Value string `xml:",chardata"`
			} `xml:"checksum"`
			Location struct {
				Href string `xml:"href,attr"`
			} `xml:"location"`
		} `xml:"data"`
	}
	if xml.Unmarshal(repomd, &doc) != nil {
		return ""
	}
	for _, d := range doc.Data {
		if d.Location.Href == href && d.Checksum.Type == "sha256" {
			return strings.ToLower(strings.TrimSpace(d.Checksum.Value))
		}
	}
	return ""
}
