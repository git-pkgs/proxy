package handler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/git-pkgs/proxy/internal/metadata/chef"
)

// The universe filter reads the recently updated listing and those
// cookbooks' documents from the upstream each time, rather than from the
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
			h.recordWithheld(name, version, published)
		},
	}
}

// publishTime returns a cookbook version's time from the versions table, or
// from its version document, which is then stored. A version upstream
// doesn't know, or whose document has no published_at, gets the zero time
// and is let through. A failed fetch is an error, so a flaky upstream can't
// let a version through.
func (h *ChefHandler) publishTime(ctx context.Context, name, version string) (time.Time, error) {
	versionPURL, packagePURL := h.timeKey(name, version)
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

// timeKey returns the versions-table PURLs a version's time is stored
// under. Another upstream's keys carry it as repository_url, like a Maven
// named repository's: the same cookbook version there can have been
// published at another time than on the public Supermarket, and pointing
// upstream.chef elsewhere must not reuse the times stored for it.
func (h *ChefHandler) timeKey(name, version string) (versionPURL, packagePURL string) {
	versionPURL, packagePURL = canonicalVersionPURL(chefEcosystem, name, version), canonicalPackagePURL(chefEcosystem, name)
	if versionPURL == "" || h.timeQualifier == "" {
		return versionPURL, packagePURL
	}
	return versionPURL + h.timeQualifier, packagePURL + h.timeQualifier
}

// recordWithheld records a version cooldown held back for the package page;
// see Proxy.recordWithheld. The page shows the public Supermarket's times,
// which another upstream's would be mistaken for, so only those are kept.
func (h *ChefHandler) recordWithheld(name, version string, published time.Time) {
	if h.timeQualifier == "" {
		h.proxy.recordWithheld(chefEcosystem, name, version, published)
	}
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
	if h.proxy.cooldown().IsAllowed(chefEcosystem, canonicalPackagePURL(chefEcosystem, name), published) {
		return false, nil
	}
	h.recordWithheld(name, version, published)
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
	info, err := chef.Cookbook(body)
	if err != nil {
		return nil, err
	}
	return chef.Withheld(canonicalPackagePURL(chefEcosystem, name), info.UpdatedAt, info.Versions, h.cooldownOptions(ctx, name))
}

// universeWithheld returns, for each cookbook with versions in cooldown,
// the versions of it the universe lists that are in cooldown.
func (h *ChefHandler) universeWithheld(ctx context.Context, universe []byte) (map[string]map[string]bool, error) {
	if h.proxy.LongestCooldown == nil {
		return nil, nil
	}
	longest := h.proxy.LongestCooldown(chefEcosystem)
	if longest <= 0 {
		return nil, nil
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

// recentCookbooks returns the cookbooks updated since cutoff. Supermarket's
// recently updated listing has the cookbooks that aren't deprecated first,
// newest first, and then the deprecated ones, newest first. Both sections
// are read from the newest down to the first cookbook updated before
// cutoff; the start of the deprecated section is found by a binary search.
// Reading more than maxChefRecentPages pages fails.
func (h *ChefHandler) recentCookbooks(ctx context.Context, cutoff time.Time) ([]recentCookbook, error) {
	var recent []recentCookbook
	start, deprecated := 0, false
	for range maxChefRecentPages {
		names, total, err := h.recentPage(ctx, start, chefRecentPageSize)
		if err != nil {
			return nil, err
		}
		next := start + len(names)
		for i, name := range names {
			info, found, err := h.cookbookInfo(ctx, name)
			if err != nil {
				return nil, err
			}
			if !found {
				continue
			}
			deprecated = deprecated || info.Deprecated
			if info.UpdatedAt.IsZero() || !info.UpdatedAt.Before(cutoff) {
				recent = append(recent, recentCookbook{name, info.UpdatedAt})
				continue
			}
			if deprecated {
				return recent, nil
			}
			// The rest of the section is older still; skip to the
			// deprecated cookbooks.
			if next, err = h.firstDeprecated(ctx, start+i+1, total); err != nil {
				return nil, err
			}
			deprecated = true
			break
		}
		if len(names) == 0 || next >= total {
			return recent, nil
		}
		start = next
	}
	return nil, errChefTooManyRecent
}

// recentPage reads names from the recently updated listing, from start on,
// and the listing's length.
func (h *ChefHandler) recentPage(ctx context.Context, start, items int) ([]string, int, error) {
	listURL := fmt.Sprintf("%s%s/cookbooks?order=recently_updated&start=%d&items=%d", h.upstreamURL, chefAPIPath, start, items)
	body, err := h.freshDocument(ctx, fmt.Sprintf("recently_updated/%d/%d", start, items), listURL)
	if err != nil {
		return nil, 0, fmt.Errorf("reading recently updated cookbooks: %w", err)
	}
	names, total, err := chef.RecentCookbooks(body)
	if err != nil {
		return nil, 0, err
	}
	valid := names[:0]
	for _, name := range names {
		if chefCookbookName.MatchString(name) && !containsPathTraversal(name) {
			valid = append(valid, name)
		}
	}
	return valid, total, nil
}

// firstDeprecated returns the position of the first deprecated cookbook in
// the recently updated listing between lo and hi, or hi when there is none.
func (h *ChefHandler) firstDeprecated(ctx context.Context, lo, hi int) (int, error) {
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		names, _, err := h.recentPage(ctx, mid, 1)
		if err != nil {
			return 0, err
		}
		// A position past the end, as when the listing shrank, counts as
		// the end of the section.
		deprecated := len(names) == 0
		if !deprecated {
			info, found, err := h.cookbookInfo(ctx, names[0])
			if err != nil {
				return 0, err
			}
			// A cookbook deleted since the listing named it is in neither
			// section, and guessing one could skip the cookbooks before it.
			if !found {
				return 0, fmt.Errorf("cookbook %s is in the recently updated listing but has no document", names[0])
			}
			deprecated = info.Deprecated
		}
		if deprecated {
			hi = mid
		} else {
			lo = mid + 1
		}
	}
	return lo, nil
}

// cookbookInfo reads a cookbook's document from the upstream. found is
// false for a cookbook the upstream doesn't have.
func (h *ChefHandler) cookbookInfo(ctx context.Context, name string) (info chef.CookbookInfo, found bool, err error) {
	body, err := h.freshDocument(ctx, "cookbooks/"+name, h.upstreamCookbookURL(name))
	if errors.Is(err, ErrUpstreamNotFound) {
		return chef.CookbookInfo{}, false, nil
	}
	if err != nil {
		return chef.CookbookInfo{}, false, fmt.Errorf("reading cookbook %s: %w", name, err)
	}
	info, err = chef.Cookbook(body)
	return info, true, err
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
