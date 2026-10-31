package server

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/git-pkgs/proxy/internal/config"
	"github.com/git-pkgs/proxy/internal/denylist"
	"github.com/git-pkgs/proxy/internal/handler"
	"github.com/git-pkgs/proxy/internal/retention"
	"github.com/git-pkgs/purl"
)

// A Swift package can be configured by its source repository, as SBOM tools
// and GHSA name it (pkg:swift/github.com/apple/swift-nio), as well as by its
// registry identity. The proxy stores and compares registry identities, so
// source repositories are resolved through the upstream's /identifiers: for
// the denylist at startup, which fails when one can't be, and for retention
// before each sweep, which leaves Swift unswept until all are.

// sourceResolveTimeout bounds resolving the denylist's source repositories at
// startup.
const sourceResolveTimeout = time.Minute

// sourceResolver resolves a package PURL that names a package by its source
// to the stored package PURLs retention compares.
type sourceResolver func(ctx context.Context, p *purl.PURL) ([]string, error)

// swiftRetentionResolver resolves Swift source repositories through h, to
// every identity the upstream lists.
func swiftRetentionResolver(h *handler.SwiftHandler, reg *retention.Registry) sourceResolver {
	return func(ctx context.Context, p *purl.PURL) ([]string, error) {
		resolved, err := h.ResolveSourcePURL(ctx, p)
		if err != nil {
			return nil, err
		}
		canonical := make([]string, 0, len(resolved))
		for _, r := range resolved {
			c, ok := reg.Canonical(r)
			if !ok {
				return nil, fmt.Errorf("%s resolved to %s, which names no stored package", p, r)
			}
			canonical = append(canonical, c)
		}
		return canonical, nil
	}
}

// retentionResolvers returns how the retention loop resolves package rules
// that name a package by its source, by ecosystem key.
func (s *Server) retentionResolvers(proxy *handler.Proxy) map[string]sourceResolver {
	return map[string]sourceResolver{
		"swift": swiftRetentionResolver(handler.NewSwiftHandler(proxy, s.cfg.BaseURL, s.cfg.Upstream.Swift), retention.Default),
	}
}

// resolveDenylistSources returns entries with each Swift source repository
// replaced by every registry identity the upstream lists for it, keeping the
// version. An entry that can't be resolved is an error: left out, it would
// let the version through.
func resolveDenylistSources(ctx context.Context, h *handler.SwiftHandler, entries []string) ([]string, error) {
	resolved := make([]string, 0, len(entries))
	for _, entry := range entries {
		p, err := purl.Parse(entry)
		if err != nil || handler.SwiftSourcePURL(p) == "" {
			resolved = append(resolved, entry)
			continue
		}
		identities, err := h.ResolveSourcePURL(ctx, p)
		if err != nil {
			return nil, fmt.Errorf("resolving denylist package %q: %w", entry, err)
		}
		for _, identity := range identities {
			resolved = append(resolved, identity.String())
		}
	}
	return resolved, nil
}

// resolveRetentionSources resolves the package rules in rules.Sources it can,
// moving them to rules.Packages. A rule that fails stays pending, so its
// ecosystem isn't swept, and is tried again before the next sweep.
func resolveRetentionSources(ctx context.Context, logger *slog.Logger, resolvers map[string]sourceResolver, rules retention.Rules) {
	for _, key := range slices.Sorted(maps.Keys(rules.Sources)) {
		resolve := resolvers[key]
		for _, source := range slices.Sorted(maps.Keys(rules.Sources[key])) {
			if resolve == nil {
				logger.Error("retention: no way to resolve package rule, not sweeping "+key, "package", source)
				continue
			}
			p, err := purl.Parse(source)
			if err != nil {
				continue // config validation refuses these
			}
			canonical, err := resolve(ctx, p)
			if err == nil {
				err = rules.Resolve(key, source, canonical)
			}
			if err != nil {
				logger.Warn("retention: could not resolve package rule, not sweeping "+key+" until it is",
					"package", source, "error", err)
				continue
			}
			logger.Info("retention: resolved package rule", "package", source, "stored", strings.Join(canonical, ", "))
		}
	}
}

// ConfigureDenylist sets proxy's denylist from cfg, resolving Swift source
// repositories through the upstream first. serve and proxy mirror call it
// once proxy can fetch metadata. An entry that can't be resolved is an
// error, so neither starts with a denylist that lets its versions through.
func ConfigureDenylist(cfg *config.Config, proxy *handler.Proxy) error {
	entries := cfg.Denylist.Packages
	// Report a malformed entry before asking the upstream about any.
	if _, err := denylist.New(entries); err != nil {
		return fmt.Errorf("configuring denylist: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), sourceResolveTimeout)
	defer cancel()
	entries, err := resolveDenylistSources(ctx, handler.NewSwiftHandler(proxy, cfg.BaseURL, cfg.Upstream.Swift), entries)
	if err != nil {
		return fmt.Errorf("configuring denylist: %w", err)
	}
	policy, err := denylist.New(entries)
	if err != nil {
		return fmt.Errorf("configuring denylist: %w", err)
	}
	proxy.Denylist = policy
	return nil
}
