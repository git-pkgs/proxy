package server

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/git-pkgs/proxy/internal/database"
)

func TestFormatCount(t *testing.T) {
	tests := []struct {
		in   int64
		want string
	}{
		{0, "0"},
		{7, "7"},
		{100, "100"},
		{1000, "1,000"},
		{12004, "12,004"},
		{1000000, "1,000,000"},
		{-4200, "-4,200"},
	}
	for _, tc := range tests {
		if got := formatCount(tc.in); got != tc.want {
			t.Errorf("formatCount(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestPercentOf(t *testing.T) {
	tests := []struct {
		part, whole int64
		want        string
	}{
		{1, 4, "25.0"},
		{2, 3, "66.7"},
		{0, 100, "0.0"},
		{5, 5, "100.0"},
		// A zero total must not divide by zero.
		{0, 0, "0.0"},
		{7, 0, "0.0"},
		// A negligible share still rounds to a plain number here; only
		// formatPercent turns it into "<0.1".
		{1, 1000000, "0.0"},
	}
	for _, tc := range tests {
		if got := percentOf(tc.part, tc.whole); got != tc.want {
			t.Errorf("percentOf(%d, %d) = %q, want %q", tc.part, tc.whole, got, tc.want)
		}
	}
}

func TestFormatPercent(t *testing.T) {
	tests := []struct {
		part, whole int64
		want        string
	}{
		{1, 4, "25.0"},
		{0, 100, "0.0"},
		{0, 0, "0.0"},
		// A real but tiny share must not be reported as zero.
		{1, 1000000, "<0.1"},
		{7_800_000, 18_000_000_000, "<0.1"},
	}
	for _, tc := range tests {
		if got := formatPercent(tc.part, tc.whole); got != tc.want {
			t.Errorf("formatPercent(%d, %d) = %q, want %q", tc.part, tc.whole, got, tc.want)
		}
	}
}

func TestFormatRatio(t *testing.T) {
	if got := formatRatio(3500, 1000); got != "3.5x" {
		t.Errorf("formatRatio(3500, 1000) = %q, want %q", got, "3.5x")
	}
	// Nothing cached means no meaningful multiplier, not "+Infx".
	if got := formatRatio(3500, 0); got != "" {
		t.Errorf("formatRatio(3500, 0) = %q, want empty", got)
	}
}

func TestAvgArtifactSize(t *testing.T) {
	if got := avgArtifactSize(1000, 4); got != "250 B" {
		t.Errorf("avgArtifactSize(1000, 4) = %q, want %q", got, "250 B")
	}
	if got := avgArtifactSize(0, 0); got != "—" {
		t.Errorf("avgArtifactSize(0, 0) = %q, want an em dash", got)
	}
}

func TestAnalyticsView(t *testing.T) {
	stats := []database.EcosystemStats{
		{Ecosystem: "npm", Packages: 2, Versions: 2, Artifacts: 2, CacheSize: 1500, Downloads: 4, DownloadedBytes: 3000},
		{Ecosystem: "cargo", Packages: 1, Versions: 1, Artifacts: 1, CacheSize: 200, Downloads: 5, DownloadedBytes: 1000},
		{Ecosystem: "gem", Packages: 3, Versions: 4},
	}

	totals, rows := analyticsView(stats)

	if totals.DownloadedBytes != 4000 {
		t.Errorf("DownloadedBytes = %d, want 4000", totals.DownloadedBytes)
	}
	if totals.Downloads != "9" {
		t.Errorf("Downloads = %q, want %q", totals.Downloads, "9")
	}
	if totals.CachedArtifacts != "3" {
		t.Errorf("CachedArtifacts = %q, want %q", totals.CachedArtifacts, "3")
	}
	if totals.Packages != "6" {
		t.Errorf("Packages = %q, want %q", totals.Packages, "6")
	}
	if totals.Versions != "7" {
		t.Errorf("Versions = %q, want %q", totals.Versions, "7")
	}
	if totals.Ecosystems != 3 {
		t.Errorf("Ecosystems = %d, want 3", totals.Ecosystems)
	}
	// gem has served nothing, so it is known but not active.
	if totals.ActiveEcosystems != 2 {
		t.Errorf("ActiveEcosystems = %d, want 2", totals.ActiveEcosystems)
	}
	// 4000 bytes served from 1700 bytes stored.
	if totals.Amplification != "2.4x" {
		t.Errorf("Amplification = %q, want %q", totals.Amplification, "2.4x")
	}

	if len(rows) != 3 {
		t.Fatalf("expected 3 rows, got %d", len(rows))
	}

	// Shares are of the grand total.
	if rows[0].SharePct != "75.0" {
		t.Errorf("npm share = %q, want 75.0", rows[0].SharePct)
	}
	if rows[1].SharePct != "25.0" {
		t.Errorf("cargo share = %q, want 25.0", rows[1].SharePct)
	}
	// gem served nothing, so it contributes no share but still gets a row.
	if rows[2].SharePct != "0.0" || rows[2].Ecosystem != "gem" {
		t.Errorf("gem row = %+v, want a 0.0%% share", rows[2])
	}
	if rows[0].AvgArtifactSize != "750 B" {
		t.Errorf("npm AvgArtifactSize = %q, want %q", rows[0].AvgArtifactSize, "750 B")
	}
}

// With nothing cached at all the view must stay renderable rather than dividing
// by a zero total.
func TestAnalyticsViewEmpty(t *testing.T) {
	totals, rows := analyticsView(nil)

	if len(rows) != 0 {
		t.Errorf("expected no rows, got %+v", rows)
	}
	if totals.DownloadedBytes != 0 {
		t.Errorf("DownloadedBytes = %d, want 0", totals.DownloadedBytes)
	}
	if totals.Downloaded != "0 B" {
		t.Errorf("Downloaded = %q, want %q", totals.Downloaded, "0 B")
	}
	if totals.Amplification != "" {
		t.Errorf("Amplification = %q, want empty", totals.Amplification)
	}
}

func TestAnalyticsPageRendersTotals(t *testing.T) {
	tpl := &Templates{}
	w := httptest.NewRecorder()

	totals, rows := analyticsView([]database.EcosystemStats{
		{Ecosystem: "npm", Packages: 1, Versions: 1, Artifacts: 1, CacheSize: 1000, Downloads: 3, DownloadedBytes: 3000},
	})
	data := AnalyticsData{Totals: totals, Ecosystems: rows}
	data.Donut = donutView([]database.EcosystemStats{
		{Ecosystem: "npm", Packages: 1, Versions: 1, Artifacts: 1, CacheSize: 1000, Downloads: 3, DownloadedBytes: 3000},
	}, totals.DownloadedBytes, totals.Downloaded)

	if err := tpl.Render(w, "analytics", data); err != nil {
		t.Fatalf("Render: %v", err)
	}

	body := w.Body.String()
	if strings.Contains(body, "ZgotmplZ") {
		t.Error("a template value was sanitized away; check the donut dash geometry")
	}
	for _, want := range []string{
		"accumulated download size",
		"2.9 KB",        // the total, stated in the middle of the ring
		"donut-slot-0",  // the single slice takes the first categorical slot
		"3.0x",          // 3000 bytes served from 1000 stored
		"/ui/analytics", // nav link renders on the page itself
	} {
		if !strings.Contains(body, want) {
			t.Errorf("rendered page missing %q", want)
		}
	}
}

// A retained snapshot must be labelled as one. The cache deliberately serves
// the last good rows when the query fails, so without this the page presents
// arbitrarily old figures as current and the only trace is a log line.
func TestAnalyticsPageFlagsStaleFigures(t *testing.T) {
	tpl := &Templates{}
	w := httptest.NewRecorder()

	totals, rows := analyticsView([]database.EcosystemStats{
		{Ecosystem: "npm", Artifacts: 1, CacheSize: 1000, Downloads: 3, DownloadedBytes: 3000},
	})
	data := AnalyticsData{
		Totals:      totals,
		Ecosystems:  rows,
		StatsFailed: true,
		StatsStale:  true,
		StatsAge:    "2 hours ago",
	}

	if err := tpl.Render(w, "analytics", data); err != nil {
		t.Fatalf("Render: %v", err)
	}

	body := w.Body.String()
	for _, want := range []string{"Figures may be out of date", "2 hours ago"} {
		if !strings.Contains(body, want) {
			t.Errorf("rendered page missing %q", want)
		}
	}
	// The figures themselves still render — stale beats absent.
	if !strings.Contains(body, "2.9 KB") {
		t.Error("the retained snapshot was not rendered alongside the warning")
	}
}

// A failure with nothing retained keeps the existing unavailable message and
// must not also claim the figures below are stale, since there are none.
func TestAnalyticsPageStaleBannerNeedsASnapshot(t *testing.T) {
	tpl := &Templates{}
	w := httptest.NewRecorder()

	if err := tpl.Render(w, "analytics", AnalyticsData{StatsFailed: true}); err != nil {
		t.Fatalf("Render: %v", err)
	}

	body := w.Body.String()
	if strings.Contains(body, "Figures may be out of date") {
		t.Error("stale banner rendered with no retained snapshot")
	}
	if !strings.Contains(body, "the database query failed") {
		t.Error("expected the unavailable message when the query failed with no snapshot")
	}
}

func TestAnalyticsPageRendersWithoutTraffic(t *testing.T) {
	tpl := &Templates{}
	w := httptest.NewRecorder()

	if err := tpl.Render(w, "analytics", AnalyticsData{}); err != nil {
		t.Fatalf("Render: %v", err)
	}
	if body := w.Body.String(); !strings.Contains(body, "Nothing has been served from cache yet") {
		t.Error("expected the empty-state message when no traffic has been recorded")
	}
}

func TestEcosystemMetricsMapping(t *testing.T) {
	got := ecosystemMetrics([]database.EcosystemStats{
		{Ecosystem: "npm", Packages: 1, Versions: 2, Artifacts: 3, CacheSize: 4, Downloads: 5, DownloadedBytes: 6},
	})
	if len(got) != 1 {
		t.Fatalf("expected 1 snapshot, got %d", len(got))
	}
	m := got[0]
	if m.Ecosystem != "npm" || m.Packages != 1 || m.Versions != 2 || m.Artifacts != 3 ||
		m.CacheSize != 4 || m.Downloads != 5 || m.DownloadedBytes != 6 {
		t.Errorf("fields did not map across: %+v", m)
	}
}
