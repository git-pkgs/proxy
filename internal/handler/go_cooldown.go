package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	gomodule "golang.org/x/mod/module"
	"golang.org/x/mod/semver"
	"golang.org/x/sync/errgroup"
)

// Go cooldown measures a version by the Time in its .info, which the module
// proxy takes from the commit the version points at. That is when the code
// was committed, not when the version was tagged or first fetched.
//
// Versions in cooldown are left out of @v/list and @latest, and their .info,
// .mod and .zip are answered with 403. The go command falls back to the next
// GOPROXY entry on 404 and 410, so with GOPROXY=proxy,direct a 404 would let
// it fetch the version directly.

// goInfoFetchLimit bounds the .info requests made at once to filter a list.
const goInfoFetchLimit = 8

const errGoCooldown = "version is in cooldown"

// goInfo is the JSON served for a version's .info and for @latest.
type goInfo struct {
	Version string    `json:"Version"`
	Time    time.Time `json:"Time"`
}

func (h *GoHandler) cooldownEnabled() bool {
	return h.proxy.Cooldown != nil && h.proxy.Cooldown.Enabled()
}

func (h *GoHandler) allowedAt(module string, published time.Time) bool {
	if published.IsZero() {
		return true
	}
	return h.proxy.cooldown().IsAllowed("golang", canonicalPackagePURL("golang", module), published)
}

// writeGoMetadataError answers a failed upstream metadata fetch.
func (h *GoHandler) writeGoMetadataError(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrUpstreamNotFound) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if writeUpstreamHeld(w, err) {
		return
	}
	h.proxy.Logger.Error("go metadata fetch failed", "error", err)
	http.Error(w, "failed to fetch from upstream", http.StatusBadGateway)
}

// fetchGoMetadata reads a module's @v/list, @latest, or a version's .info.
// The .info of a canonical version comes from the cache whatever its age,
// like proxyVersionFile serves it.
func (h *GoHandler) fetchGoMetadata(ctx context.Context, escapedModule, module, rest string) ([]byte, error) {
	cacheKey, upstreamURL := module+"/"+rest, h.upstreamURL+"/"+escapedModule+"/"+rest
	if v, ok := strings.CutPrefix(rest, "@v/"); ok && strings.HasSuffix(v, ".info") && goVersionImmutable(strings.TrimSuffix(v, ".info")) {
		res := h.proxy.fetchImmutableMetadata(ctx, "golang", cacheKey, upstreamURL, "*/*")
		return res.body, res.err
	}
	body, _, err := h.proxy.FetchOrCacheMetadata(ctx, "golang", cacheKey, upstreamURL, "*/*")
	return body, err
}

// Keep escaped module paths in publish-time keys for cache compatibility.
// Only canonical versions are stored; branch queries can resolve differently.
func (h *GoHandler) goTimeKey(module, version string) (versionPURL, packagePURL string, ok bool) {
	if h.proxy.DB == nil || gomodule.CanonicalVersion(version) != version {
		return "", "", false
	}
	escaped, err := gomodule.EscapePath(module)
	if err != nil {
		return "", "", false
	}
	return canonicalVersionPURL("golang", escaped, version), canonicalPackagePURL("golang", escaped), true
}

// publishTime returns a version's time from the versions table, or from its
// .info, which is then stored. A version upstream doesn't know, or whose
// .info has no time, gets the zero time and is let through, as in the other
// ecosystems. A failed fetch is an error, so a flaky or rate-limited
// upstream can't let a version through.
func (h *GoHandler) publishTime(ctx context.Context, escapedModule, module, version string) (time.Time, error) {
	if versionPURL, _, ok := h.goTimeKey(module, version); ok {
		if ver, err := h.proxy.DB.GetVersionByPURL(versionPURL); err == nil && ver != nil && ver.PublishedAt.Valid {
			return ver.PublishedAt.Time, nil
		}
	}

	escapedVersion, err := gomodule.EscapeVersion(version)
	if err != nil {
		return time.Time{}, nil
	}
	body, err := h.fetchGoMetadata(ctx, escapedModule, module, "@v/"+escapedVersion+".info")
	if errors.Is(err, ErrUpstreamNotFound) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	var info goInfo
	if err := json.Unmarshal(body, &info); err != nil {
		return time.Time{}, err
	}
	if !info.Time.IsZero() {
		h.storePublishTime(module, version, info.Time)
	}
	return info.Time, nil
}

func (h *GoHandler) storePublishTime(module, version string, published time.Time) {
	versionPURL, packagePURL, ok := h.goTimeKey(module, version)
	if !ok {
		return
	}
	if err := h.proxy.DB.SetVersionPublishedAt(versionPURL, packagePURL, published); err != nil {
		h.proxy.Logger.Warn("cooldown: could not store go publish time",
			"module", module, "version", version, "error", err)
	}
}

// checkCooldown answers a request for an exact version in cooldown, or whose
// time couldn't be read, and reports whether it did.
func (h *GoHandler) checkCooldown(w http.ResponseWriter, r *http.Request, escapedModule, module, version string) bool {
	if !h.cooldownEnabled() {
		return false
	}
	published, err := h.publishTime(r.Context(), escapedModule, module, version)
	if err != nil {
		h.proxy.Logger.Warn("cooldown: could not read go version time",
			"module", module, "version", version, "error", err)
		http.Error(w, "failed to check cooldown", http.StatusBadGateway)
		return true
	}
	if !h.allowedAt(module, published) {
		h.refuseCooldown(w, module, version)
		return true
	}
	return false
}

func (h *GoHandler) refuseCooldown(w http.ResponseWriter, module, version string) {
	h.proxy.Logger.Info("cooldown: withholding go module version", "module", module, "version", version)
	http.Error(w, errGoCooldown+": "+canonicalVersionPURL("golang", module, version), http.StatusForbidden)
}

// allowedVersions returns the listed versions of module that are out of
// cooldown, in their upstream order.
func (h *GoHandler) allowedVersions(ctx context.Context, escapedModule, module string) ([]string, error) {
	body, err := h.fetchGoMetadata(ctx, escapedModule, module, "@v/list")
	if err != nil {
		return nil, err
	}
	var versions []string
	for line := range strings.SplitSeq(string(body), "\n") {
		if v := strings.TrimSpace(line); v != "" {
			versions = append(versions, v)
		}
	}

	keep := make([]bool, len(versions))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(goInfoFetchLimit)
	for i, v := range versions {
		g.Go(func() error {
			published, err := h.publishTime(gctx, escapedModule, module, v)
			if err != nil {
				return fmt.Errorf("reading time of %s: %w", v, err)
			}
			keep[i] = h.allowedAt(module, published)
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}

	allowed := versions[:0]
	for i, v := range versions {
		if keep[i] {
			allowed = append(allowed, v)
		} else {
			h.proxy.Logger.Info("cooldown: filtering go module version", "module", module, "version", v)
		}
	}
	return allowed, nil
}

// serveCooldownList serves @v/list without versions in cooldown.
func (h *GoHandler) serveCooldownList(w http.ResponseWriter, r *http.Request, escapedModule, module string) {
	allowed, err := h.allowedVersions(r.Context(), escapedModule, module)
	if err != nil {
		h.writeGoMetadataError(w, err)
		return
	}
	w.Header().Set(headerContentType, "text/plain; charset=utf-8")
	if len(allowed) > 0 {
		_, _ = w.Write([]byte(strings.Join(allowed, "\n") + "\n"))
	}
}

// serveCooldownInfo serves a .info, refusing it when the version it names is
// in cooldown. rest is the request after /@v/: an escaped version or a query
// such as a branch name, followed by .info.
func (h *GoHandler) serveCooldownInfo(w http.ResponseWriter, r *http.Request, escapedModule, module, rest string) {
	body, err := h.fetchGoMetadata(r.Context(), escapedModule, module, "@v/"+rest)
	if err != nil {
		h.writeGoMetadataError(w, err)
		return
	}
	var info goInfo
	if err := json.Unmarshal(body, &info); err == nil && info.Version != "" {
		if !h.allowedAt(module, info.Time) {
			h.refuseCooldown(w, module, info.Version)
			return
		}
		if !info.Time.IsZero() {
			h.storePublishTime(module, info.Version, info.Time)
		}
	}
	w.Header().Set(headerContentType, contentTypeJSON)
	_, _ = w.Write(body)
}

// serveCooldownLatest serves @latest. When upstream's answer is in cooldown,
// it serves the newest listed version out of cooldown instead: the highest
// release, or else the highest pre-release, which is how the go command
// picks from a list.
func (h *GoHandler) serveCooldownLatest(w http.ResponseWriter, r *http.Request, escapedModule, module string) {
	body, err := h.fetchGoMetadata(r.Context(), escapedModule, module, "@latest")
	if err != nil {
		h.writeGoMetadataError(w, err)
		return
	}
	var info goInfo
	if err := json.Unmarshal(body, &info); err != nil || h.allowedAt(module, info.Time) {
		w.Header().Set(headerContentType, contentTypeJSON)
		_, _ = w.Write(body)
		return
	}

	allowed, err := h.allowedVersions(r.Context(), escapedModule, module)
	if err != nil {
		h.writeGoMetadataError(w, err)
		return
	}
	best := newestGoVersion(allowed)
	if best == "" {
		h.refuseCooldown(w, module, info.Version)
		return
	}
	escapedVersion, err := gomodule.EscapeVersion(best)
	if err != nil {
		h.refuseCooldown(w, module, info.Version)
		return
	}
	h.serveCooldownInfo(w, r, escapedModule, module, escapedVersion+".info")
}

// newestGoVersion returns the highest release in versions, or the highest
// pre-release when there is no release.
func newestGoVersion(versions []string) string {
	var release, prerelease string
	for _, v := range versions {
		if !semver.IsValid(v) {
			continue
		}
		if semver.Prerelease(v) == "" {
			if release == "" || semver.Compare(v, release) > 0 {
				release = v
			}
		} else if prerelease == "" || semver.Compare(v, prerelease) > 0 {
			prerelease = v
		}
	}
	if release != "" {
		return release
	}
	return prerelease
}
