package server

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/git-pkgs/proxy/internal/storage"
)

type fakeGradleCacheStore struct {
	objects map[string]storage.ObjectInfo
}

func newFakeGradleCacheStore(objects []storage.ObjectInfo) *fakeGradleCacheStore {
	m := make(map[string]storage.ObjectInfo, len(objects))
	for _, obj := range objects {
		m[obj.Path] = obj
	}
	return &fakeGradleCacheStore{objects: m}
}

func (s *fakeGradleCacheStore) Store(_ context.Context, path string, r io.Reader) (int64, string, error) {
	data, _ := io.ReadAll(r)
	s.objects[path] = storage.ObjectInfo{Path: path, Size: int64(len(data)), ModTime: time.Now()}
	return int64(len(data)), "", nil
}

func (s *fakeGradleCacheStore) Open(_ context.Context, path string) (io.ReadCloser, error) {
	obj, ok := s.objects[path]
	if !ok {
		return nil, storage.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(make([]byte, obj.Size))), nil
}

func (s *fakeGradleCacheStore) Exists(_ context.Context, path string) (bool, error) {
	_, ok := s.objects[path]
	return ok, nil
}

func (s *fakeGradleCacheStore) Delete(_ context.Context, path string) error {
	delete(s.objects, path)
	return nil
}

func (s *fakeGradleCacheStore) Size(_ context.Context, path string) (int64, error) {
	obj, ok := s.objects[path]
	if !ok {
		return 0, storage.ErrNotFound
	}
	return obj.Size, nil
}

func (s *fakeGradleCacheStore) SignedURL(_ context.Context, _ string, _ time.Duration) (string, error) {
	return "", storage.ErrSignedURLUnsupported
}

func (s *fakeGradleCacheStore) UsedSpace(_ context.Context) (int64, error) {
	var total int64
	for _, obj := range s.objects {
		total += obj.Size
	}
	return total, nil
}

func (s *fakeGradleCacheStore) URL() string { return "mem://" }

func (s *fakeGradleCacheStore) Close() error { return nil }

func (s *fakeGradleCacheStore) ListPrefix(_ context.Context, prefix string) ([]storage.ObjectInfo, error) {
	objects := make([]storage.ObjectInfo, 0)
	for _, obj := range s.objects {
		if strings.HasPrefix(obj.Path, prefix) {
			objects = append(objects, obj)
		}
	}
	return objects, nil
}

func TestSweepGradleBuildCache_MaxAge(t *testing.T) {
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	store := newFakeGradleCacheStore([]storage.ObjectInfo{
		{Path: "_gradle/http-build-cache/old", Size: 10, ModTime: now.Add(-48 * time.Hour)},
		{Path: "_gradle/http-build-cache/new", Size: 10, ModTime: now.Add(-2 * time.Hour)},
	})

	deleted, freed, err := sweepGradleBuildCache(context.Background(), store, store, 24*time.Hour, 0, now)
	if err != nil {
		t.Fatalf("sweepGradleBuildCache() error = %v", err)
	}
	if deleted != 1 {
		t.Fatalf("deleted entries = %d, want 1", deleted)
	}
	if freed != 10 {
		t.Fatalf("freed bytes = %d, want 10", freed)
	}

	if _, ok := store.objects["_gradle/http-build-cache/old"]; ok {
		t.Fatal("old entry was not deleted")
	}
	if _, ok := store.objects["_gradle/http-build-cache/new"]; !ok {
		t.Fatal("new entry should remain")
	}
}

func TestSweepGradleBuildCache_MaxSizeOldestFirst(t *testing.T) {
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	store := newFakeGradleCacheStore([]storage.ObjectInfo{
		{Path: "_gradle/http-build-cache/a", Size: 5, ModTime: now.Add(-3 * time.Hour)},
		{Path: "_gradle/http-build-cache/b", Size: 5, ModTime: now.Add(-2 * time.Hour)},
		{Path: "_gradle/http-build-cache/c", Size: 5, ModTime: now.Add(-1 * time.Hour)},
	})

	deleted, freed, err := sweepGradleBuildCache(context.Background(), store, store, 0, 10, now)
	if err != nil {
		t.Fatalf("sweepGradleBuildCache() error = %v", err)
	}
	if deleted != 1 {
		t.Fatalf("deleted entries = %d, want 1", deleted)
	}
	if freed != 5 {
		t.Fatalf("freed bytes = %d, want 5", freed)
	}

	if _, ok := store.objects["_gradle/http-build-cache/a"]; ok {
		t.Fatal("oldest entry was not deleted")
	}
	if _, ok := store.objects["_gradle/http-build-cache/b"]; !ok {
		t.Fatal("middle entry should remain")
	}
	if _, ok := store.objects["_gradle/http-build-cache/c"]; !ok {
		t.Fatal("newest entry should remain")
	}
}
