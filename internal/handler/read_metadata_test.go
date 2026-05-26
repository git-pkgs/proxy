package handler

import (
	"bytes"
	"errors"
	"testing"
)

func TestReadMetadata(t *testing.T) {
	const limit = 1024
	p := &Proxy{MetadataMaxSize: limit}

	t.Run("small body", func(t *testing.T) {
		data := []byte("hello world")
		got, err := p.ReadMetadata(bytes.NewReader(data))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !bytes.Equal(got, data) {
			t.Errorf("got %q, want %q", got, data)
		}
	})

	t.Run("exactly at limit", func(t *testing.T) {
		data := make([]byte, limit)
		for i := range data {
			data[i] = 'x'
		}
		got, err := p.ReadMetadata(bytes.NewReader(data))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != limit {
			t.Errorf("got length %d, want %d", len(got), limit)
		}
	})

	t.Run("over limit returns error", func(t *testing.T) {
		data := make([]byte, limit+100)
		for i := range data {
			data[i] = 'x'
		}
		_, err := p.ReadMetadata(bytes.NewReader(data))
		if !errors.Is(err, ErrMetadataTooLarge) {
			t.Errorf("got error %v, want ErrMetadataTooLarge", err)
		}
	})

	t.Run("zero limit uses default", func(t *testing.T) {
		p := &Proxy{}
		data := make([]byte, 1<<20)
		got, err := p.ReadMetadata(bytes.NewReader(data))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != len(data) {
			t.Errorf("got length %d, want %d", len(got), len(data))
		}
	})
}
