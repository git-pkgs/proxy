package handler

import (
	"crypto/sha256"
	"encoding/hex"
	"path"
	"sort"
	"strings"
)

// routeIDLength is the number of hex characters of the upstream URL digest
// that qualify cache entries of a routed package.
const routeIDLength = 16

// packageRoutes sends packages whose names match a pattern to a dedicated
// upstream instead of the ecosystem's default registry. A matched package is
// never looked up anywhere else, so a public package with the same name cannot
// replace it.
type packageRoutes struct {
	routes []packageRoute // longest pattern first
}

type packageRoute struct {
	pattern string
	url     string
	id      string
}

func newPackageRoutes(patterns map[string]string) packageRoutes {
	routes := make([]packageRoute, 0, len(patterns))
	for pattern, upstreamURL := range patterns {
		upstreamURL = strings.TrimSuffix(upstreamURL, "/")
		digest := sha256.Sum256([]byte(upstreamURL))
		routes = append(routes, packageRoute{
			pattern: strings.ToLower(pattern),
			url:     upstreamURL,
			id:      hex.EncodeToString(digest[:])[:routeIDLength],
		})
	}
	sort.Slice(routes, func(i, j int) bool {
		if len(routes[i].pattern) != len(routes[j].pattern) {
			return len(routes[i].pattern) > len(routes[j].pattern)
		}
		return routes[i].pattern < routes[j].pattern
	})
	return packageRoutes{routes: routes}
}

// match returns the route for a package name, if any pattern matches it.
func (r packageRoutes) match(packageName string) (packageRoute, bool) {
	name := strings.ToLower(packageName)
	for _, route := range r.routes {
		if ok, _ := path.Match(route.pattern, name); ok {
			return route, true
		}
	}
	return packageRoute{}, false
}

// cacheKey qualifies a package's metadata cache key with the route's upstream,
// so entries cached from the default registry, or from a route's previous
// upstream, are never served for a routed package.
func (r packageRoute) cacheKey(packageName string) string {
	return "routes/" + r.id + "/" + packageName
}

// cacheFilename qualifies an artifact filename the same way as cacheKey.
func (r packageRoute) cacheFilename(filename string) string {
	return r.id + "/" + filename
}
