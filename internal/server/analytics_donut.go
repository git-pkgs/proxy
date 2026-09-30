package server

import (
	"math"
	"strconv"

	"github.com/git-pkgs/proxy/internal/database"
)

// Donut geometry, in SVG user units. The ring is drawn as a dashed stroke on a
// circle rather than as arc paths: one dash per slice, which makes the 2px
// surface gap between slices fall out of the dash arithmetic.
const (
	donutSize   = 260.0
	donutRadius = 104.0
	donutStroke = 30.0
	// donutGap is the surface-coloured separator between touching slices.
	donutGap = 2.0
	// donutMinSlice keeps a slice that rounds to nothing from vanishing
	// entirely; the legend and table carry its real value.
	donutMinSlice      = 1.5
	donutCircumference = 2 * math.Pi * donutRadius //nolint:mnd // circumference
)

// donutSlices is the most slices the ring will draw. Part-to-whole reads at a
// glance only while the segment count stays small, so past this the tail is
// folded into a single "Other" slice and the full detail lives in the table
// below the chart.
const donutSlices = 6

const otherSliceLabel = "Other"

// DonutSlice is one arc of the ring plus its legend row.
type DonutSlice struct {
	// Index selects the fixed categorical slot. The hues themselves live in
	// the .donut-slot-N and .swatch-N rules in the analytics template, so the
	// validated palette has exactly one definition; slots are assigned here in
	// sequence and never cycled.
	Index     int
	Label     string
	Value     string
	SharePct  string
	Downloads string
	CacheSize string
	Dash      string
	Gap       string
	Offset    string
	// IsOther marks the folded tail, which has no single ecosystem behind it.
	IsOther bool
	// Members lists what was folded in, for the tooltip.
	Members string
}

// DonutView is the whole chart: the ring, its centre figure and its legend.
type DonutView struct {
	Slices []DonutSlice
	// Size, Radius, Stroke and Center are handed to the template so the SVG
	// geometry has exactly one definition.
	Size   string
	Radius string
	Stroke string
	Center string
	// CenterValue is the accumulated download size across every ecosystem,
	// stated in the middle of the ring.
	CenterValue string
	CenterLabel string
	HasSlices   bool
}

// donutView folds the per-ecosystem rows into at most donutSlices arcs and
// computes the dash geometry for each.
func donutView(stats []database.EcosystemStats, totalBytes int64, centerValue string) DonutView {
	view := DonutView{
		Size:        trimFloat(donutSize),
		Radius:      trimFloat(donutRadius),
		Stroke:      trimFloat(donutStroke),
		Center:      trimFloat(donutSize / 2), //nolint:mnd // the centre of the viewBox
		CenterValue: centerValue,
		CenterLabel: "accumulated download size",
	}

	// Only ecosystems that have actually served bytes get an arc; an
	// ecosystem at zero would be an invisible slice with a legend entry
	// claiming a share it does not have.
	active := make([]database.EcosystemStats, 0, len(stats))
	for _, e := range stats {
		if e.DownloadedBytes > 0 {
			active = append(active, e)
		}
	}
	if len(active) == 0 || totalBytes <= 0 {
		return view
	}

	head, tail := active, []database.EcosystemStats(nil)
	if len(active) > donutSlices {
		head, tail = active[:donutSlices-1], active[donutSlices-1:]
	}

	slices := make([]DonutSlice, 0, donutSlices)
	for i, e := range head {
		slices = append(slices, DonutSlice{
			Index:     i,
			Label:     ecosystemBadgeLabel(e.Ecosystem),
			Value:     formatSize(e.DownloadedBytes),
			SharePct:  formatPercent(e.DownloadedBytes, totalBytes),
			Downloads: formatCount(e.Downloads),
			CacheSize: formatSize(e.CacheSize),
		})
	}

	if len(tail) > 0 {
		var bytes, downloads, cacheSize int64
		members := ""
		for i, e := range tail {
			bytes += e.DownloadedBytes
			downloads += e.Downloads
			cacheSize += e.CacheSize
			if i > 0 {
				members += ", "
			}
			members += ecosystemBadgeLabel(e.Ecosystem)
		}
		slices = append(slices, DonutSlice{
			Index:     donutSlices - 1,
			Label:     otherSliceLabel,
			Value:     formatSize(bytes),
			SharePct:  formatPercent(bytes, totalBytes),
			Downloads: formatCount(downloads),
			CacheSize: formatSize(cacheSize),
			IsOther:   true,
			Members:   members,
		})
	}

	applyDonutGeometry(slices, active, tail, totalBytes)
	view.Slices = slices
	view.HasSlices = true
	return view
}

// applyDonutGeometry sets each slice's dash length and offset.
func applyDonutGeometry(slices []DonutSlice, active, tail []database.EcosystemStats, totalBytes int64) {
	// Recover each slice's byte value in the same order the slices were built,
	// so the geometry is driven by the numbers rather than by the rendered text.
	values := make([]int64, 0, len(slices))
	head := active
	if len(tail) > 0 {
		head = active[:len(active)-len(tail)]
	}
	for _, e := range head {
		values = append(values, e.DownloadedBytes)
	}
	if len(tail) > 0 {
		var sum int64
		for _, e := range tail {
			sum += e.DownloadedBytes
		}
		values = append(values, sum)
	}

	dashes := make([]float64, len(slices))
	widest := 0
	for i := range slices {
		arc := donutCircumference * float64(values[i]) / float64(totalBytes)

		dashes[i] = arc - donutGap
		if dashes[i] < donutMinSlice {
			dashes[i] = donutMinSlice
		}
		if dashes[i] > dashes[widest] {
			widest = i
		}
	}

	// Widening a sliver to donutMinSlice buys visibility with room the ring
	// does not have, and the overshoot would wrap the last slice back over the
	// first. The widest slice gives the space back: it is the only one that can
	// lose a couple of units without becoming unreadable, and at six slices the
	// most that can be owed is donutSlices*(donutMinSlice+donutGap), far less
	// than the circumference.
	var needed float64
	for _, d := range dashes {
		needed += d + donutGap
	}
	if excess := needed - donutCircumference; excess > 0 {
		dashes[widest] = math.Max(donutMinSlice, dashes[widest]-excess)
	}

	var consumed float64
	for i := range slices {
		slices[i].Dash = trimFloat(dashes[i])
		slices[i].Gap = trimFloat(donutCircumference - dashes[i])
		// A dashoffset runs backwards around the circle, so the running total
		// is negated to lay slices out clockwise from twelve o'clock.
		slices[i].Offset = trimFloat(-consumed)

		// Advance by what is drawn: advancing by the smaller true arc would
		// start the next slice underneath a widened one, and the later colour
		// would win.
		consumed += dashes[i] + donutGap
	}
}

// trimFloat renders an SVG coordinate without trailing zeroes.
func trimFloat(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}
