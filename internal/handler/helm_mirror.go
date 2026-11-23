package handler

import (
	"context"
	"fmt"
	"path"
	"slices"
	"strings"
)

// MirrorChart caches a chart release of a configured repository under the
// key handleChart serves it from, checked against the digest the index
// lists. The index is read through the metadata cache, as handleIndex reads
// it, so with metadata caching on helm finds the chart there as well while
// the repository is down.
func (h *HelmHandler) MirrorChart(ctx context.Context, repository, chart, version string) (*CacheResult, error) {
	upstreamURL, ok := h.repositories[repository]
	if !ok {
		return nil, h.unknownRepositoryError(repository)
	}
	index := h.fetchIndex(ctx, repository, upstreamURL)
	if index.err != nil {
		return nil, fmt.Errorf("fetching the %s index: %w", repository, index.err)
	}
	charts, err := h.chartLookup(repository, upstreamURL, index)
	if err != nil {
		return nil, err
	}

	release, ok := charts.ByRelease[helmRelease{Chart: chart, Version: version}]
	switch {
	case !ok:
		return nil, fmt.Errorf("%w: %s %s is not in the %s index", ErrUpstreamNotFound, chart, version, repository)
	case release.First.OCI:
		return nil, fmt.Errorf("%s %s is published as %s, which helm pulls through the container registry routes; mirror it as an image: %s",
			chart, version, release.First.URL, ociChartPURL(release.First.URL, version))
	case h.chartOnCooldown(chart, release.Created):
		return nil, fmt.Errorf("%s %s is in cooldown, so the index doesn't list it", chart, version)
	}
	return h.proxy.GetOrFetchArtifactFromURLWithDigest(ctx, helmMetadataEcosystem, repository, release.Digest,
		release.First.Filename, release.First.URL, "sha256:"+release.Digest)
}

func (h *HelmHandler) unknownRepositoryError(repository string) error {
	names := make([]string, 0, len(h.repositories))
	for name := range h.repositories {
		names = append(names, name)
	}
	if len(names) == 0 {
		return fmt.Errorf("unknown Helm repository %q: upstream.helm configures none", repository)
	}
	slices.Sort(names)
	return fmt.Errorf("unknown Helm repository %q: upstream.helm configures %s", repository, strings.Join(names, ", "))
}

// ociChartPURL returns the image PURL that mirrors an OCI chart reference,
// tagged with the chart version when the reference has no tag or digest, as
// helm pulls it.
func ociChartPURL(reference, version string) string {
	repository := strings.TrimPrefix(reference, "oci://")
	if at := strings.LastIndex(repository, "@"); at >= 0 {
		repository, version = repository[:at], repository[at+1:]
	} else if colon := strings.LastIndex(repository, ":"); colon > strings.LastIndex(repository, "/") {
		repository, version = repository[:colon], repository[colon+1:]
	}
	return "pkg:oci/" + path.Base(repository) + "@" + version + "?repository_url=" + repository
}
