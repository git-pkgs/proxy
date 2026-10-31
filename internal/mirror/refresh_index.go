package mirror

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/git-pkgs/proxy/internal/database"
	"github.com/git-pkgs/proxy/internal/handler"
	"github.com/git-pkgs/vers"
)

// A Debian, Alpine or RPM client finds new versions in a repository index
// rather than in metadata per package. A refresh reads the indexes clients
// fetched again, from the URLs the metadata cache recorded, and mirrors the
// newer files they list of the packages downloaded most.

// osIndex describes the repository index of one ecosystem.
type osIndex struct {
	// ecosystem is what package files are cached under, metaEcosystem what
	// the index documents are cached under.
	ecosystem, metaEcosystem string
	// scheme orders versions.
	scheme string
	// base returns the repository base package paths are relative to, for
	// the URL of an index document a refresh starts from, or false for
	// other documents.
	base func(indexURL string) (string, bool)
	// files lists the package files of the index document body fetched
	// from indexURL. cached are the files of the most used packages under
	// the repository base, for indexes that list their files in parts.
	files func(ctx context.Context, m *Mirror, indexURL string, body []byte, cached []cachedOSFile) ([]indexFile, error)
}

var osIndexes = map[string]osIndex{
	"alpine": {
		ecosystem: "alpine", metaEcosystem: "alpine", scheme: "alpine",
		base: func(u string) (string, bool) {
			return strings.CutSuffix(u, "/APKINDEX.tar.gz")
		},
		files: func(_ context.Context, _ *Mirror, _ string, body []byte, _ []cachedOSFile) ([]indexFile, error) {
			return parseAPKIndex(bytes.NewReader(body))
		},
	},
	"deb": {
		ecosystem: "deb", metaEcosystem: "debian", scheme: "deb",
		base: func(u string) (string, bool) {
			if !strings.HasSuffix(u, "/InRelease") && !strings.HasSuffix(u, "/Release") {
				return "", false
			}
			base, _, ok := strings.Cut(u, "/dists/")
			return base, ok
		},
		files: debIndexFiles,
	},
	"rpm": {
		ecosystem: "rpm", metaEcosystem: "rpm", scheme: "rpm",
		base: func(u string) (string, bool) {
			return strings.CutSuffix(u, "/repodata/repomd.xml")
		},
		files: rpmIndexFiles,
	},
}

// cachedOSFile is a cached file of a most used package, by its path below a
// repository base.
type cachedOSFile struct {
	file                database.UsedFile
	rel                 string
	name, version, arch string
}

func (f cachedOSFile) key() string { return f.name + "\x00" + f.arch }

// indexRefreshItems returns the newer files the recently fetched indexes of
// an ecosystem list for packages, and an error per index it couldn't read.
func (m *Mirror) indexRefreshItems(ctx context.Context, ix osIndex, packages []database.UsedPackage, cutoff time.Time) ([]PackageVersion, []error) {
	docs, err := m.db.RecentMetadataDocuments(ix.metaEcosystem, cutoff)
	if err != nil {
		return nil, []error{fmt.Errorf("listing %s indexes: %w", ix.metaEcosystem, err)}
	}
	var items []PackageVersion
	var errs []error
	seenIndex := map[string]bool{}
	seenFile := map[string]bool{}
	for _, doc := range docs {
		base, ok := ix.base(doc.UpstreamURL)
		if !ok || seenIndex[urlDir(doc.UpstreamURL)] {
			continue
		}
		cached := cachedUnder(ix.ecosystem, base, packages)
		if len(cached) == 0 {
			continue
		}
		seenIndex[urlDir(doc.UpstreamURL)] = true
		body, _, err := m.proxy.FetchOrCacheMetadata(ctx, ix.metaEcosystem, doc.Name, doc.UpstreamURL, "*/*")
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", doc.UpstreamURL, err))
			continue
		}
		files, err := ix.files(ctx, m, doc.UpstreamURL, body, cached)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", doc.UpstreamURL, err))
			continue
		}
		for _, item := range m.newerIndexFiles(ix, base, cached, files) {
			if !seenFile[item.fileURL] {
				seenFile[item.fileURL] = true
				items = append(items, item)
			}
		}
	}
	return items, errs
}

// cachedUnder returns the cached files of packages below base.
func cachedUnder(ecosystem, base string, packages []database.UsedPackage) []cachedOSFile {
	var cached []cachedOSFile
	for _, pkg := range packages {
		for _, f := range pkg.Files {
			rel, ok := strings.CutPrefix(f.UpstreamURL, base+"/")
			if !ok {
				continue
			}
			name, version, arch := handler.OSPackageIdentity(ecosystem, rel)
			if name != "" {
				cached = append(cached, cachedOSFile{file: f, rel: rel, name: name, version: version, arch: arch})
			}
		}
	}
	return cached
}

// newerIndexFiles returns, per package and architecture cached below base,
// the newest file the index lists when it is newer than every cached one.
func (m *Mirror) newerIndexFiles(ix osIndex, base string, cached []cachedOSFile, files []indexFile) []PackageVersion {
	newestCached := map[string]cachedOSFile{}
	for _, c := range cached {
		if current, ok := newestCached[c.key()]; !ok || vers.CompareWithScheme(c.version, current.version, ix.scheme) > 0 {
			newestCached[c.key()] = c
		}
	}
	newest := map[string]PackageVersion{}
	var order []string
	for _, f := range files {
		name, version, arch := handler.OSPackageIdentity(ix.ecosystem, f.Path)
		key := name + "\x00" + arch
		c, ok := newestCached[key]
		if name == "" || !ok || vers.CompareWithScheme(version, c.version, ix.scheme) <= 0 {
			continue
		}
		if m.proxy != nil && !m.proxy.ReleaseAllowed(ix.ecosystem, name, version, time.Time{}) {
			continue
		}
		if current, seen := newest[key]; seen && vers.CompareWithScheme(version, current.Version, ix.scheme) <= 0 {
			continue
		} else if !seen {
			order = append(order, key)
		}
		cacheFilename := path.Base(f.Path)
		if dir := path.Dir(c.file.Filename); dir != "." {
			cacheFilename = dir + "/" + cacheFilename
		}
		newest[key] = PackageVersion{
			Ecosystem: ix.ecosystem, Name: name, Version: version,
			fileURL: base + "/" + f.Path, cacheFilename: cacheFilename, digest: f.Digest,
		}
	}
	items := make([]PackageVersion, 0, len(order))
	for _, key := range order {
		items = append(items, newest[key])
	}
	return items
}

// debIndexFiles reads the Packages indexes an InRelease or Release file
// lists for the components and architectures of the cached files.
func debIndexFiles(ctx context.Context, m *Mirror, indexURL string, body []byte, cached []cachedOSFile) ([]indexFile, error) {
	indexes := debPackagesIndexes(body)
	wanted := map[string]bool{}
	for _, c := range cached {
		rest, ok := strings.CutPrefix(c.rel, "pool/")
		if !ok {
			continue
		}
		component, _, _ := strings.Cut(rest, "/")
		if dir := component + "/binary-" + c.arch; indexes[dir] != "" {
			wanted[dir] = true
			continue
		}
		// Files for every architecture are listed in each binary-* index.
		for dir := range indexes {
			if strings.HasPrefix(dir, component+"/binary-") {
				wanted[dir] = true
				break
			}
		}
	}
	suite := urlDir(indexURL)
	var files []indexFile
	for dir := range wanted {
		listed, err := m.readIndex(ctx, suite+"/"+indexes[dir], parseDebPackages)
		if err != nil {
			return nil, err
		}
		files = append(files, listed...)
	}
	return files, nil
}

// rpmIndexFiles reads the primary metadata a repomd.xml points at.
func rpmIndexFiles(ctx context.Context, m *Mirror, indexURL string, body []byte, _ []cachedOSFile) ([]indexFile, error) {
	primary, err := repomdPrimary(body)
	if err != nil {
		return nil, err
	}
	base := strings.TrimSuffix(indexURL, "/repodata/repomd.xml")
	return m.readIndex(ctx, base+"/"+primary, parseRPMPrimary)
}

// readIndex streams an index from upstream through parse, decompressed as
// its name says. It is read directly rather than through the metadata
// cache, since it can be far larger than a metadata document may be.
func (m *Mirror) readIndex(ctx context.Context, indexURL string, parse func(io.Reader) ([]indexFile, error)) ([]indexFile, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, indexURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := m.proxy.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: upstream returned %d", indexURL, resp.StatusCode)
	}
	r, err := decompressByName(indexURL, resp.Body)
	if err != nil {
		return nil, err
	}
	defer func() { _ = r.Close() }()
	return parse(r)
}

// urlDir returns a URL without its last path segment. path.Dir would fold
// the slashes after the scheme.
func urlDir(u string) string {
	return u[:strings.LastIndex(u, "/")]
}
