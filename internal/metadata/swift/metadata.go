// Package swift holds the Swift Package Registry metadata transformations
// the proxy applies, with their inputs passed explicitly so the package
// builds without the proxy's handler, storage or database.
package swift

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

const (
	// MaxScopeLength and MaxNameLength bound a package scope and name, as
	// the registry specification does.
	MaxScopeLength = 39
	MaxNameLength  = 100
)

// Options are the URLs the rewrites map between.
type Options struct {
	// ProxyURL is the proxy's public base URL, without a trailing slash.
	ProxyURL string
	// UpstreamURL is the registry's base URL, without a trailing slash.
	// Only URLs under it are rewritten.
	UpstreamURL string
}

// RewriteLinkHeader rewrites each <URL> in a Link header value with
// RewriteRegistryURL, resolving relative ones against upstreamRequestURL,
// and leaves the rest of the value as it is.
func RewriteLinkHeader(value, upstreamRequestURL string, o Options) string {
	var result strings.Builder
	for len(value) > 0 {
		start := strings.IndexByte(value, '<')
		if start < 0 {
			result.WriteString(value)
			break
		}
		endOffset := strings.IndexByte(value[start+1:], '>')
		if endOffset < 0 {
			result.WriteString(value)
			break
		}
		end := start + 1 + endOffset
		result.WriteString(value[:start+1])
		result.WriteString(RewriteRegistryURL(value[start+1:end], upstreamRequestURL, o))
		result.WriteByte('>')
		value = value[end+1:]
	}
	return result.String()
}

// RewriteRegistryURL points rawURL, resolved against upstreamRequestURL,
// at the proxy's /swift path when it is under the upstream URL. Other URLs,
// and any that don't parse, are returned as they are.
func RewriteRegistryURL(rawURL, upstreamRequestURL string, o Options) string {
	base, err := url.Parse(o.UpstreamURL)
	if err != nil {
		return rawURL
	}
	requestURL, err := url.Parse(upstreamRequestURL)
	if err != nil {
		return rawURL
	}
	reference, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	absolute := requestURL.ResolveReference(reference)
	if !strings.EqualFold(absolute.Scheme, base.Scheme) || !strings.EqualFold(absolute.Host, base.Host) {
		return rawURL
	}

	basePath := strings.TrimSuffix(base.EscapedPath(), "/")
	absolutePath := absolute.EscapedPath()
	if absolutePath != basePath && !strings.HasPrefix(absolutePath, basePath+"/") {
		return rawURL
	}
	suffix := strings.TrimPrefix(absolutePath, basePath)
	rewritten := o.ProxyURL + "/swift" + suffix
	if absolute.RawQuery != "" {
		rewritten += "?" + absolute.RawQuery
	}
	if absolute.Fragment != "" {
		rewritten += "#" + absolute.Fragment
	}
	return rewritten
}

// RewriteReleaseURLs points the url of each release in a package's release
// list at the proxy. A document without a releases object is returned as
// it is.
func RewriteReleaseURLs(scope, name string, body []byte, o Options) ([]byte, error) {
	var metadata map[string]any
	if err := json.Unmarshal(body, &metadata); err != nil {
		return nil, err
	}
	releases, ok := metadata["releases"].(map[string]any)
	if !ok {
		return body, nil
	}

	for version, value := range releases {
		release, ok := value.(map[string]any)
		if !ok {
			continue
		}
		if _, hasURL := release["url"]; !hasURL {
			continue
		}
		release["url"] = ReleaseURL(o.ProxyURL, scope, name, version)
	}
	return json.Marshal(metadata)
}

// ReleaseURL is where the proxy serves a release's metadata.
func ReleaseURL(proxyURL, scope, name, version string) string {
	return fmt.Sprintf(
		"%s/swift/%s/%s/%s",
		proxyURL,
		url.PathEscape(scope),
		url.PathEscape(name),
		url.PathEscape(version),
	)
}

// ArchiveInfo is what a release's metadata says about its source archive.
type ArchiveInfo struct {
	// Checksum is the archive's SHA-256, lowercase hex.
	Checksum string
	// Signature and SignatureFormat are set when the archive is signed.
	Signature       string
	SignatureFormat string
}

type releaseMetadata struct {
	Resources []struct {
		Name     string `json:"name"`
		Type     string `json:"type"`
		Checksum string `json:"checksum"`
		Signing  *struct {
			Signature string `json:"signatureBase64Encoded"`
			Format    string `json:"signatureFormat"`
		} `json:"signing"`
	} `json:"resources"`
}

// SourceArchive reads the source-archive resource from a release's
// metadata: its checksum, and its signature when it is signed.
func SourceArchive(body []byte) (ArchiveInfo, error) {
	var metadata releaseMetadata
	if err := json.Unmarshal(body, &metadata); err != nil {
		return ArchiveInfo{}, fmt.Errorf("parsing release metadata: %w", err)
	}
	for _, resource := range metadata.Resources {
		if resource.Name != "source-archive" || resource.Type != "application/zip" {
			continue
		}
		checksum, err := NormalizeChecksum(resource.Checksum)
		if err != nil {
			return ArchiveInfo{}, err
		}
		info := ArchiveInfo{Checksum: checksum}
		if resource.Signing != nil {
			if resource.Signing.Signature == "" || resource.Signing.Format == "" {
				return ArchiveInfo{}, errors.New("source archive signing metadata is incomplete")
			}
			info.Signature = resource.Signing.Signature
			info.SignatureFormat = resource.Signing.Format
		}
		return info, nil
	}
	return ArchiveInfo{}, errors.New("source archive is missing from release metadata")
}

// NormalizeChecksum returns a hex SHA-256 checksum in lowercase.
func NormalizeChecksum(checksum string) (string, error) {
	digest, err := hex.DecodeString(checksum)
	if err != nil || len(digest) != sha256.Size {
		return "", errors.New("source archive checksum is not a SHA-256 digest")
	}
	return hex.EncodeToString(digest), nil
}

// DigestHeader is the Digest header value for a hex SHA-256 checksum,
// empty when checksum isn't one.
func DigestHeader(checksum string) string {
	digest, err := hex.DecodeString(checksum)
	if err != nil || len(digest) != sha256.Size {
		return ""
	}
	return "sha-256=" + base64.StdEncoding.EncodeToString(digest)
}

// ValidPackageReference reports whether scope, name and version name a
// release: a valid scope and name, and a version that is a single path
// segment.
func ValidPackageReference(scope, name, version string) bool {
	return ValidScope(scope) && ValidPackageName(name) && version != "" && version != "." && version != ".." && !strings.ContainsAny(version, "/\\")
}

// CanonicalPackage is a package's scope and name as the registry compares
// them, lowercased.
func CanonicalPackage(scope, name string) (string, string) {
	return strings.ToLower(scope), strings.ToLower(name)
}

// ValidScope reports whether scope is a valid package scope: alphanumeric
// with single inner hyphens, at most MaxScopeLength long.
func ValidScope(scope string) bool {
	return validIdentifier(scope, MaxScopeLength, "-")
}

// ValidPackageName reports whether name is a valid package name:
// alphanumeric with single inner hyphens or underscores, at most
// MaxNameLength long.
func ValidPackageName(name string) bool {
	return validIdentifier(name, MaxNameLength, "-_")
}

func validIdentifier(value string, maxLength int, separators string) bool {
	if value == "" || len(value) > maxLength {
		return false
	}
	previousSeparator := false
	for i := 0; i < len(value); i++ {
		character := value[i]
		separator := strings.ContainsRune(separators, rune(character))
		if separator {
			if i == 0 || i == len(value)-1 || previousSeparator {
				return false
			}
			previousSeparator = true
			continue
		}
		if (character < 'a' || character > 'z') &&
			(character < 'A' || character > 'Z') &&
			(character < '0' || character > '9') {
			return false
		}
		previousSeparator = false
	}
	return true
}
