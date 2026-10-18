package server

import (
	"errors"
	"fmt"
	"io"

	"github.com/git-pkgs/archives"
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

var errArtifactTooLarge = errors.New("artifact too large for browsing")

// isBrowseLimitError reports whether err comes from one of the browse size or
// entry-count limits rather than a malformed archive.
func isBrowseLimitError(err error) bool {
	return errors.Is(err, errArtifactTooLarge) ||
		errors.Is(err, archives.ErrInputLimit) ||
		errors.Is(err, archives.ErrEntrySizeLimit) ||
		errors.Is(err, archives.ErrDecompressLimit) ||
		errors.Is(err, archives.ErrEntryLimit)
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

func newBrowseArchive(filename, ecosystem string, content io.Reader, reopen func() (io.ReadCloser, error), limits archives.StreamOptions) (*archives.Browser, error) {
	options := archives.BrowseOptions{Limits: limits, DetectRoot: true}
	if ecosystem == "npm" {
		options.StripPrefix = npmPackagePrefix
	}
	return archives.OpenBrowser(filename, content, reopen, options)
}
