package storage

import (
	"context"
	"encoding/base64"
	"encoding/json"
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

func TestGCSRoundTripWithEmulator(t *testing.T) {
	objects := map[string]string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/upload/storage/v1/b/test-bucket/o":
			name := r.URL.Query().Get("name")
			data, _ := io.ReadAll(r.Body)
			objects[name] = string(data)
			writeJSON(w, gcsObject{Name: name, Size: strconv.Itoa(len(data)), Updated: time.Now().UTC().Format(time.RFC3339Nano)})
		case r.Method == http.MethodGet && r.URL.Path == "/storage/v1/b/test-bucket/o":
			prefix := r.URL.Query().Get("prefix")
			page := gcsListResponse{}
			for name, data := range objects {
				if strings.HasPrefix(name, prefix) {
					page.Items = append(page.Items, gcsObject{Name: name, Size: strconv.Itoa(len(data)), Updated: time.Now().UTC().Format(time.RFC3339Nano)})
				}
			}
			sort.Slice(page.Items, func(i, j int) bool { return page.Items[i].Name < page.Items[j].Name })
			writeJSON(w, page)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/storage/v1/b/test-bucket/o/"):
			name := objectNameFromPath(r.URL.Path)
			data, ok := objects[name]
			if !ok {
				http.NotFound(w, r)
				return
			}
			if r.URL.Query().Get("alt") == "media" {
				_, _ = io.WriteString(w, data)
				return
			}
			writeJSON(w, gcsObject{Name: name, Size: strconv.Itoa(len(data)), Updated: time.Now().UTC().Format(time.RFC3339Nano)})
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/storage/v1/b/test-bucket/o/"):
			delete(objects, objectNameFromPath(r.URL.Path))
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()
	t.Setenv("STORAGE_EMULATOR_HOST", server.URL)

	ctx := context.Background()
	store, err := OpenGCS(ctx, "gs://test-bucket")
	if err != nil {
		t.Fatalf("OpenGCS failed: %v", err)
	}

	size, hash, err := store.Store(ctx, "npm/pkg/file.tgz", strings.NewReader("content"))
	if err != nil {
		t.Fatalf("Store failed: %v", err)
	}
	if size != int64(len("content")) || hash == "" {
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

	list, err := store.ListPrefix(ctx, "npm/")
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
}

func TestGCSSignedURLUsesSigner(t *testing.T) {
	store := &GCS{
		bucket:   "test-bucket",
		accessID: "service@example.com",
		signBytes: func(_ context.Context, b []byte) ([]byte, error) {
			if !strings.Contains(string(b), "/test-bucket/npm/pkg/file.tgz") {
				t.Fatalf("string to sign = %q", b)
			}
			return []byte("signed"), nil
		},
	}

	got, err := store.SignedURL(context.Background(), "npm/pkg/file.tgz", time.Minute)
	if err != nil {
		t.Fatalf("SignedURL failed: %v", err)
	}
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("parsing signed URL: %v", err)
	}
	if u.Scheme != "https" || u.Host != "storage.googleapis.com" || u.Path != "/test-bucket/npm/pkg/file.tgz" {
		t.Fatalf("signed URL location = %s", got)
	}
	if u.Query().Get("GoogleAccessId") != "service@example.com" {
		t.Fatalf("GoogleAccessId = %q", u.Query().Get("GoogleAccessId"))
	}
	if u.Query().Get("Signature") != base64.StdEncoding.EncodeToString([]byte("signed")) {
		t.Fatalf("Signature = %q", u.Query().Get("Signature"))
	}
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
