package server

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/git-pkgs/archives"
	"github.com/git-pkgs/proxy/internal/database"
)

type archiveEntry struct {
	name string
	body string
	dir  bool
}

func buildTar(t testing.TB, entries []archiveEntry) []byte {
	t.Helper()
	buf := new(bytes.Buffer)
	tw := tar.NewWriter(buf)
	for _, e := range entries {
		header := &tar.Header{Name: e.name, Mode: 0o644, Size: int64(len(e.body)), Typeflag: tar.TypeReg}
		if e.dir {
			header.Typeflag = tar.TypeDir
			header.Mode = 0o755
			header.Size = 0
		}
		if err := tw.WriteHeader(header); err != nil {
			t.Fatalf("writing tar header: %v", err)
		}
		if _, err := io.WriteString(tw, e.body); err != nil {
			t.Fatalf("writing tar body: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("closing tar: %v", err)
	}
	return buf.Bytes()
}

func gzipBytes(t testing.TB, data []byte) []byte {
	t.Helper()
	buf := new(bytes.Buffer)
	gw := gzip.NewWriter(buf)
	if _, err := gw.Write(data); err != nil {
		t.Fatalf("writing gzip: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("closing gzip: %v", err)
	}
	return buf.Bytes()
}

func buildTarGz(t testing.TB, entries []archiveEntry) []byte {
	t.Helper()
	return gzipBytes(t, buildTar(t, entries))
}

func buildZip(t testing.TB, entries []archiveEntry) []byte {
	t.Helper()
	buf := new(bytes.Buffer)
	zw := zip.NewWriter(buf)
	for _, e := range entries {
		f, err := zw.Create(e.name)
		if err != nil {
			t.Fatalf("creating zip entry: %v", err)
		}
		if _, err := io.WriteString(f, e.body); err != nil {
			t.Fatalf("writing zip entry: %v", err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("closing zip: %v", err)
	}
	return buf.Bytes()
}

// buildGem wraps entries in a gem: an outer tar holding data.tar.gz.
func buildGem(t testing.TB, entries []archiveEntry) []byte {
	t.Helper()
	return buildTar(t, []archiveEntry{
		{name: "metadata.gz", body: string(gzipBytes(t, []byte("--- {}\n")))},
		{name: "data.tar.gz", body: string(buildTarGz(t, entries))},
	})
}

// testBrowseVersion is the version every browse fixture is cached under.
const testBrowseVersion = "1.0.0"

// cacheBrowseArtifact stores data as the cached artifact for ecosystem/name.
func cacheBrowseArtifact(t *testing.T, ts *testServer, ecosystem, name, filename string, data []byte) {
	t.Helper()

	storagePath := fmt.Sprintf("%s-%s-%s", ecosystem, name, filename)
	artifactsDir := filepath.Join(ts.tempDir, "artifacts")
	if err := os.MkdirAll(artifactsDir, 0o755); err != nil {
		t.Fatalf("creating artifacts dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(artifactsDir, storagePath), data, 0o644); err != nil {
		t.Fatalf("writing artifact: %v", err)
	}

	pkg := &database.Package{
		PURL:      fmt.Sprintf("pkg:%s/%s", ecosystem, name),
		Ecosystem: ecosystem,
		Name:      name,
	}
	if err := ts.db.UpsertPackage(pkg); err != nil {
		t.Fatalf("upserting package: %v", err)
	}
	ver := &database.Version{PURL: pkg.PURL + "@" + testBrowseVersion, PackagePURL: pkg.PURL}
	if err := ts.db.UpsertVersion(ver); err != nil {
		t.Fatalf("upserting version: %v", err)
	}
	artifact := &database.Artifact{
		VersionPURL: ver.PURL,
		Filename:    filename,
		UpstreamURL: "https://example.com/" + filename,
		StoragePath: sql.NullString{String: storagePath, Valid: true},
	}
	if err := ts.db.UpsertArtifact(artifact); err != nil {
		t.Fatalf("upserting artifact: %v", err)
	}
}

func browseGet(ts *testServer, target string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, target, nil)
	w := httptest.NewRecorder()
	ts.handler.ServeHTTP(w, req)
	return w
}

func browseListURL(ecosystem, name, dir string) string {
	return fmt.Sprintf("/ui/api/browse/%s/%s/%s?path=%s", ecosystem, name, testBrowseVersion, url.QueryEscape(dir))
}

func browseFileURL(ecosystem, name, file string) string {
	return fmt.Sprintf("/ui/api/browse/%s/%s/%s/file/%s", ecosystem, name, testBrowseVersion, file)
}

func decodeListing(t *testing.T, w *httptest.ResponseRecorder) BrowseListResponse {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("listing status = %d, body %s", w.Code, w.Body.String())
	}
	var resp BrowseListResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding listing: %v", err)
	}
	return resp
}

func listingPaths(resp BrowseListResponse) []string {
	paths := make([]string, len(resp.Files))
	for i, f := range resp.Files {
		paths[i] = f.Path
	}
	return paths
}

type browseFixture struct {
	name      string
	ecosystem string
	filename  string
	data      []byte
}

func browseFixtures(t *testing.T) []browseFixture {
	t.Helper()

	rooted := []archiveEntry{
		{name: "repo-abc123/", dir: true},
		{name: "repo-abc123/README.md", body: "# rooted\n"},
		{name: "repo-abc123/src/", dir: true},
		{name: "repo-abc123/src/main.go", body: "package main\n"},
		{name: "repo-abc123/src/util/strings.go", body: "package util\n"},
		{name: "repo-abc123/docs/guide.md", body: "guide\n"},
	}
	npm := []archiveEntry{
		{name: "package/package.json", body: `{"name":"x"}`},
		{name: "package/README.md", body: "# npm\n"},
		{name: "package/src/main.go", body: "package main\n"},
		{name: "package/src/util/strings.go", body: "package util\n"},
		{name: "other/ignored.txt", body: "outside the package prefix\n"},
	}
	flat := []archiveEntry{
		{name: "README.md", body: "# flat\n"},
		{name: "src/main.go", body: "package main\n"},
		{name: "src/util/strings.go", body: "package util\n"},
		{name: "docs/guide.md", body: "guide\n"},
	}

	return []browseFixture{
		{"tar.gz with root dir", "cargo", "demo-1.0.0.crate", buildTarGz(t, rooted)},
		{"extensionless tar.gz", "cargo", "artifact", buildTarGz(t, rooted)},
		{"npm package prefix", "npm", "demo-1.0.0.tgz", buildTarGz(t, npm)},
		{"flat tar.gz", "cargo", "demo-1.0.0.tar.gz", buildTarGz(t, flat)},
		{"zip with root dir", "composer", "demo.zip", buildZip(t, rooted)},
		{"extensionless zip", "composer", "d2e2f014ccd6ec9fae8dbe6336a4164346a2a856", buildZip(t, rooted)},
		{"flat zip", "composer", "demo.zip", buildZip(t, flat)},
		{"gem", "gem", "demo-1.0.0.gem", buildGem(t, flat)},
	}
}

func TestBrowseListingStripsPrefix(t *testing.T) {
	for _, fx := range browseFixtures(t) {
		t.Run(fx.name, func(t *testing.T) {
			ts := newTestServer(t)
			defer ts.close()
			cacheBrowseArtifact(t, ts, fx.ecosystem, "demo", fx.filename, fx.data)

			root := listingPaths(decodeListing(t, browseGet(ts, browseListURL(fx.ecosystem, "demo", ""))))
			for _, p := range root {
				if strings.HasPrefix(p, "repo-abc123/") || strings.HasPrefix(p, "package/") {
					t.Errorf("root listing kept archive prefix: %q", p)
				}
			}
			if !containsPath(root, "README.md") || !containsPath(root, "src/") {
				t.Errorf("root listing = %v, want README.md and src/", root)
			}

			src := listingPaths(decodeListing(t, browseGet(ts, browseListURL(fx.ecosystem, "demo", "src"))))
			if !containsPath(src, "src/main.go") || !containsPath(src, "src/util/") {
				t.Errorf("src listing = %v, want src/main.go and src/util/", src)
			}
		})
	}
}

func containsPath(paths []string, want string) bool {
	for _, p := range paths {
		if p == want {
			return true
		}
	}
	return false
}

// TestBrowseListingMatchesBufferedReader checks the streamed listing returns
// exactly what the buffered archive reader returned for the same request.
func TestBrowseListingMatchesBufferedReader(t *testing.T) {
	dirs := []string{"", "/", "src", "src/", "/src", "src/util", "docs", "missing", "README.md"}

	for _, fx := range browseFixtures(t) {
		t.Run(fx.name, func(t *testing.T) {
			ts := newTestServer(t)
			defer ts.close()
			cacheBrowseArtifact(t, ts, fx.ecosystem, "demo", fx.filename, fx.data)

			buffered, err := openArchive(fx.filename, bytes.NewReader(fx.data), fx.ecosystem)
			if err != nil {
				t.Fatalf("openArchive: %v", err)
			}
			defer func() { _ = buffered.Close() }()

			for _, dir := range dirs {
				want, err := buffered.ListDir(dir)
				if err != nil {
					t.Fatalf("buffered ListDir(%q): %v", dir, err)
				}
				got := decodeListing(t, browseGet(ts, browseListURL(fx.ecosystem, "demo", dir)))
				if len(got.Files) != len(want) {
					t.Fatalf("ListDir(%q) = %v, want %d entries", dir, listingPaths(got), len(want))
				}
				for i, f := range got.Files {
					w := want[i]
					if f.Path != w.Path || f.Name != w.Name || f.IsDir != w.IsDir || f.Size != w.Size {
						t.Errorf("ListDir(%q)[%d] = %+v, want %+v", dir, i, f, w)
					}
				}
			}
		})
	}
}

func TestBrowseFileContents(t *testing.T) {
	for _, fx := range browseFixtures(t) {
		t.Run(fx.name, func(t *testing.T) {
			ts := newTestServer(t)
			defer ts.close()
			cacheBrowseArtifact(t, ts, fx.ecosystem, "demo", fx.filename, fx.data)

			buffered, err := openArchive(fx.filename, bytes.NewReader(fx.data), fx.ecosystem)
			if err != nil {
				t.Fatalf("openArchive: %v", err)
			}
			defer func() { _ = buffered.Close() }()

			for _, file := range []string{"README.md", "src/main.go", "src/util/strings.go"} {
				rc, err := buffered.Extract(file)
				if err != nil {
					t.Fatalf("buffered Extract(%q): %v", file, err)
				}
				want, _ := io.ReadAll(rc)
				_ = rc.Close()

				w := browseGet(ts, browseFileURL(fx.ecosystem, "demo", file))
				if w.Code != http.StatusOK {
					t.Fatalf("file %s status = %d, body %s", file, w.Code, w.Body.String())
				}
				if w.Body.String() != string(want) {
					t.Errorf("file %s = %q, want %q", file, w.Body.String(), want)
				}
				wantType, _ := detectContentTypeFromPath(file)
				if got := w.Header().Get("Content-Type"); got != wantType {
					t.Errorf("file %s Content-Type = %q, want %q", file, got, wantType)
				}
				if got := w.Header().Get("Content-Disposition"); !strings.Contains(got, filepath.Base(file)) {
					t.Errorf("file %s Content-Disposition = %q", file, got)
				}
			}

			w := browseGet(ts, browseFileURL(fx.ecosystem, "demo", "missing.txt"))
			if w.Code != http.StatusNotFound {
				t.Errorf("missing file status = %d, want 404", w.Code)
			}
		})
	}
}

func TestBrowseFileFirstDuplicateWins(t *testing.T) {
	ts := newTestServer(t)
	defer ts.close()
	data := buildTarGz(t, []archiveEntry{
		{name: "package/index.js", body: "first"},
		{name: "package/index.js", body: "second"},
	})
	cacheBrowseArtifact(t, ts, "npm", "dup", "dup-1.0.0.tgz", data)

	w := browseGet(ts, browseFileURL("npm", "dup", "index.js"))
	if w.Code != http.StatusOK || w.Body.String() != "first" {
		t.Fatalf("got %d %q, want 200 \"first\"", w.Code, w.Body.String())
	}
}

func TestBrowseLimits(t *testing.T) {
	entries := []archiveEntry{
		{name: "package/a.txt", body: strings.Repeat("a", 100)},
		{name: "package/b.txt", body: strings.Repeat("b", 100)},
		{name: "package/c.txt", body: strings.Repeat("c", 100)},
	}
	tarGz := buildTarGz(t, entries)
	zipped := buildZip(t, entries)

	tests := []struct {
		name     string
		filename string
		data     []byte
		limits   archives.StreamOptions
		file     string
	}{
		{"entry count", "limit.tgz", tarGz, archives.StreamOptions{MaxEntries: 2}, "c.txt"},
		{"entry size", "limit.tgz", tarGz, archives.StreamOptions{MaxEntryBytes: 50}, "a.txt"},
		{"expanded size", "limit.tgz", tarGz, archives.StreamOptions{MaxExpandedBytes: 250}, "c.txt"},
		{"input size", "limit.tgz", tarGz, archives.StreamOptions{MaxInputBytes: int64(len(tarGz) / 2)}, "c.txt"},
		{"zip entry count", "limit.zip", zipped, archives.StreamOptions{MaxEntries: 2}, "c.txt"},
		{"zip input size", "limit.zip", zipped, archives.StreamOptions{MaxInputBytes: int64(len(zipped) / 2)}, "a.txt"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := newTestServer(t)
			defer ts.close()
			ts.server.browseLimits = tt.limits
			cacheBrowseArtifact(t, ts, "npm", "limited", tt.filename, tt.data)

			for _, target := range []string{
				browseListURL("npm", "limited", ""),
				browseFileURL("npm", "limited", tt.file),
			} {
				w := browseGet(ts, target)
				if w.Code != http.StatusInternalServerError {
					t.Fatalf("%s status = %d, want 500 (body %s)", target, w.Code, w.Body.String())
				}
				if !strings.Contains(w.Body.String(), "archive exceeds browse limits") {
					t.Errorf("%s body = %s, want limit error", target, w.Body.String())
				}
			}
		})
	}
}

// largeBrowseArchive is a gzip tarball with many unrelated entries and one
// small file. The padding compresses well, so the fixture is small on disk
// while its expanded size dwarfs what a streamed request should allocate.
func largeBrowseArchive(t testing.TB, entries, entrySize int) []byte {
	t.Helper()
	buf := new(bytes.Buffer)
	gw := gzip.NewWriter(buf)
	tw := tar.NewWriter(gw)
	padding := make([]byte, entrySize)
	write := func(name string, body []byte) {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body))}); err != nil {
			t.Fatalf("writing tar header: %v", err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatalf("writing tar body: %v", err)
		}
	}
	write("repo-large/README.md", []byte("small file\n"))
	for i := range entries {
		write(fmt.Sprintf("repo-large/vendor/blob%05d.bin", i), padding)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("closing tar: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("closing gzip: %v", err)
	}
	return buf.Bytes()
}

// allocatedBytes returns the bytes allocated while fn runs. Total allocation
// bounds the peak heap growth fn can cause.
func allocatedBytes(fn func()) uint64 {
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	fn()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

func TestBrowseLargeArchiveMemory(t *testing.T) {
	const (
		entries   = 1024
		entrySize = 32 << 10
		expanded  = entries * entrySize // 32 MiB
	)
	data := largeBrowseArchive(t, entries, entrySize)

	ts := newTestServer(t)
	defer ts.close()
	cacheBrowseArtifact(t, ts, "cargo", "large", "large-1.0.0.crate", data)

	buffered := allocatedBytes(func() {
		r, err := openArchive("large-1.0.0.crate", bytes.NewReader(data), "cargo")
		if err != nil {
			t.Fatalf("openArchive: %v", err)
		}
		_ = r.Close()
	})

	var listing BrowseListResponse
	list := allocatedBytes(func() {
		listing = decodeListing(t, browseGet(ts, browseListURL("cargo", "large", "")))
	})
	if !containsPath(listingPaths(listing), "README.md") || !containsPath(listingPaths(listing), "vendor/") {
		t.Fatalf("listing = %v, want README.md and vendor/", listingPaths(listing))
	}

	var w *httptest.ResponseRecorder
	file := allocatedBytes(func() {
		w = browseGet(ts, browseFileURL("cargo", "large", "README.md"))
	})
	if w.Code != http.StatusOK || w.Body.String() != "small file\n" {
		t.Fatalf("file = %d %q, want 200 \"small file\\n\"", w.Code, w.Body.String())
	}

	t.Logf("compressed %d bytes, expanded %d bytes", len(data), expanded)
	t.Logf("allocated: buffered open %d, streamed listing %d, streamed file %d", buffered, list, file)

	if buffered < expanded {
		t.Fatalf("buffered open allocated %d bytes, expected at least the expanded %d", buffered, expanded)
	}
	limit := uint64(expanded / 4)
	if list > limit {
		t.Errorf("streamed listing allocated %d bytes, want under %d", list, limit)
	}
	if file > limit {
		t.Errorf("streamed file read allocated %d bytes, want under %d", file, limit)
	}
}

func BenchmarkBrowseLargeArchive(b *testing.B) {
	data := largeBrowseArchive(b, 1024, 32<<10)
	artifactPath := filepath.Join(b.TempDir(), "large.crate")
	if err := os.WriteFile(artifactPath, data, 0o644); err != nil {
		b.Fatal(err)
	}

	b.Run("buffered-list", func(b *testing.B) { benchBufferedBrowse(b, data, false) })
	b.Run("streamed-list", func(b *testing.B) { benchStreamedBrowse(b, artifactPath, false) })
	b.Run("buffered-file", func(b *testing.B) { benchBufferedBrowse(b, data, true) })
	b.Run("streamed-file", func(b *testing.B) { benchStreamedBrowse(b, artifactPath, true) })
}

// benchBufferedBrowse lists the root, or reads README.md, through openArchive.
func benchBufferedBrowse(b *testing.B, data []byte, readFile bool) {
	b.ReportAllocs()
	for b.Loop() {
		r, err := openArchive("large.crate", bytes.NewReader(data), "cargo")
		if err != nil {
			b.Fatal(err)
		}
		if readFile {
			drainBrowseFile(b, r.Extract)
		} else if _, err := r.ListDir(""); err != nil {
			b.Fatal(err)
		}
		_ = r.Close()
	}
}

// benchStreamedBrowse lists the root, or reads README.md, through browseArchive.
func benchStreamedBrowse(b *testing.B, artifactPath string, readFile bool) {
	reopen := func() (io.ReadCloser, error) { return os.Open(artifactPath) }
	b.ReportAllocs()
	for b.Loop() {
		content, err := reopen()
		if err != nil {
			b.Fatal(err)
		}
		a, err := newBrowseArchive("large.crate", "cargo", content, reopen, defaultBrowseLimits)
		if err != nil {
			b.Fatal(err)
		}
		if readFile {
			drainBrowseFile(b, a.Extract)
		} else if _, err := a.ListDir(""); err != nil {
			b.Fatal(err)
		}
		_ = content.Close()
	}
}

func drainBrowseFile(b *testing.B, extract func(string) (io.ReadCloser, error)) {
	b.Helper()
	rc, err := extract("README.md")
	if err != nil {
		b.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, rc)
	_ = rc.Close()
}

// truncatedTar is a TAR whose only entry declares size bytes but carries just
// body, ending without the rest of the entry or the end-of-archive marker.
func truncatedTar(t testing.TB, name string, size int64, body []byte) []byte {
	t.Helper()
	buf := new(bytes.Buffer)
	tw := tar.NewWriter(buf)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: size}); err != nil {
		t.Fatalf("writing tar header: %v", err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatalf("writing tar body: %v", err)
	}
	// Deliberately not closed: tw.Close would fail on the short entry.
	return buf.Bytes()
}

func TestBrowseFileReadErrorBeforeResponse(t *testing.T) {
	complete := buildTar(t, []archiveEntry{{name: "package/data.txt", body: strings.Repeat("x", 4096)}})

	tests := []struct {
		name     string
		filename string
		data     []byte
		limits   archives.StreamOptions
		wantBody string
	}{
		{
			name:     "truncated entry body",
			filename: "short-1.0.0.tgz",
			data:     gzipBytes(t, truncatedTar(t, "package/data.txt", 4096, []byte("abc"))),
			wantBody: "failed to read file",
		},
		{
			name:     "input limit inside entry",
			filename: "short-1.0.0.tar",
			data:     complete,
			limits:   archives.StreamOptions{MaxInputBytes: 1024},
			wantBody: "archive exceeds browse limits",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := newTestServer(t)
			defer ts.close()
			ts.server.browseLimits = tt.limits
			cacheBrowseArtifact(t, ts, "npm", "short", tt.filename, tt.data)

			w := browseGet(ts, browseFileURL("npm", "short", "data.txt"))
			if w.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500 (body %q)", w.Code, w.Body.String())
			}
			if strings.Contains(w.Body.String(), "abc") || strings.Contains(w.Body.String(), "xxx") {
				t.Errorf("error response leaked partial file content: %q", w.Body.String())
			}
			if !strings.Contains(w.Body.String(), tt.wantBody) {
				t.Errorf("body = %q, want %q", w.Body.String(), tt.wantBody)
			}
		})
	}
}

func TestBrowseFileReadErrorAfterResponseStarts(t *testing.T) {
	const size = 4 * browsePrefetchSize
	body := bytes.Repeat([]byte("0123456789abcdef"), size/16)
	complete := buildTar(t, []archiveEntry{{name: "package/big.txt", body: string(body)}})

	tests := []struct {
		name     string
		filename string
		data     []byte
		limits   archives.StreamOptions
		wantErr  bool
	}{
		{
			name:     "complete file",
			filename: "big-1.0.0.tar",
			data:     complete,
		},
		{
			name:     "truncated entry body",
			filename: "big-1.0.0.tgz",
			data:     gzipBytes(t, truncatedTar(t, "package/big.txt", size, body[:size/2])),
			wantErr:  true,
		},
		{
			name:     "input limit inside entry",
			filename: "big-1.0.0.tar",
			data:     complete,
			limits:   archives.StreamOptions{MaxInputBytes: size / 2},
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := newTestServer(t)
			defer ts.close()
			ts.server.browseLimits = tt.limits
			cacheBrowseArtifact(t, ts, "npm", "big", tt.filename, tt.data)

			srv := httptest.NewServer(ts.handler)
			defer srv.Close()

			resp, err := http.Get(srv.URL + browseFileURL("npm", "big", "big.txt"))
			if err != nil {
				if tt.wantErr {
					return
				}
				t.Fatalf("GET: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			got, readErr := io.ReadAll(resp.Body)

			if !tt.wantErr {
				if resp.StatusCode != http.StatusOK || readErr != nil || !bytes.Equal(got, body) {
					t.Fatalf("status %d, read error %v, %d of %d bytes", resp.StatusCode, readErr, len(got), len(body))
				}
				return
			}
			if readErr == nil {
				t.Fatalf("status %d with %d of %d bytes and no read error; want an aborted response",
					resp.StatusCode, len(got), len(body))
			}
			if len(got) >= len(body) {
				t.Errorf("read %d bytes, want fewer than %d", len(got), len(body))
			}
		})
	}
}
