// Package denylist implements an immutable, exact-version package policy.
package denylist

import (
	"fmt"
	"strings"

	"github.com/git-pkgs/purl"
)

// Policy is safe for concurrent reads. A nil policy allows every version.
type Policy struct {
	packages map[string]map[string]bool
}

// New validates and canonicalizes versioned PURLs. Qualifiers and subpaths are
// rejected because the proxy cannot reliably distinguish them at every endpoint,
// except a conda channel, which download URLs always name.
func New(packages []string) (*Policy, error) {
	if len(packages) == 0 {
		return nil, nil
	}
	p := &Policy{packages: make(map[string]map[string]bool)}
	for _, value := range packages {
		pkg, err := purl.Parse(value)
		if err != nil {
			return nil, fmt.Errorf("invalid denylist package %q: %w", value, err)
		}
		if pkg.Version == "" || !allowedQualifiers(pkg) || pkg.Subpath != "" ||
			strings.ContainsAny(pkg.Namespace+pkg.Name+pkg.Version, "*?[]<>=| \t\r\n") {
			return nil, fmt.Errorf("invalid denylist package %q: expected an exact versioned PURL without qualifiers or subpath", value)
		}
		id := identify(pkg)
		key := id.key
		if id.channel != "" {
			key = id.channelKey()
		}
		if p.packages[key] == nil {
			p.packages[key] = make(map[string]bool)
		}
		p.packages[key][id.version] = true
	}
	return p, nil
}

func allowedQualifiers(pkg *purl.PURL) bool {
	for _, q := range pkg.Qualifiers {
		if pkg.Type != "conda" || q.Key != "channel" {
			return false
		}
	}
	return true
}

// Denied reports whether a versioned PURL is denied.
func (p *Policy) Denied(value string) bool {
	if p == nil || len(p.packages) == 0 {
		return false
	}
	pkg, err := purl.Parse(value)
	if err != nil {
		return false
	}
	id := identify(pkg)
	versions := []string{id.version}
	// A platform gem's version carries the platform ("1.16.0-x86_64-linux"),
	// so also match the plain version.
	if pkg.Type == "gem" {
		if base, _, found := strings.Cut(id.version, "-"); found && base != "" {
			versions = append(versions, base)
		}
	}
	for _, version := range versions {
		if p.packages[id.key][version] || (id.channel != "" && p.packages[id.channelKey()][version]) {
			return true
		}
	}
	return false
}

// Versions returns a caller-owned set of denied versions for a package PURL.
func (p *Policy) Versions(packagePURL string) map[string]bool {
	if p == nil || len(p.packages) == 0 {
		return nil
	}
	pkg, err := purl.Parse(packagePURL)
	if err != nil {
		return nil
	}
	id := identify(pkg)
	result := make(map[string]bool)
	for version := range p.packages[id.key] {
		result[version] = true
	}
	if id.channel != "" {
		for version := range p.packages[id.channelKey()] {
			result[version] = true
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

// identity is the form a PURL is compared in: the same for denylist entries
// and for the PURLs handlers build, however each spells the package.
type identity struct {
	key     string // versionless PURL without channel
	channel string // conda channel, if any
	version string
}

func (id identity) channelKey() string {
	return id.key + "?channel=" + id.channel
}

// genericSwiftRegistry returns the scope and name of a Swift registry archive,
// which the proxy stores as pkg:generic/swift-registry/{scope}.{name}.
func genericSwiftRegistry(pkg *purl.PURL) (scope, name string, ok bool) {
	if pkg.Type != "generic" || !strings.EqualFold(pkg.Namespace, "swift-registry") {
		return "", "", false
	}
	scope, name, found := strings.Cut(pkg.Name, ".")
	return scope, name, found && scope != "" && name != ""
}

// identify compares PURLs in purl's comparison form. The generic
// swift-registry spelling is the proxy's own, so it is mapped here first.
func identify(pkg *purl.PURL) identity {
	if scope, name, ok := genericSwiftRegistry(pkg); ok {
		// Registry identities ignore case.
		pkg = purl.New("swift", strings.ToLower(scope), strings.ToLower(name), pkg.Version, nil)
	}
	c := pkg.ComparisonForm()
	var channel string
	if c.Type == "conda" {
		// An entry without a channel matches every channel.
		channel = c.Qualifier("channel")
	}
	key := purl.New(c.Type, c.Namespace, c.Name, "", nil).String()
	return identity{key: key, channel: channel, version: c.Version}
}
