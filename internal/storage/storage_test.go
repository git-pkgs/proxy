package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"testing"
)

func TestIsFetchDir(t *testing.T) {
	for range 10 {
		if id := NewFetchID(); !isFetchDir(id) {
			t.Errorf("isFetchDir(%q) = false for a fetch id", id)
		}
	}
	for _, name := range []string{"1.0.0", "0123456789abcde", "0123456789abcdef0", "0123456789ABCDEF", "0123456789abcdeg", ""} {
		if isFetchDir(name) {
			t.Errorf("isFetchDir(%q) = true", name)
		}
	}
}

// assertLargeFileRoundTrip stores a 1MB file in the given storage, verifies size and
// hash, then reads it back and confirms the content matches.
func assertLargeFileRoundTrip(t *testing.T, s Storage) {
	t.Helper()
	ctx := context.Background()

	data := bytes.Repeat([]byte("x"), 1024*1024)

	size, hash, err := s.Store(ctx, "large/file.bin", bytes.NewReader(data))
	if err != nil {
		t.Fatalf("Store large file failed: %v", err)
	}
	if size != int64(len(data)) {
		t.Errorf("size = %d, want %d", size, len(data))
	}

	h := sha256.Sum256(data)
	wantHash := hex.EncodeToString(h[:])
	if hash != wantHash {
		t.Errorf("hash mismatch for large file")
	}

	r, _ := s.Open(ctx, "large/file.bin")
	defer func() { _ = r.Close() }()
	readBack, _ := io.ReadAll(r)
	if !bytes.Equal(readBack, data) {
		t.Error("large file content mismatch")
	}
}
