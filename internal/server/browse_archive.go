package server

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"

	"github.com/git-pkgs/archives"
	"github.com/git-pkgs/magic"
)

// Limits applied when listing or reading a single file. Bodies are streamed
// rather than retained, so these bound decompression work per request; they
// match the limits the buffered archive reader enforces.
const (
	maxBrowseExpandedSize = 512 << 20 // 512 MB across all entries
	maxBrowseEntrySize    = 512 << 20 // 512 MB for any one entry
	maxBrowseEntries      = 100_000
)

// npmPackagePrefix is the directory npm wraps every tarball's contents in.
const npmPackagePrefix = "package/"

var defaultBrowseLimits = archives.StreamOptions{
	MaxInputBytes:    maxBrowseArchiveSize,
	MaxEntryBytes:    maxBrowseEntrySize,
	MaxExpandedBytes: maxBrowseExpandedSize,
	MaxEntries:       maxBrowseEntries,
}

var (
	errArtifactTooLarge = errors.New("artifact too large for browsing")
	errBrowseNotFound   = errors.New("file not found")
	errBrowseIsDir      = errors.New("path is a directory")

	// errStopWalk ends a walk early without reporting an error.
	errStopWalk = errors.New("stop walk")
)

// isBrowseLimitError reports whether err comes from one of the browse size or
// entry-count limits rather than a malformed archive.
func isBrowseLimitError(err error) bool {
	return errors.Is(err, errArtifactTooLarge) ||
		errors.Is(err, archives.ErrInputLimit) ||
		errors.Is(err, archives.ErrEntrySizeLimit) ||
		errors.Is(err, archives.ErrDecompressLimit) ||
		errors.Is(err, archives.ErrEntryLimit)
}

// browseArchive reads a cached artifact sequentially for the browse endpoints
// without holding expanded file bodies in memory. TAR based formats stream from
// storage, reopening it when a second pass is needed. ZIP and conda need random
// access, so their input is read once and shared by every pass.
type browseArchive struct {
	filename  string
	ecosystem string
	limits    archives.StreamOptions
	reopen    func() (io.ReadCloser, error)

	// first is the caller's reader, used for the first pass only.
	first io.Reader
	// data holds buffered ZIP or conda input.
	data []byte
}

// newBrowseArchive prepares content for streaming. The caller keeps ownership
// of content; reopen supplies a fresh reader for any later pass.
func newBrowseArchive(filename, ecosystem string, content io.Reader, reopen func() (io.ReadCloser, error), limits archives.StreamOptions) (*browseArchive, error) {
	buffered := bufio.NewReaderSize(content, browseSniffSize)
	sniff, err := buffered.Peek(browseSniffSize)
	if err != nil && err != io.EOF {
		return nil, fmt.Errorf("reading artifact: %w", err)
	}

	a := &browseArchive{filename: filename, ecosystem: ecosystem, limits: limits, reopen: reopen}
	if !needsRandomAccess(filename, sniff) {
		a.first = buffered
		return a, nil
	}

	a.data, err = readBrowseInput(buffered, limits.MaxInputBytes)
	if err != nil {
		return nil, err
	}
	return a, nil
}

// readBrowseInput reads all of r, failing once it exceeds limit bytes.
func readBrowseInput(r io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, fmt.Errorf("reading artifact: %w", err)
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%w (%d bytes)", errArtifactTooLarge, len(data))
	}
	return data, nil
}

// needsRandomAccess reports whether the archives library buffers this format
// when streaming it, following the library's own detection order: a known
// extension first, then the content's leading bytes.
func needsRandomAccess(filename string, sniff []byte) bool {
	name := strings.ToLower(filename)
	for _, suffix := range []string{".tar.gz", ".tar.bz2", ".tar.xz", ".tar.zst"} {
		if strings.HasSuffix(name, suffix) {
			return false
		}
	}
	switch path.Ext(name) {
	case ".zip", ".jar", ".whl", ".nupkg", ".egg", ".vsix", ".conda":
		return true
	case ".tar", ".tgz", ".crate", ".gem":
		return false
	}
	return magic.DetectPrefix(sniff).Format == "zip"
}

// openStream starts a pass over the archive. The returned function releases
// the stream and any reader opened for it.
func (a *browseArchive) openStream() (*archives.Stream, func(), error) {
	if a.data != nil {
		stream, err := archives.OpenStreamBytes(a.filename, a.data, a.limits)
		if err != nil {
			return nil, nil, err
		}
		return stream, func() { _ = stream.Close() }, nil
	}

	if a.first != nil {
		content := a.first
		a.first = nil
		stream, err := archives.OpenStream(a.filename, content, a.limits)
		if err != nil {
			return nil, nil, err
		}
		return stream, func() { _ = stream.Close() }, nil
	}

	content, err := a.reopen()
	if err != nil {
		return nil, nil, fmt.Errorf("reopening artifact: %w", err)
	}
	stream, err := archives.OpenStream(a.filename, content, a.limits)
	if err != nil {
		_ = content.Close()
		return nil, nil, err
	}
	return stream, func() {
		_ = stream.Close()
		_ = content.Close()
	}, nil
}

// walk calls fn for each entry in archive order until fn returns an error or
// the archive ends. fn may read the entry body from the stream.
func (a *browseArchive) walk(fn func(entry *archives.StreamEntry) error) error {
	stream, release, err := a.openStream()
	if err != nil {
		return err
	}
	defer release()

	for {
		entry, err := stream.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if err := fn(entry); err != nil {
			if errors.Is(err, errStopWalk) {
				return nil
			}
			return err
		}
	}
}

// prefix returns the directory stripped from every path: npm's "package/", or
// a single top-level directory shared by all entries (like GitHub zipballs).
// For other ecosystems this costs one pass, which stops early once two
// top-level names differ.
func (a *browseArchive) prefix() (string, error) {
	if a.ecosystem == "npm" {
		return npmPackagePrefix, nil
	}

	var root rootDetector
	err := a.walk(func(entry *archives.StreamEntry) error {
		if !root.add(entry.Path) {
			return errStopWalk
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return root.prefix(), nil
}

// ListDir returns the entries directly inside dirPath, after prefix stripping,
// in a single pass that keeps only entry metadata.
func (a *browseArchive) ListDir(dirPath string) ([]archives.FileInfo, error) {
	var entries []archives.FileInfo
	var root rootDetector
	err := a.walk(func(entry *archives.StreamEntry) error {
		entries = append(entries, entry.FileInfo)
		root.add(entry.Path)
		return nil
	})
	if err != nil {
		return nil, err
	}

	prefix := npmPackagePrefix
	if a.ecosystem != "npm" {
		prefix = root.prefix()
	}
	return listBrowseDir(entries, prefix, dirPath), nil
}

// Extract returns the body of the first entry at filePath, streamed from the
// archive. The caller must close the returned reader.
func (a *browseArchive) Extract(filePath string) (io.ReadCloser, error) {
	prefix, err := a.prefix()
	if err != nil {
		return nil, err
	}
	target := prefix + filePath

	stream, release, err := a.openStream()
	if err != nil {
		return nil, err
	}
	for {
		entry, err := stream.Next()
		if err == io.EOF {
			release()
			return nil, fmt.Errorf("%w: %s", errBrowseNotFound, filePath)
		}
		if err != nil {
			release()
			return nil, err
		}
		if entry.Path != target {
			continue
		}
		if entry.IsDir {
			release()
			return nil, fmt.Errorf("%w: %s", errBrowseIsDir, filePath)
		}
		return &streamedFile{Reader: stream, release: release}, nil
	}
}

// streamedFile is an entry body that releases its archive stream on Close.
type streamedFile struct {
	io.Reader
	release func()
}

func (f *streamedFile) Close() error {
	if f.release != nil {
		f.release()
		f.release = nil
	}
	return nil
}

// rootDetector tracks whether every path seen so far lives under one
// top-level directory.
type rootDetector struct {
	root  string
	mixed bool
	seen  bool
}

// add records p and reports whether a single root is still possible.
func (d *rootDetector) add(p string) bool {
	if d.mixed {
		return false
	}
	d.seen = true
	dir, _, _ := strings.Cut(p, "/")
	if d.root == "" {
		d.root = dir
	} else if dir != d.root {
		d.mixed = true
	}
	return !d.mixed
}

// prefix returns the shared root with a trailing slash, or "" if paths have
// more than one root or none were seen.
func (d *rootDetector) prefix() string {
	if d.mixed || !d.seen || d.root == "" {
		return ""
	}
	return d.root + "/"
}

// listBrowseDir lists dirPath from entry metadata the same way a reader from
// archives.OpenWithPrefix does: the prefix is joined to dirPath before
// listing, then stripped from the results.
func listBrowseDir(entries []archives.FileInfo, prefix, dirPath string) []archives.FileInfo {
	files := listDir(entries, prefix+dirPath)
	if prefix == "" {
		return files
	}
	return stripBrowsePrefix(files, prefix)
}

// listDir mirrors archives.Reader.ListDir: entries directly in dirPath are
// returned in archive order. Deeper paths add one synthesized entry for each
// immediate subdirectory.
func listDir(entries []archives.FileInfo, dirPath string) []archives.FileInfo {
	dirPath = normalizeBrowseDir(dirPath)
	var files []archives.FileInfo
	seenDirs := make(map[string]bool)

	for _, f := range entries {
		p := f.Path

		if isInBrowseDir(p, dirPath) {
			if f.IsDir {
				name := strings.TrimSuffix(strings.TrimPrefix(p, dirPath), "/")
				if seenDirs[name] {
					continue
				}
				seenDirs[name] = true
			}
			files = append(files, f)
			continue
		}

		if dirPath != "" && !strings.HasPrefix(p, dirPath) {
			continue
		}
		rel := strings.TrimSuffix(strings.TrimPrefix(p, dirPath), "/")
		name, _, nested := strings.Cut(rel, "/")
		if nested && !seenDirs[name] {
			seenDirs[name] = true
			files = append(files, archives.FileInfo{
				Path:  dirPath + name + "/",
				Name:  name,
				IsDir: true,
			})
		}
	}

	return files
}

func normalizeBrowseDir(dirPath string) string {
	dirPath = strings.Trim(strings.TrimSpace(dirPath), "/")
	if dirPath == "" {
		return ""
	}
	return dirPath + "/"
}

// isInBrowseDir reports whether filePath is directly in the normalized
// dirPath. An entry naming the directory itself counts as inside it.
func isInBrowseDir(filePath, dirPath string) bool {
	filePath = strings.TrimSuffix(filePath, "/")
	if dirPath == "" {
		return !strings.Contains(filePath, "/")
	}
	if filePath == dirPath[:len(dirPath)-1] {
		return true
	}
	rest, ok := strings.CutPrefix(filePath, dirPath)
	return ok && !strings.Contains(rest, "/")
}

// stripBrowsePrefix removes prefix from each path, dropping entries outside
// it and the prefix directory itself.
func stripBrowsePrefix(files []archives.FileInfo, prefix string) []archives.FileInfo {
	result := make([]archives.FileInfo, 0, len(files))
	for _, f := range files {
		stripped, ok := strings.CutPrefix(f.Path, prefix)
		if !ok || stripped == "" || stripped == "/" {
			continue
		}
		f.Path = stripped
		f.Name = entryName(stripped)
		result = append(result, f)
	}
	return result
}

// entryName returns the last element of an archive path, ignoring a trailing
// slash.
func entryName(p string) string {
	p = strings.TrimSuffix(p, "/")
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}
