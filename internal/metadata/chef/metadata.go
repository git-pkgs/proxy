// Package chef rewrites Chef Supermarket documents so every download
// location points at the proxy. It depends on nothing but the standard
// library, so it builds for targets such as WASI.
package chef

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"strings"
)

// MountPath and APIPath are where the proxy serves Chef Supermarket
// requests and its cookbook API under that.
const (
	MountPath = "/chef"
	APIPath   = "/api/v1"
)

// ErrDocument is returned for a document that isn't the JSON shape
// Supermarket serves.
var ErrDocument = errors.New("unexpected Chef Supermarket document")

// versionPattern matches Chef cookbook versions, which have two or three
// numeric parts.
var versionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+(\.[0-9]+)?$`)

// Options are the inputs the rewrites take.
type Options struct {
	// ProxyURL is the proxy's public URL, without a trailing slash.
	ProxyURL string
}

// RewriteUniverse points every version's location_path and download_url at
// the proxy. The universe maps cookbook names to versions to their location
// and dependencies; dependencies and any other field are copied as they are.
func RewriteUniverse(body []byte, opts Options) ([]byte, error) {
	var universe map[string]map[string]map[string]json.RawMessage
	if err := json.Unmarshal(body, &universe); err != nil {
		return nil, fmt.Errorf("%w: universe: %w", ErrDocument, err)
	}

	locationPath, err := jsonString(APIURL(opts.ProxyURL))
	if err != nil {
		return nil, err
	}
	for name, versions := range universe {
		for version, entry := range versions {
			if entry == nil {
				continue
			}
			downloadURL, err := jsonString(DownloadURL(opts.ProxyURL, name, version))
			if err != nil {
				return nil, err
			}
			entry["location_path"] = locationPath
			entry["download_url"] = downloadURL
		}
	}
	return EncodeJSON(universe)
}

// RewriteCookbook points a cookbook's version URLs at the proxy.
func RewriteCookbook(name string, body []byte, opts Options) ([]byte, error) {
	var cookbook map[string]json.RawMessage
	if err := json.Unmarshal(body, &cookbook); err != nil {
		return nil, fmt.Errorf("%w: cookbook %s: %w", ErrDocument, name, err)
	}

	if raw, ok := cookbook["latest_version"]; ok {
		rewritten, err := RewriteVersionURL(name, raw, opts)
		if err != nil {
			return nil, err
		}
		cookbook["latest_version"] = rewritten
	}
	if raw, ok := cookbook["versions"]; ok {
		var versions []json.RawMessage
		if err := json.Unmarshal(raw, &versions); err != nil {
			return nil, fmt.Errorf("%w: cookbook %s versions: %w", ErrDocument, name, err)
		}
		for i, versionURL := range versions {
			rewritten, err := RewriteVersionURL(name, versionURL, opts)
			if err != nil {
				return nil, err
			}
			versions[i] = rewritten
		}
		encoded, err := EncodeJSON(versions)
		if err != nil {
			return nil, err
		}
		cookbook["versions"] = encoded
	}
	return EncodeJSON(cookbook)
}

// RewriteVersion points a cookbook version's cookbook and file URLs at the
// proxy.
func RewriteVersion(name, version string, body []byte, opts Options) ([]byte, error) {
	var document map[string]json.RawMessage
	if err := json.Unmarshal(body, &document); err != nil {
		return nil, fmt.Errorf("%w: cookbook %s version %s: %w", ErrDocument, name, version, err)
	}

	if _, ok := document["cookbook"]; ok {
		cookbookURL, err := jsonString(CookbookURL(opts.ProxyURL, name))
		if err != nil {
			return nil, err
		}
		document["cookbook"] = cookbookURL
	}
	if _, ok := document["file"]; ok {
		fileURL, err := jsonString(DownloadURL(opts.ProxyURL, name, version))
		if err != nil {
			return nil, err
		}
		document["file"] = fileURL
	}
	return EncodeJSON(document)
}

// RewriteVersionURL rewrites an upstream cookbook version URL, a JSON string
// ending in /versions/{version}, to the proxy. Anything else, such as null,
// is returned unchanged.
func RewriteVersionURL(name string, raw json.RawMessage, opts Options) (json.RawMessage, error) {
	var versionURL string
	if err := json.Unmarshal(raw, &versionURL); err != nil {
		return raw, nil //nolint:nilerr // not a string, so not a URL to rewrite
	}
	parsed, err := url.Parse(versionURL)
	if err != nil {
		return raw, nil //nolint:nilerr // not a URL, so nothing to rewrite
	}
	version, ok := NormalizeVersion(path.Base(parsed.Path))
	if !ok {
		return raw, nil
	}
	return jsonString(VersionURL(opts.ProxyURL, name, version))
}

// APIURL is the proxy's cookbook API root.
func APIURL(proxyURL string) string {
	return proxyURL + MountPath + APIPath
}

// CookbookURL is where the proxy serves a cookbook's document.
func CookbookURL(proxyURL, name string) string {
	return APIURL(proxyURL) + "/cookbooks/" + url.PathEscape(name)
}

// VersionURL is where the proxy serves a cookbook version's document.
func VersionURL(proxyURL, name, version string) string {
	return CookbookURL(proxyURL, name) + "/versions/" + url.PathEscape(version)
}

// DownloadURL is where the proxy serves a cookbook version's archive.
func DownloadURL(proxyURL, name, version string) string {
	return VersionURL(proxyURL, name, version) + "/download"
}

// NormalizeVersion accepts a version in either form Supermarket serves
// (1.2.3, or 1_2_3 as Berkshelf writes it in URLs) and returns the dotted
// form.
func NormalizeVersion(version string) (string, bool) {
	version = strings.ReplaceAll(version, "_", ".")
	if !versionPattern.MatchString(version) {
		return "", false
	}
	return version, true
}

func jsonString(s string) (json.RawMessage, error) {
	return EncodeJSON(s)
}

// EncodeJSON encodes v without escaping HTML characters, so dependency
// constraints such as ">= 1.0" keep their upstream spelling.
func EncodeJSON(v any) (json.RawMessage, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}
