package handler

import "time"

// WithCooldown creates a policy view sharing storage, fetches and rewrite space.
// Configure the original proxy before constructing its views.
func (p *Proxy) WithCooldown(policy CooldownPolicy, shortest time.Duration) *Proxy {
	view := &Proxy{
		DB: p.DB, Storage: p.Storage, Fetcher: p.Fetcher, Resolver: p.Resolver,
		RouteFor: p.RouteFor, Logger: p.Logger,
		Cooldown: policy, ShortestCooldown: shortest, Denylist: p.Denylist,
		CacheMetadata: p.CacheMetadata, MetadataTTL: p.MetadataTTL,
		MetadataMaxSize: p.MetadataMaxSize, MetadataStreamMaxSize: p.MetadataStreamMaxSize,
		GradleReadOnly: p.GradleReadOnly, HostedEnabled: p.HostedEnabled,
		GradleMaxUploadSize: p.GradleMaxUploadSize, NPMFullMetadata: p.NPMFullMetadata,
		DirectServe: p.DirectServe, DirectServeTTL: p.DirectServeTTL,
		HTTPClient: p.HTTPClient,
		RateLimits: p.RateLimits, HostedNPMScopes: p.HostedNPMScopes,
		HostedComposerVendors: p.HostedComposerVendors, StreamArtifacts: p.StreamArtifacts,
		Scanners: p.Scanners, ScanSigningKey: p.ScanSigningKey, ScanFetchBaseURL: p.ScanFetchBaseURL,
		rewrites: p.rewrites, now: p.now, started: p.started, shared: p.sharedProxy(),
	}
	if view.cooldownEnabled() {
		view.shared.viewCooldown.Store(true)
	}
	return view
}

func (p *Proxy) sharedProxy() *Proxy {
	if p.shared != nil {
		return p.shared
	}
	return p
}

// anyCooldownEnabled reports whether cooldown is on for this proxy, the one
// it is a view of, or another view of that one. What they keep in one cache
// row has to suit all of them.
func (p *Proxy) anyCooldownEnabled() bool {
	shared := p.sharedProxy()
	return p.cooldownEnabled() || shared.cooldownEnabled() || shared.viewCooldown.Load()
}
