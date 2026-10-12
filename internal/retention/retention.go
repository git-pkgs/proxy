// Package retention holds the per-ecosystem rules for evicting cached
// artifacts that have not been accessed for a configured time.
//
// An ecosystem takes part only once its handler registers a Spec with
// Default, usually from an init function in the handler's own file. Until
// then configuring retention for it is a validation error, and the global
// default does not apply to it.
package retention

import (
	"fmt"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/git-pkgs/purl"
)

// KnownKeys lists the ecosystem keys storage.retention.ecosystems accepts.
// They are the literals the handlers pass when they fetch an artifact. A key
// outside this list is a typo; a key in it whose handler has not registered
// yet is not supported by this version.
var KnownKeys = []string{
	"alpine", "cargo", "composer", "conan", "conda", "cran", "deb", "gem",
	"golang", "helm", "hex", "julia", "maven", "npm", "nuget", "oci", "pub",
	"pypi", "rpm", "swift",
}

// IsKnown reports whether key is one of KnownKeys.
func IsKnown(key string) bool {
	return slices.Contains(KnownKeys, key)
}

// Spec describes how one ecosystem takes part in retention.
type Spec struct {
	// Key is the ecosystem's config key, one of KnownKeys.
	Key string

	// ExtraDBEcosystems lists packages.ecosystem values besides the ones
	// derived from Key that belong to this ecosystem.
	ExtraDBEcosystems []string

	// CanonicalPackage turns a package PURL from the config into the
	// package PURL the handler stores in the cache, reporting false when the
	// PURL does not belong to this ecosystem. Nil means the ecosystem accepts
	// no package overrides.
	CanonicalPackage func(p *purl.PURL) (string, bool)
}

// dbEcosystems returns every packages.ecosystem value that belongs to the
// spec: the key, its PURL type and the ecosystem name that type maps back
// to, the same spellings git-pkgs matches. The proxy's mirror command and
// git-pkgs store "rubygems" where the gem handler stores "gem", for example.
func (s Spec) dbEcosystems() []string {
	purlType := purl.EcosystemToPURLType(s.Key)
	values := []string{s.Key, purlType, purl.PURLTypeToEcosystem(purlType)}
	values = append(values, s.ExtraDBEcosystems...)
	slices.Sort(values)
	return slices.Compact(values)
}

// DefaultCanonical returns a CanonicalPackage for ecosystems that store the
// package PURL exactly as purl.MakePURLString builds it from the PURL's full
// name. It accepts a PURL only when building it back that way gives the same
// package, so for an ecosystem that stores a different form, such as one
// with a default namespace, it refuses the key instead of producing one
// that never matches. Such ecosystems need their own CanonicalPackage.
func DefaultCanonical(key string) func(p *purl.PURL) (string, bool) {
	purlType := purl.EcosystemToPURLType(key)
	return func(p *purl.PURL) (string, bool) {
		if p == nil || p.Type != purlType {
			return "", false
		}
		canonical := purl.MakePURLString(key, p.FullName(), "")
		if canonical == "" {
			return "", false
		}
		given := purl.New(p.Type, p.Namespace, p.Name, "", nil).String()
		return canonical, canonical == given
	}
}

// Registry maps ecosystem keys to their specs.
type Registry struct {
	mu    sync.RWMutex
	specs map[string]Spec
	byDB  map[string]string
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{specs: map[string]Spec{}, byDB: map[string]string{}}
}

// Default is the registry the handlers register with.
var Default = NewRegistry()

// Register adds s. It panics when the key is not one of KnownKeys, is
// already registered, or claims a packages.ecosystem value another spec
// claims, since each of those is a programming error.
func (r *Registry) Register(s Spec) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if !IsKnown(s.Key) {
		panic(fmt.Sprintf("retention: unknown ecosystem key %q", s.Key))
	}
	if _, ok := r.specs[s.Key]; ok {
		panic(fmt.Sprintf("retention: ecosystem %q registered twice", s.Key))
	}
	values := s.dbEcosystems()
	for _, v := range values {
		if owner, ok := r.byDB[v]; ok {
			panic(fmt.Sprintf("retention: ecosystem value %q claimed by both %q and %q", v, owner, s.Key))
		}
	}
	r.specs[s.Key] = s
	for _, v := range values {
		r.byDB[v] = s.Key
	}
}

// Lookup returns the spec registered for key.
func (r *Registry) Lookup(key string) (Spec, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.specs[key]
	return s, ok
}

// Keys returns the registered keys in sorted order.
func (r *Registry) Keys() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	keys := make([]string, 0, len(r.specs))
	for k := range r.specs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// KeyForDBEcosystem returns the registered key a packages.ecosystem value
// belongs to.
func (r *Registry) KeyForDBEcosystem(ecosystem string) (string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	key, ok := r.byDB[ecosystem]
	return key, ok
}

// Canonical returns the stored package PURL a configured package PURL
// stands for, asking the registered specs in key order.
func (r *Registry) Canonical(p *purl.PURL) (string, bool) {
	for _, key := range r.Keys() {
		s, _ := r.Lookup(key)
		if s.CanonicalPackage == nil {
			continue
		}
		if canonical, ok := s.CanonicalPackage(p); ok {
			return canonical, true
		}
	}
	return "", false
}

// Rules are resolved retention durations. A duration of zero means an
// artifact is never evicted by age.
type Rules struct {
	// Default applies to registered ecosystems without their own rule.
	Default time.Duration
	// Ecosystems holds rules keyed by registered ecosystem key.
	Ecosystems map[string]time.Duration
	// Packages holds rules keyed by the package PURL stored in the cache.
	Packages map[string]time.Duration
}

// For returns the retention for an artifact of the given ecosystem key and
// stored package PURL: the package rule, else the ecosystem rule, else the
// default.
func (r Rules) For(key, packagePURL string) time.Duration {
	if d, ok := r.Packages[packagePURL]; ok {
		return d
	}
	if d, ok := r.Ecosystems[key]; ok {
		return d
	}
	return r.Default
}

// MinPositive returns the shortest non-zero duration among all rules, or
// zero when every rule is zero.
func (r Rules) MinPositive() time.Duration {
	minimum := time.Duration(0)
	consider := func(d time.Duration) {
		if d > 0 && (minimum == 0 || d < minimum) {
			minimum = d
		}
	}
	consider(r.Default)
	for _, d := range r.Ecosystems {
		consider(d)
	}
	for _, d := range r.Packages {
		consider(d)
	}
	return minimum
}
