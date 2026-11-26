package handler

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/git-pkgs/proxy/internal/database"
	"github.com/git-pkgs/proxy/internal/metadata/swift"
	"github.com/git-pkgs/purl"
)

// Swift cooldown measures a release by the publishedAt in its release
// document, which the registry specification makes optional. The release
// list has no times, so the proxy reads each release's time once, from the
// newest release down to the first that is old enough, and stores it.
// Releases in cooldown are left out of the list, and their release
// document, manifests and source archive are answered with 403. A release
// without publishedAt is let through, so with an upstream that doesn't
// record the time, such as the default one, cooldown has no effect.

const errSwiftCooldown = "release is in cooldown"

// swiftPolicyPURL is the PURL cooldown rules name a package by,
// pkg:swift/{scope}/{name}, with version when it is set. The registry scope
// is the namespace, as in a denylist entry.
func swiftPolicyPURL(scope, name, version string) string {
	return purl.New("swift", scope, name, version, nil).String()
}

// swiftCooldownApplies reports whether a cooldown applies to scope.name, so
// its releases' times have to be read.
func (h *SwiftHandler) swiftCooldownApplies(scope, name string) bool {
	return h.proxy.cooldownEnabled() && h.proxy.cooldown().For("swift", swiftPolicyPURL(scope, name, "")) > 0
}

// publishTime returns a release's time from the versions table, or from its
// release document, and stores it there. A release upstream doesn't know,
// or whose document has no publishedAt, gets the zero time. A failed fetch
// is an error, so a flaky upstream can't let a release through.
func (h *SwiftHandler) publishTime(ctx context.Context, scope, name, version string) (time.Time, error) {
	key, err := swiftSourceArchiveKey(scope, name, version)
	if err != nil {
		return time.Time{}, nil
	}
	if h.proxy.DB != nil {
		if ver, err := h.proxy.DB.GetVersionByPURL(key.versionPURL); err == nil && ver != nil && ver.PublishedAt.Valid {
			return ver.PublishedAt.Time, nil
		}
	}
	body, _, err := h.proxy.FetchOrCacheMetadata(
		ctx, "swift", swiftReleaseCacheKey(scope, name, version), h.buildUpstreamURL(scope, name, version, "", ""), swiftAcceptJSON,
	)
	if errors.Is(err, ErrUpstreamNotFound) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	published, err := swift.PublishedAt(body)
	if err != nil || published.IsZero() {
		return published, err
	}
	if h.proxy.DB != nil {
		if err := h.proxy.DB.SetVersionPublishedAt(key.versionPURL, key.packagePURL, published); err != nil {
			h.proxy.Logger.Warn("cooldown: could not store swift publish time",
				"package", key.packageName, "version", version, "error", err)
		}
	}
	return published, nil
}

func (h *SwiftHandler) allowedAt(scope, name string, published time.Time) bool {
	if published.IsZero() {
		return true
	}
	return h.proxy.cooldown().IsAllowed("swift", swiftPolicyPURL(scope, name, ""), published)
}

// checkCooldown answers a request for a release in cooldown, or whose time
// couldn't be read, and reports whether it did. The time comes from the
// release document a source archive is checked against anyway, so an
// archive fails the same way without cooldown when it can't be read.
func (h *SwiftHandler) checkCooldown(w http.ResponseWriter, r *http.Request, scope, name, version string) bool {
	if !h.swiftCooldownApplies(scope, name) {
		return false
	}
	published, err := h.publishTime(r.Context(), scope, name, version)
	if err != nil {
		h.proxy.writeCooldownLookupError(w, "swift", scope+"/"+name, err)
		return true
	}
	if h.allowedAt(scope, name, published) {
		return false
	}
	h.recordWithheld(scope, name, version, published)
	h.proxy.Logger.Info("cooldown: withholding swift release", "package", scope+"/"+name, "version", version)
	writeSwiftProblem(w, http.StatusForbidden, errSwiftCooldown+": "+swiftPolicyPURL(scope, name, version))
	return true
}

// filterReleases removes releases in cooldown from a release list; see
// swift.FilterReleases.
func (h *SwiftHandler) filterReleases(ctx context.Context, scope, name string, body []byte) ([]byte, map[string]bool, error) {
	return swift.FilterReleases(swiftPolicyPURL(scope, name, ""), body, swift.CooldownOptions{
		Cooldown:    h.proxy.cooldown(),
		EvaluatedAt: time.Now(),
		PublishTime: func(version string) (time.Time, error) {
			return h.publishTime(ctx, scope, name, version)
		},
		OnWithheld: func(version string, published time.Time) {
			h.proxy.Logger.Info("cooldown: filtering swift release", "package", scope+"/"+name, "version", version)
			h.recordWithheld(scope, name, version, published)
		},
	})
}

// recordWithheld records a release cooldown held back under the PURLs its
// source archive is cached under, so both land on the same package row;
// see Proxy.recordWithheld.
func (h *SwiftHandler) recordWithheld(scope, name, version string, published time.Time) {
	key, err := swiftSourceArchiveKey(scope, name, version)
	if h.proxy.DB == nil || err != nil || published.IsZero() {
		return
	}
	pkg := &database.Package{PURL: key.packagePURL, Ecosystem: "swift", Name: key.packageName}
	if err := h.proxy.DB.RecordWithheldVersion(pkg, key.versionPURL, published); err != nil {
		h.proxy.Logger.Warn("cooldown: failed to record withheld version",
			"ecosystem", "swift", "name", key.packageName, "version", version, "error", err)
	}
}
