package handler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/git-pkgs/proxy/internal/metadata/chef"
)

// Chef cooldown measures a cookbook version by the published_at in its
// version document, which Supermarket sets when the version is uploaded.
// It is read once per version and stored in the versions table.
//
// A cookbook document has one updated_at, set when a version is published,
// so when that is out of cooldown none of its versions is looked up;
// otherwise they are, from the highest down to the first that is old
// enough. Versions in cooldown are left out of the cookbook document,
// latest_version names the newest one left, and the version document and
// download of one are answered with 403.
//
// /universe lists every version of every cookbook without times. The
// cookbooks that can have a version in cooldown are the ones updated within
// the longest cooldown configured, which the recently updated listing gives
// newest first. The proxy reads that listing and those cookbooks' documents
// from the upstream each time it filters the universe, rather than from the
// metadata cache, whose copies can be older than the universe it filters.

const (
	errChefCooldown = "version is in cooldown"
	// chefRecentPageSize is how many cookbooks a page of the recently
	// updated listing asks for, the most Supermarket returns.
	chefRecentPageSize = 100
	// maxChefRecentPages bounds the recently updated listing read to filter
	// the universe. Reaching it fails the universe rather than letting
	// versions through.
	maxChefRecentPages = 10
)

var errChefTooManyRecent = errors.New("too many cookbooks updated within the cooldown to check")

func (h *ChefHandler) cooldownApplies(name string) bool {
	return h.proxy.cooldownApplies(chefEcosystem, name)
}

// cooldownOptions returns the options for filtering name's versions.
func (h *ChefHandler) cooldownOptions(ctx context.Context, name string) chef.CooldownOptions {
	return chef.CooldownOptions{
		Cooldown:    h.proxy.cooldown(),
		EvaluatedAt: time.Now(),
		PublishTime: func(version string) (time.Time, error) {
			return h.publishTime(ctx, name, version)
		},
		OnWithheld: func(version string, published time.Time) {
			h.proxy.Logger.Info("cooldown: filtering chef cookbook version", "cookbook", name, "version", version)
			h.proxy.recordWithheld(chefEcosystem, name, version, published)
		},
	}
}

// publishTime returns a cookbook version's time from the versions table, or
// from its version document, which is then stored. A version upstream
// doesn't know, or whose document has no published_at, gets the zero time
// and is let through. A failed fetch is an error, so a flaky upstream can't
// let a version through.
func (h *ChefHandler) publishTime(ctx context.Context, name, version string) (time.Time, error) {
	versionPURL, packagePURL := canonicalVersionPURL(chefEcosystem, name, version), canonicalPackagePURL(chefEcosystem, name)
	if h.proxy.DB != nil && versionPURL != "" {
		if ver, err := h.proxy.DB.GetVersionByPURL(versionPURL); err == nil && ver != nil && ver.PublishedAt.Valid {
			return ver.PublishedAt.Time, nil
		}
	}
	res := h.proxy.fetchMetadataDocument(ctx, chefEcosystem, h.cacheKey("cookbooks/"+name+"/versions/"+version),
		h.upstreamVersionURL(name, version), contentTypeJSON, nil)
	if errors.Is(res.err, ErrUpstreamNotFound) {
		return time.Time{}, nil
	}
	if res.err != nil {
		return time.Time{}, res.err
	}
	published, err := chef.PublishedAt(res.body)
	if err != nil || published.IsZero() {
		return published, err
	}
	if h.proxy.DB != nil && versionPURL != "" {
		if err := h.proxy.DB.SetVersionPublishedAt(versionPURL, packagePURL, published); err != nil {
			h.proxy.Logger.Warn("cooldown: could not store chef publish time",
				"cookbook", name, "version", version, "error", err)
		}
	}
	return published, nil
}

// inCooldown reports whether a cookbook version is in cooldown.
func (h *ChefHandler) inCooldown(ctx context.Context, name, version string) (bool, error) {
	if !h.cooldownApplies(name) {
		return false, nil
	}
	published, err := h.publishTime(ctx, name, version)
	if err != nil || published.IsZero() {
		return false, err
	}
	if h.proxy.cooldown().IsAllowed("chef", canonicalPackagePURL(chefEcosystem, name), published) {
		return false, nil
	}
	h.proxy.recordWithheld(chefEcosystem, name, version, published)
	return true, nil
}

// refuseCooldown answers a request for a version in cooldown.
func (h *ChefHandler) refuseCooldown(w http.ResponseWriter, name, version string) {
	h.proxy.Logger.Info("cooldown: withholding chef cookbook version", "cookbook", name, "version", version)
	JSONError(w, http.StatusForbidden, errChefCooldown+": "+canonicalVersionPURL(chefEcosystem, name, version))
}

// checkVersionCooldown answers a version document request for a version in
// cooldown, or whose time couldn't be read, and reports whether it did.
func (h *ChefHandler) checkVersionCooldown(w http.ResponseWriter, r *http.Request, name, version string) bool {
	withheld, err := h.inCooldown(r.Context(), name, version)
	if err != nil {
		h.proxy.writeCooldownLookupError(w, chefEcosystem, name, err)
		return true
	}
	if withheld {
		h.refuseCooldown(w, name, version)
	}
	return withheld
}

// checkDownloadCooldown is checkVersionCooldown for a download: when the
// time can't be read, an archive already cached is served.
func (h *ChefHandler) checkDownloadCooldown(w http.ResponseWriter, r *http.Request, name, version string) bool {
	withheld, err := h.inCooldown(r.Context(), name, version)
	if err != nil {
		if !h.proxy.serveCachedDespiteCooldownError(w, r, chefEcosystem, name, version, h.archiveFilename(name, version), err) {
			h.proxy.writeCooldownLookupError(w, chefEcosystem, name, err)
		}
		return true
	}
	if withheld {
		h.refuseCooldown(w, name, version)
	}
	return withheld
}

// cookbookWithheld returns the versions in cooldown of the cookbook whose
// document is body.
func (h *ChefHandler) cookbookWithheld(ctx context.Context, name string, body []byte) (map[string]bool, error) {
	if !h.cooldownApplies(name) {
		return nil, nil
	}
	updated, versions, err := chef.Cookbook(body)
	if err != nil {
		return nil, err
	}
	return chef.Withheld(canonicalPackagePURL(chefEcosystem, name), updated, versions, h.cooldownOptions(ctx, name))
}

// universeWithheld returns, for each cookbook with versions in cooldown,
// the versions of it the universe lists that are in cooldown.
func (h *ChefHandler) universeWithheld(ctx context.Context, universe []byte) (map[string]map[string]bool, error) {
	if !h.proxy.cooldownEnabled() {
		return nil, nil
	}
	longest := h.proxy.LongestCooldown
	if longest <= 0 {
		longest = h.proxy.cooldown().For(chefEcosystem, "")
	}
	updated, err := h.recentCookbooks(ctx, time.Now().Add(-longest))
	if err != nil {
		return nil, err
	}

	names := make([]string, 0, len(updated))
	for _, c := range updated {
		if h.cooldownApplies(c.name) {
			names = append(names, c.name)
		}
	}
	listed, err := chef.UniverseVersions(universe, names)
	if err != nil {
		return nil, err
	}
	withheld := map[string]map[string]bool{}
	for _, c := range updated {
		versions, ok := listed[c.name]
		if !ok {
			continue
		}
		w, err := chef.Withheld(canonicalPackagePURL(chefEcosystem, c.name), c.updated, versions, h.cooldownOptions(ctx, c.name))
		if err != nil {
			return nil, fmt.Errorf("cookbook %s: %w", c.name, err)
		}
		if len(w) > 0 {
			withheld[c.name] = w
		}
	}
	return withheld, nil
}

// recentCookbook is a cookbook from the recently updated listing and its
// updated_at.
type recentCookbook struct {
	name    string
	updated time.Time
}

// recentCookbooks reads the recently updated listing from the newest down
// to the first cookbook updated before cutoff, and fails when that takes
// more than maxChefRecentPages pages.
func (h *ChefHandler) recentCookbooks(ctx context.Context, cutoff time.Time) ([]recentCookbook, error) {
	var recent []recentCookbook
	// The listing can shift between pages as cookbooks are updated, so a
	// cookbook can come up twice.
	seen := map[string]bool{}
	for page := range maxChefRecentPages {
		start := page * chefRecentPageSize
		listURL := fmt.Sprintf("%s%s/cookbooks?order=recently_updated&start=%d&items=%d", h.upstreamURL, chefAPIPath, start, chefRecentPageSize)
		body, err := h.freshDocument(ctx, fmt.Sprintf("recently_updated/%d", start), listURL)
		if err != nil {
			return nil, fmt.Errorf("reading recently updated cookbooks: %w", err)
		}
		names, total, err := chef.RecentCookbooks(body)
		if err != nil {
			return nil, err
		}
		for _, name := range names {
			if seen[name] || !chefCookbookName.MatchString(name) || containsPathTraversal(name) {
				continue
			}
			seen[name] = true
			updated, found, err := h.cookbookUpdated(ctx, name)
			if err != nil {
				return nil, err
			}
			if !found {
				continue
			}
			if !updated.IsZero() && updated.Before(cutoff) {
				return recent, nil
			}
			recent = append(recent, recentCookbook{name, updated})
		}
		if len(names) == 0 || start+len(names) >= total {
			return recent, nil
		}
	}
	return nil, errChefTooManyRecent
}

// cookbookUpdated reads a cookbook's updated_at from the upstream. found is
// false for a cookbook the upstream doesn't have.
func (h *ChefHandler) cookbookUpdated(ctx context.Context, name string) (updated time.Time, found bool, err error) {
	body, err := h.freshDocument(ctx, "cookbooks/"+name, h.upstreamCookbookURL(name))
	if errors.Is(err, ErrUpstreamNotFound) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("reading cookbook %s: %w", name, err)
	}
	updated, _, err = chef.Cookbook(body)
	return updated, true, err
}

// freshDocument fetches a document from the upstream, storing it in the
// metadata cache. A stale copy is not the upstream's answer, so it is an
// error.
func (h *ChefHandler) freshDocument(ctx context.Context, documentKey, upstreamURL string) ([]byte, error) {
	res := h.proxy.refreshMetadata(ctx, chefEcosystem, h.cacheKey(documentKey), upstreamURL, contentTypeJSON, "", nil)
	if res.err == nil && res.stale {
		res.err = fmt.Errorf("chef metadata could not be fetched: %w", res.upstreamErr)
	}
	return res.body, res.err
}
