package server

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"net/http"
)

//go:embed static/*
var staticFiles embed.FS

// staticHandler returns an http.Handler that serves embedded static files.
// Asset URLs carry no version, so cached copies are revalidated by content
// ETag on every use.
func staticHandler() http.Handler {
	sub, _ := fs.Sub(staticFiles, "static")
	etags := staticETags(sub)
	files := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if etag, ok := etags[r.URL.Path]; ok {
			w.Header().Set("ETag", etag)
			w.Header().Set("Cache-Control", "no-cache")
		}
		files.ServeHTTP(w, r)
	})
}

// staticETags hashes every file in fsys, keyed by its path within fsys.
func staticETags(fsys fs.FS) map[string]string {
	etags := make(map[string]string)
	_ = fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := fs.ReadFile(fsys, path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		etags[path] = `"` + hex.EncodeToString(sum[:16]) + `"`
		return nil
	})
	return etags
}
