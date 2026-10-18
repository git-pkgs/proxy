package npm

import "github.com/Masterminds/semver/v3"

func (h *transformer) applyDenylistFiltering(metadata, versions map[string]any) {
	denied := h.DeniedVersions
	if len(denied) == 0 {
		return
	}
	times, _ := metadata["time"].(map[string]any)
	for version := range denied {
		delete(versions, version)
		delete(times, version)
	}
	tags, _ := metadata["dist-tags"].(map[string]any)
	for tag, value := range tags {
		version, _ := value.(string)
		if !denied[version] {
			continue
		}
		delete(tags, tag)
		// Custom tags describe publisher intent; do not retarget them. For
		// latest, use the highest remaining stable version, even in abbreviated
		// packuments without publication timestamps.
		if tag == "latest" {
			if latest := newestStableNPMVersion(versions); latest != "" {
				tags[tag] = latest
			}
		}
	}
}

func newestStableNPMVersion(versions map[string]any) string {
	var best *semver.Version
	result := ""
	for version := range versions {
		parsed, err := semver.StrictNewVersion(version)
		if err != nil || parsed.Prerelease() != "" {
			continue
		}
		if best == nil || parsed.GreaterThan(best) || (parsed.Equal(best) && version > result) {
			best, result = parsed, version
		}
	}
	return result
}
