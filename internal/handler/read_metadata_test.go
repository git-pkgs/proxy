package handler

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"runtime"
	"testing"
	"testing/iotest"
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

func TestReadMetadataSized(t *testing.T) {
	const limit = 1024
	p := &Proxy{MetadataMaxSize: limit}
	body := bytes.Repeat([]byte("x"), 100)

	for _, tt := range []struct {
		name string
		body []byte
		size int64
	}{
		{"exact size", body, 100},
		{"unknown size", body, -1},
		{"size too small", body, 10},
		{"size too large", body, 900},
		{"size above limit", body, limit * 4},
		{"empty body", nil, 0},
		{"empty body with size", nil, 50},
		{"body at limit", bytes.Repeat([]byte("x"), limit), limit},
		{"body at limit, size too small", bytes.Repeat([]byte("x"), limit), 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			// OneByteReader makes sure short reads are not taken for EOF.
			got, err := p.ReadMetadataSized(iotest.OneByteReader(bytes.NewReader(tt.body)), tt.size)
			if err != nil {
				t.Fatalf("ReadMetadataSized: %v", err)
			}
			if !bytes.Equal(got, tt.body) {
				t.Errorf("got %d bytes, want %d", len(got), len(tt.body))
			}
		})
	}

	for _, size := range []int64{-1, 10, limit, limit + 1, limit * 4} {
		t.Run(fmt.Sprintf("over limit with size %d", size), func(t *testing.T) {
			_, err := p.ReadMetadataSized(bytes.NewReader(make([]byte, limit+1)), size)
			if !errors.Is(err, ErrMetadataTooLarge) {
				t.Errorf("got error %v, want ErrMetadataTooLarge", err)
			}
		})
	}

	t.Run("read error", func(t *testing.T) {
		errRead := errors.New("read failed")
		r := io.MultiReader(bytes.NewReader(body), iotest.ErrReader(errRead))
		if _, err := p.ReadMetadataSized(r, 100); !errors.Is(err, errRead) {
			t.Errorf("got error %v, want %v", err, errRead)
		}
	})
}

// TestReadMetadataSizedAllocatesOnce checks that a correct size hint reads
// into a single buffer of that size rather than a growing one.
func TestReadMetadataSizedAllocatesOnce(t *testing.T) {
	const size = 8 << 20
	p := &Proxy{MetadataMaxSize: 64 << 20}
	payload := bytes.Repeat([]byte("x"), size)

	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	data, err := p.ReadMetadataSized(bytes.NewReader(payload), size)
	runtime.ReadMemStats(&after)
	if err != nil || len(data) != size {
		t.Fatalf("read %d bytes, %v", len(data), err)
	}
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > size*3/2 {
		t.Errorf("allocated %d bytes reading %d", allocated, size)
	}
}
