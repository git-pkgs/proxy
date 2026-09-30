package server

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/git-pkgs/proxy/internal/database"
	"github.com/git-pkgs/proxy/internal/metrics"
)

// analyticsTopSources caps how many callers the page lists; the rest are
// summarised in the overflow row, and the access log has every one of them.
const analyticsTopSources = 15

// AnalyticsData contains data for rendering the analytics dashboard.
type AnalyticsData struct {
	Layout
	Totals          AnalyticsTotals
	Donut           DonutView
	EnrichmentStats EnrichmentStatsView
	Ecosystems      []EcosystemRow
	Runtime         RuntimeView
	// StatsFailed records that the per-ecosystem query errored, so the page
	// can say the figures are unavailable instead of claiming an empty cache.
	StatsFailed bool
	// StatsStale is set when the query errored but a previous snapshot was
	// retained and is being shown. Without it a database that has been down
	// for an hour renders hour-old figures as current, with the failure
	// visible only in the logs.
	StatsStale bool
	// StatsAge is when the retained snapshot was read, phrased for display.
	StatsAge string
}

// AnalyticsTotals holds the headline figures across every ecosystem.
type AnalyticsTotals struct {
	DownloadedBytes  int64
	Downloaded       string
	Downloads        string
	CacheSize        string
	CachedArtifacts  string
	Packages         string
	Versions         string
	Ecosystems       int
	ActiveEcosystems int
	// Amplification is accumulated download volume divided by the bytes
	// currently held in cache: how many times over the cache has served
	// what it stores. Empty when nothing is cached.
	Amplification string
}

// EcosystemRow is one ecosystem's row in the analytics table.
type EcosystemRow struct {
	Ecosystem       string
	DownloadedBytes int64
	Downloaded      string
	Downloads       string
	CacheSize       string
	AvgArtifactSize string
	Artifacts       string
	Packages        string
	Versions        string
	// SharePct is this ecosystem's share of the accumulated download total.
	SharePct string
}

func formatCount(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}

	var b strings.Builder
	for i, digit := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(digit)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

// percentOf renders part/whole as a percentage with one decimal place, always
// as a bare number. formatPercent wraps it to catch the case where a real
// share rounds away to zero.
func percentOf(part, whole int64) string {
	if whole <= 0 {
		return "0.0"
	}
	return strconv.FormatFloat(float64(part)/float64(whole)*100, 'f', 1, 64) //nolint:mnd // percent
}

// formatPercent renders a share for display. A share that is real but rounds
// to zero reads as "<0.1" rather than "0.0", which would claim the ecosystem
// served nothing.
func formatPercent(part, whole int64) string {
	pct := percentOf(part, whole)
	if pct == "0.0" && part > 0 && whole > 0 {
		return "<0.1"
	}
	return pct
}

// formatRatio renders an "N.Nx" multiplier, or "" when the denominator is zero.
func formatRatio(numerator, denominator int64) string {
	if denominator <= 0 {
		return ""
	}
	return fmt.Sprintf("%.1fx", float64(numerator)/float64(denominator))
}

// avgArtifactSize returns the mean cached artifact size, or a dash when the
// ecosystem holds nothing.
func avgArtifactSize(cacheSize, artifacts int64) string {
	if artifacts <= 0 {
		return "—"
	}
	return formatSize(cacheSize / artifacts)
}

// enrichmentStatsView maps enrichment totals onto the shared view struct used
// by both the dashboard and the analytics page.
func enrichmentStatsView(stats *database.EnrichmentStats) EnrichmentStatsView {
	return EnrichmentStatsView{
		EnrichedPackages:     stats.EnrichedPackages,
		VulnSyncedPackages:   stats.VulnSyncedPackages,
		TotalVulnerabilities: stats.TotalVulnerabilities,
		CriticalVulns:        stats.CriticalVulns,
		HighVulns:            stats.HighVulns,
		MediumVulns:          stats.MediumVulns,
		LowVulns:             stats.LowVulns,
		HasVulns:             stats.TotalVulnerabilities > 0,
	}
}

// handleAnalytics renders the analytics dashboard: accumulated download volume
// in total and per ecosystem, alongside the cache and enrichment figures the
// main dashboard reports.
func (s *Server) handleAnalytics(w http.ResponseWriter, r *http.Request) {
	ecosystems, err := s.ecoStats.Get(s.db)
	statsFailed := err != nil
	if err != nil {
		s.logger.Error("failed to get ecosystem stats", "error", err)
	}

	enrichStats, err := s.db.GetEnrichmentStats()
	if err != nil {
		s.logger.Error("failed to get enrichment stats", "error", err)
		enrichStats = &database.EnrichmentStats{}
	}

	data := AnalyticsData{
		Layout:          s.layoutFor(r),
		EnrichmentStats: enrichmentStatsView(enrichStats),
	}
	data.StatsFailed = statsFailed
	data.StatsStale = statsFailed && len(ecosystems) > 0
	if data.StatsStale {
		data.StatsAge = formatTimeAgo(s.ecoStats.SnapshotAt())
	}
	data.Totals, data.Ecosystems = analyticsView(ecosystems)
	data.Donut = donutView(ecosystems, data.Totals.DownloadedBytes, data.Totals.Downloaded)

	// Process-lifetime counters come from the Prometheus registry rather than
	// the database; a failure here must not take the page down with it.
	snap, err := metrics.Gather()
	if err != nil {
		s.logger.Error("failed to gather runtime metrics", "error", err)
	} else {
		data.Runtime = runtimeView(snap)
		data.Runtime.Sources = s.sources.Top(analyticsTopSources)
		data.Runtime.SourceCount = s.sources.Count()
		data.Runtime.TrustsForward = s.trustsForwardedFor()
	}

	if err := s.templates.Render(w, "analytics", data); err != nil {
		s.logger.Error("failed to render analytics", "error", err)
	}
}

// analyticsView turns per-ecosystem database rows into the rendered totals and
// table rows, preserving the order they arrive in.
func analyticsView(stats []database.EcosystemStats) (AnalyticsTotals, []EcosystemRow) {
	var totals AnalyticsTotals
	var downloads, cacheSize, artifacts, packages, versions int64

	for _, e := range stats {
		totals.DownloadedBytes += e.DownloadedBytes
		downloads += e.Downloads
		cacheSize += e.CacheSize
		artifacts += e.Artifacts
		packages += e.Packages
		versions += e.Versions
		if e.DownloadedBytes > 0 {
			totals.ActiveEcosystems++
		}
	}

	totals.Downloaded = formatSize(totals.DownloadedBytes)
	totals.Downloads = formatCount(downloads)
	totals.CacheSize = formatSize(cacheSize)
	totals.CachedArtifacts = formatCount(artifacts)
	totals.Packages = formatCount(packages)
	totals.Versions = formatCount(versions)
	totals.Ecosystems = len(stats)
	totals.Amplification = formatRatio(totals.DownloadedBytes, cacheSize)

	rows := make([]EcosystemRow, 0, len(stats))
	for _, e := range stats {
		rows = append(rows, EcosystemRow{
			Ecosystem:       e.Ecosystem,
			DownloadedBytes: e.DownloadedBytes,
			Downloaded:      formatSize(e.DownloadedBytes),
			Downloads:       formatCount(e.Downloads),
			CacheSize:       formatSize(e.CacheSize),
			AvgArtifactSize: avgArtifactSize(e.CacheSize, e.Artifacts),
			Artifacts:       formatCount(e.Artifacts),
			Packages:        formatCount(e.Packages),
			Versions:        formatCount(e.Versions),
			SharePct:        formatPercent(e.DownloadedBytes, totals.DownloadedBytes),
		})
	}
	return totals, rows
}
