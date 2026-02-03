package archive

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"strings"
)

type zipReader struct {
	data   []byte
	reader *zip.Reader
}

func openZip(content io.Reader) (Reader, error) {
	// Read entire content into memory
	data, err := io.ReadAll(content)
	if err != nil {
		return nil, fmt.Errorf("reading zip content: %w", err)
	}

	// Create zip reader from bytes
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("opening zip: %w", err)
	}

	return &zipReader{
		data:   data,
		reader: reader,
	}, nil
}

func (z *zipReader) List() ([]FileInfo, error) {
	files := make([]FileInfo, 0, len(z.reader.File))

	for _, f := range z.reader.File {
		files = append(files, fileInfoFromZip(f))
	}

	return files, nil
}

func (z *zipReader) ListDir(dirPath string) ([]FileInfo, error) {
	dirPath = normalizeDir(dirPath)
	var files []FileInfo

	// Track directories we've seen to avoid duplicates
	seenDirs := make(map[string]bool)

	for _, f := range z.reader.File {
		path := f.Name

		// Check if this file/dir is directly in the requested directory
		if isInDir(path, dirPath) {
			files = append(files, fileInfoFromZip(f))
			continue
		}

		// Check if we should add a subdirectory entry
		if dirPath == "" || strings.HasPrefix(path, dirPath) {
			rel := strings.TrimPrefix(path, dirPath)
			parts := strings.Split(strings.TrimSuffix(rel, "/"), "/")
			if len(parts) > 1 {
				// This file is in a subdirectory
				subdir := dirPath + parts[0] + "/"
				if !seenDirs[subdir] {
					seenDirs[subdir] = true
					files = append(files, FileInfo{
						Path:  subdir,
						Name:  parts[0],
						IsDir: true,
					})
				}
			}
		}
	}

	return files, nil
}

func (z *zipReader) Extract(filePath string) (io.ReadCloser, error) {
	// Find the file
	for _, f := range z.reader.File {
		if f.Name == filePath {
			if f.FileInfo().IsDir() {
				return nil, fmt.Errorf("path is a directory: %s", filePath)
			}
			return f.Open()
		}
	}

	return nil, fmt.Errorf("file not found: %s", filePath)
}

func (z *zipReader) Close() error {
	// No resources to clean up for zip reader
	z.data = nil
	z.reader = nil
	return nil
}

func fileInfoFromZip(f *zip.File) FileInfo {
	return FileInfo{
		Path:           f.Name,
		Name:           extractName(f.Name),
		Size:           int64(f.UncompressedSize64),
		CompressedSize: int64(f.CompressedSize64),
		ModTime:        f.Modified,
		IsDir:          f.FileInfo().IsDir(),
		Mode:           uint32(f.Mode()),
	}
}

func extractName(path string) string {
	path = strings.TrimSuffix(path, "/")
	if idx := strings.LastIndex(path, "/"); idx >= 0 {
		return path[idx+1:]
	}
	return path
}
