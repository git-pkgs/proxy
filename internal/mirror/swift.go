package mirror

import (
	"context"
	"fmt"
	"strings"

	"github.com/git-pkgs/proxy/internal/handler"
)

// mirrorSwift mirrors the source archive of a Swift registry release, with
// its release metadata, from the registry the proxy serves Swift packages
// from.
func (m *Mirror) mirrorSwift(ctx context.Context, pv PackageVersion, tracker *progressTracker) {
	upstream, err := m.registryURL("swift", pv.Name)
	if err != nil {
		m.recordFailure(tracker, pv, err)
		return
	}
	h := handler.NewSwiftHandler(m.proxy, "", upstream)
	var scope, name string
	if source := swiftSourceURL(pv.Name); source != "" {
		scope, name, err = h.ResolveIdentity(ctx, source)
	} else {
		scope, name, err = swiftScopeAndName(pv.Name)
	}
	if err != nil {
		m.recordFailure(tracker, pv, err)
		return
	}
	result, err := h.MirrorRelease(ctx, scope, name, pv.Version)
	if err != nil {
		m.recordFailure(tracker, pv, err)
		return
	}
	m.recordResult(tracker, pv, result)
}

// registryURL returns the registry base URL Versions lists purlType packages
// from, which is the configured upstream, or empty for the default one.
func (m *Mirror) registryURL(purlType, name string) (string, error) {
	if r, ok := m.Versions.(RegistryVersions); ok && r.URL != nil {
		return r.URL(purlType, name)
	}
	return "", nil
}

// swiftSourceURL returns the repository URL of a pkg:swift name whose
// namespace starts with a host, as in pkg:swift/github.com/apple/swift-nio,
// or "" for a name whose namespace is a registry scope.
func swiftSourceURL(name string) string {
	if host, _, ok := strings.Cut(name, "/"); ok && strings.Contains(host, ".") {
		return "https://" + name
	}
	return ""
}

// swiftScopeAndName splits the name of a pkg:swift PURL whose namespace is
// a registry scope, as in pkg:swift/apple/swift-nio for the identity
// apple.swift-nio.
func swiftScopeAndName(name string) (scope, pkg string, _ error) {
	scope, pkg, ok := strings.Cut(name, "/")
	if !ok || scope == "" || pkg == "" || strings.ContainsAny(scope, ".:") || strings.Contains(pkg, "/") {
		return "", "", fmt.Errorf("pkg:swift/%s needs a registry scope or a source repository as the namespace, such as pkg:swift/apple/swift-nio or pkg:swift/github.com/apple/swift-nio", name)
	}
	return scope, pkg, nil
}
