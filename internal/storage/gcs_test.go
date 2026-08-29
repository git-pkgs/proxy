package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestOpenBucketGCSRoundTripWithEmulator(t *testing.T) {
	server := httptest.NewServer(&fakeGCSServer{t: t, objects: map[string]string{}})
	defer server.Close()
	t.Setenv("STORAGE_EMULATOR_HOST", server.URL)

	ctx := context.Background()
	store, err := OpenBucket(ctx, "gs://test-bucket")
	if err != nil {
		t.Fatalf("OpenBucket failed: %v", err)
	}

	size, hash, err := store.Store(ctx, "npm/pkg/file.tgz", strings.NewReader("content"))
	if err != nil {
		t.Fatalf("Store failed: %v", err)
	}
	wantHash := sha256.Sum256([]byte("content"))
	if size != int64(len("content")) || hash != hex.EncodeToString(wantHash[:]) {
		t.Fatalf("Store returned size=%d hash=%q", size, hash)
	}

	exists, err := store.Exists(ctx, "npm/pkg/file.tgz")
	if err != nil || !exists {
		t.Fatalf("Exists = %v, %v; want true, nil", exists, err)
	}

	r, err := store.Open(ctx, "npm/pkg/file.tgz")
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	data, _ := io.ReadAll(r)
	_ = r.Close()
	if string(data) != "content" {
		t.Fatalf("Open content = %q, want content", data)
	}

	lister, ok := store.(interface {
		ListPrefix(context.Context, string) ([]ObjectInfo, error)
	})
	if !ok {
		t.Fatal("GCS storage does not support prefix listing")
	}
	list, err := lister.ListPrefix(ctx, "npm/")
	if err != nil {
		t.Fatalf("ListPrefix failed: %v", err)
	}
	if len(list) != 1 || list[0].Path != "npm/pkg/file.tgz" {
		t.Fatalf("ListPrefix = %#v", list)
	}

	if err := store.Delete(ctx, "npm/pkg/file.tgz"); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}
	exists, err = store.Exists(ctx, "npm/pkg/file.tgz")
	if err != nil || exists {
		t.Fatalf("Exists after delete = %v, %v; want false, nil", exists, err)
	}

	reader, err := store.Open(ctx, "npm/pkg/file.tgz")
	if reader != nil || !errors.Is(err, ErrNotFound) {
		t.Fatalf("Open missing object = %v, %v; want nil, ErrNotFound", reader, err)
	}
	if _, err := store.Size(ctx, "npm/pkg/file.tgz"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Size missing object = %v, want ErrNotFound", err)
	}
	if _, err := store.SignedURL(ctx, "npm/pkg/file.tgz", time.Minute); !errors.Is(err, ErrSignedURLUnsupported) {
		t.Fatalf("SignedURL with emulator = %v, want ErrSignedURLUnsupported", err)
	}
}

type fakeGCSServer struct {
	t       *testing.T
	objects map[string]string
}

func (f *fakeGCSServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/upload/storage/v1/b/test-bucket/o":
		name := r.URL.Query().Get("name")
		data, _ := io.ReadAll(r.Body)
		f.objects[name] = string(data)
		writeJSON(w, fakeGCSObject{Name: name, Size: strconv.Itoa(len(data)), Updated: time.Now().UTC().Format(time.RFC3339Nano)})
	case r.Method == http.MethodGet && r.URL.Path == "/storage/v1/b/test-bucket/o":
		prefix := r.URL.Query().Get("prefix")
		page := fakeGCSListResponse{}
		for name, data := range f.objects {
			if strings.HasPrefix(name, prefix) {
				page.Items = append(page.Items, fakeGCSObject{Name: name, Size: strconv.Itoa(len(data)), Updated: time.Now().UTC().Format(time.RFC3339Nano)})
			}
		}
		sort.Slice(page.Items, func(i, j int) bool { return page.Items[i].Name < page.Items[j].Name })
		writeJSON(w, page)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/storage/v1/b/test-bucket/o/"):
		name := objectNameFromPath(r.URL.Path)
		data, ok := f.objects[name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		if r.URL.Query().Get("alt") == "media" {
			_, _ = io.WriteString(w, data)
			return
		}
		writeJSON(w, fakeGCSObject{Name: name, Size: strconv.Itoa(len(data)), Updated: time.Now().UTC().Format(time.RFC3339Nano)})
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/storage/v1/b/test-bucket/o/"):
		delete(f.objects, objectNameFromPath(r.URL.Path))
		w.WriteHeader(http.StatusNoContent)
	default:
		f.t.Errorf("unexpected request: %s %s", r.Method, r.URL.String())
		http.Error(w, "unexpected request", http.StatusInternalServerError)
	}
}

type fakeGCSObject struct {
	Name    string `json:"name"`
	Size    string `json:"size"`
	Updated string `json:"updated"`
}

type fakeGCSListResponse struct {
	NextPageToken string          `json:"nextPageToken"`
	Items         []fakeGCSObject `json:"items"`
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func objectNameFromPath(p string) string {
	escaped := strings.TrimPrefix(p, "/storage/v1/b/test-bucket/o/")
	name, _ := url.PathUnescape(escaped)
	return name
}
