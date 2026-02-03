package archive

import (
	"archive/tar"
	"fmt"
	"io"
)

// gemReader handles Ruby .gem files which have a nested structure:
// - gem file is a tar archive containing metadata.gz and data.tar.gz
// - data.tar.gz contains the actual source code
type gemReader struct {
	dataReader Reader // The inner data.tar.gz reader
}

func openGem(content io.Reader) (Reader, error) {
	// Read the gem file as a tar archive
	tr := tar.NewReader(content)

	// Find data.tar.gz in the gem
	for {
		header, err := tr.Next()
		if err == io.EOF {
			return nil, fmt.Errorf("data.tar.gz not found in gem")
		}
		if err != nil {
			return nil, fmt.Errorf("reading gem tar: %w", err)
		}

		// Look for data.tar.gz
		if header.Name == "data.tar.gz" {
			// Read the data.tar.gz content
			dataContent, err := io.ReadAll(tr)
			if err != nil {
				return nil, fmt.Errorf("reading data.tar.gz: %w", err)
			}

			// Open the inner tar.gz
			dataReader, err := openTar(io.NopCloser(newBytesReader(dataContent)), "gzip")
			if err != nil {
				return nil, fmt.Errorf("opening data.tar.gz: %w", err)
			}

			return &gemReader{dataReader: dataReader}, nil
		}
	}
}

func (g *gemReader) List() ([]FileInfo, error) {
	return g.dataReader.List()
}

func (g *gemReader) ListDir(dirPath string) ([]FileInfo, error) {
	return g.dataReader.ListDir(dirPath)
}

func (g *gemReader) Extract(filePath string) (io.ReadCloser, error) {
	return g.dataReader.Extract(filePath)
}

func (g *gemReader) Close() error {
	if g.dataReader != nil {
		return g.dataReader.Close()
	}
	return nil
}

// bytesReader implements io.Reader from a byte slice
type bytesReader struct {
	data []byte
	pos  int
}

func newBytesReader(data []byte) *bytesReader {
	return &bytesReader{data: data}
}

func (b *bytesReader) Read(p []byte) (n int, err error) {
	if b.pos >= len(b.data) {
		return 0, io.EOF
	}
	n = copy(p, b.data[b.pos:])
	b.pos += n
	return n, nil
}

func (b *bytesReader) Close() error {
	return nil
}
