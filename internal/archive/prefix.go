package archive

import (
	"io"
	"strings"
)

// prefixStripper wraps a Reader and strips a prefix from all file paths.
type prefixStripper struct {
	reader Reader
	prefix string
}

func (p *prefixStripper) List() ([]FileInfo, error) {
	files, err := p.reader.List()
	if err != nil {
		return nil, err
	}

	return p.stripPrefix(files), nil
}

func (p *prefixStripper) ListDir(dirPath string) ([]FileInfo, error) {
	// Add prefix to the requested directory
	prefixedPath := p.prefix + dirPath
	files, err := p.reader.ListDir(prefixedPath)
	if err != nil {
		return nil, err
	}

	return p.stripPrefix(files), nil
}

func (p *prefixStripper) Extract(filePath string) (io.ReadCloser, error) {
	// Add prefix to the requested file path
	prefixedPath := p.prefix + filePath
	return p.reader.Extract(prefixedPath)
}

func (p *prefixStripper) Close() error {
	return p.reader.Close()
}

// stripPrefix removes the prefix from all file paths.
func (p *prefixStripper) stripPrefix(files []FileInfo) []FileInfo {
	result := make([]FileInfo, 0, len(files))

	for _, f := range files {
		// Skip files that don't have the prefix
		if !strings.HasPrefix(f.Path, p.prefix) {
			continue
		}

		// Strip the prefix
		stripped := f
		stripped.Path = strings.TrimPrefix(f.Path, p.prefix)
		stripped.Name = extractName(stripped.Path)

		// Skip if path is now empty (was the prefix directory itself)
		if stripped.Path == "" || stripped.Path == "/" {
			continue
		}

		result = append(result, stripped)
	}

	return result
}
