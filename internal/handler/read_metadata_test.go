package handler

import (
	"bytes"
	"errors"
	"testing"
)

func TestReadMetadata(t *testing.T) {
	t.Run("small body", func(t *testing.T) {
		data := []byte("hello world")
		got, err := ReadMetadata(bytes.NewReader(data))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !bytes.Equal(got, data) {
			t.Errorf("got %q, want %q", got, data)
		}
	})

	t.Run("exactly at limit", func(t *testing.T) {
		data := make([]byte, maxMetadataSize)
		for i := range data {
			data[i] = 'x'
		}
		got, err := ReadMetadata(bytes.NewReader(data))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != int(maxMetadataSize) {
			t.Errorf("got length %d, want %d", len(got), maxMetadataSize)
		}
	})

	t.Run("over limit returns error", func(t *testing.T) {
		data := make([]byte, maxMetadataSize+100)
		for i := range data {
			data[i] = 'x'
		}
		_, err := ReadMetadata(bytes.NewReader(data))
		if !errors.Is(err, ErrMetadataTooLarge) {
			t.Errorf("got error %v, want ErrMetadataTooLarge", err)
		}
	})
}
