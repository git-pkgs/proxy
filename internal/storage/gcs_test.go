package storage

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
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
	if err := store.Delete(ctx, "npm/pkg/file.tgz"); err != nil {
		t.Fatalf("Delete missing object = %v, want nil", err)
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

// TestGCSSignedURLSignEndpoint verifies that GCS presigned URLs have their
// scheme and host rewritten to the configured sign endpoint.
func TestGCSSignedURLSignEndpoint(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating RSA key: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	credsJSON, err := json.Marshal(map[string]string{
		"type":         "service_account",
		"client_email": "test@example.iam.gserviceaccount.com",
		"private_key":  string(pemBytes),
	})
	if err != nil {
		t.Fatalf("marshaling creds: %v", err)
	}
	credsFile := filepath.Join(t.TempDir(), "creds.json")
	if err := os.WriteFile(credsFile, credsJSON, 0o600); err != nil {
		t.Fatalf("writing creds: %v", err)
	}
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", credsFile)
	t.Setenv("STORAGE_EMULATOR_HOST", "")

	ctx := context.Background()
	store, err := OpenBucket(ctx, "gs://test-bucket", WithSignEndpoint("https://cdn.example.com"))
	if err != nil {
		t.Fatalf("OpenBucket failed: %v", err)
	}
	defer func() { _ = store.Close() }()

	signed, err := store.SignedURL(ctx, "npm/pkg/file.tgz", time.Minute)
	if err != nil {
		t.Fatalf("SignedURL failed: %v", err)
	}
	u, err := url.Parse(signed)
	if err != nil {
		t.Fatalf("parsing signed URL %q: %v", signed, err)
	}
	if u.Scheme != "https" || u.Host != "cdn.example.com" {
		t.Errorf("signed URL = %q, want https://cdn.example.com", signed)
	}
	if u.Path != "/test-bucket/npm/pkg/file.tgz" {
		t.Errorf("signed URL path = %q, want /test-bucket/npm/pkg/file.tgz", u.Path)
	}
	if u.Query().Get("Signature") == "" {
		t.Errorf("signed URL %q carries no signature", signed)
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
		name := objectNameFromPath(r.URL.Path)
		if _, ok := f.objects[name]; !ok {
			http.NotFound(w, r)
			return
		}
		delete(f.objects, name)
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
