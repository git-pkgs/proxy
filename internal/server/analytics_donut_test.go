package server

import (
	"math"
	"strconv"
	"testing"

	"github.com/git-pkgs/proxy/internal/database"
)

func eco(name string, bytes int64) database.EcosystemStats {
	return database.EcosystemStats{Ecosystem: name, DownloadedBytes: bytes, Downloads: 1, CacheSize: bytes / 2}
}

func TestDonutViewFoldsTailIntoOther(t *testing.T) {
	// Nine active ecosystems: five keep their own slice, four fold into "Other".
	stats := []database.EcosystemStats{
		eco("npm", 900), eco("pypi", 800), eco("maven", 700), eco("cargo", 600),
		eco("gem", 500), eco("golang", 40), eco("nuget", 30), eco("deb", 20), eco("conda", 10),
	}
	var total int64
	for _, e := range stats {
		total += e.DownloadedBytes
	}

	view := donutView(stats, total, formatSize(total))

	if len(view.Slices) != donutSlices {
		t.Fatalf("expected %d slices, got %d", donutSlices, len(view.Slices))
	}

	last := view.Slices[donutSlices-1]
	if !last.IsOther {
		t.Fatalf("expected the final slice to be the folded tail, got %+v", last)
	}
	// 40 + 30 + 20 + 10 = 100
	if last.Value != formatSize(100) {
		t.Errorf("Other value = %q, want %q", last.Value, formatSize(100))
	}
	if last.Members != "golang, nuget, debian, conda" {
		t.Errorf("Other members = %q, want the four folded ecosystems by display name", last.Members)
	}
	// "Other" must take the last slot, never one of a real ecosystem's.
	if last.Index != donutSlices-1 {
		t.Errorf("Other took slot %d, want %d", last.Index, donutSlices-1)
	}
	for i, s := range view.Slices[:donutSlices-1] {
		if s.Index != i {
			t.Errorf("slice %d took slot %d; slots must be assigned in sequence", i, s.Index)
		}
	}
}

func TestDonutViewKeepsEveryEcosystemWhenFewEnough(t *testing.T) {
	stats := []database.EcosystemStats{eco("npm", 600), eco("pypi", 400)}
	view := donutView(stats, 1000, "1000 B")

	if len(view.Slices) != 2 {
		t.Fatalf("expected 2 slices, got %d", len(view.Slices))
	}
	for _, s := range view.Slices {
		if s.IsOther {
			t.Errorf("nothing should be folded with only 2 ecosystems, got %+v", s)
		}
	}
	if view.Slices[0].SharePct != "60.0" || view.Slices[1].SharePct != "40.0" {
		t.Errorf("shares = %q / %q, want 60.0 / 40.0", view.Slices[0].SharePct, view.Slices[1].SharePct)
	}
}

// The ring must account for the whole circle: the arcs, plus one gap each,
// have to add back up to the circumference.
func TestDonutGeometryCoversTheCircle(t *testing.T) {
	stats := []database.EcosystemStats{eco("npm", 500), eco("pypi", 300), eco("maven", 200)}
	view := donutView(stats, 1000, "1000 B")

	circumference := 2 * math.Pi * donutRadius

	var covered float64
	for _, s := range view.Slices {
		dash, err := strconv.ParseFloat(s.Dash, 64)
		if err != nil {
			t.Fatalf("dash %q: %v", s.Dash, err)
		}
		gap, err := strconv.ParseFloat(s.Gap, 64)
		if err != nil {
			t.Fatalf("gap %q: %v", s.Gap, err)
		}
		// Each slice is drawn as one dash followed by a gap spanning the
		// rest of the circle, so the pair always sums to the circumference.
		if math.Abs(dash+gap-circumference) > 0.01 {
			t.Errorf("slice %q: dash+gap = %v, want the circumference %v", s.Label, dash+gap, circumference)
		}
		covered += dash + donutGap
	}

	if math.Abs(covered-circumference) > 0.01 {
		t.Errorf("arcs plus gaps cover %v, want the full circumference %v", covered, circumference)
	}
}

// Offsets must advance monotonically so slices sit end to end instead of
// stacking on top of each other.
func TestDonutOffsetsAdvanceInOrder(t *testing.T) {
	stats := []database.EcosystemStats{eco("npm", 500), eco("pypi", 300), eco("maven", 200)}
	view := donutView(stats, 1000, "1000 B")

	circumference := 2 * math.Pi * donutRadius
	want := []float64{0, -circumference * 0.5, -circumference * 0.8}

	for i, s := range view.Slices {
		got, err := strconv.ParseFloat(s.Offset, 64)
		if err != nil {
			t.Fatalf("offset %q: %v", s.Offset, err)
		}
		if math.Abs(got-want[i]) > 0.01 {
			t.Errorf("slice %d offset = %v, want %v", i, got, want[i])
		}
	}
}

// A slice too small to render still has to be visible; the legend and table
// carry its real value.
func TestDonutTinySliceStaysVisible(t *testing.T) {
	stats := []database.EcosystemStats{eco("npm", 10_000_000), eco("composer", 1)}
	view := donutView(stats, 10_000_001, "9.5 MB")

	tiny := view.Slices[1]
	dash, err := strconv.ParseFloat(tiny.Dash, 64)
	if err != nil {
		t.Fatalf("dash %q: %v", tiny.Dash, err)
	}
	if dash < donutMinSlice {
		t.Errorf("tiny slice dash = %v, want at least %v so it stays visible", dash, donutMinSlice)
	}
	if tiny.SharePct != "<0.1" {
		t.Errorf("tiny slice share = %q, want %q", tiny.SharePct, "<0.1")
	}
}

func TestDonutViewEmpty(t *testing.T) {
	if view := donutView(nil, 0, "0 B"); view.HasSlices || len(view.Slices) != 0 {
		t.Errorf("expected an empty ring, got %+v", view)
	}

	// Ecosystems that exist but have served nothing get no arc at all.
	idle := []database.EcosystemStats{{Ecosystem: "rpm", Packages: 3}}
	if view := donutView(idle, 0, "0 B"); view.HasSlices {
		t.Errorf("an ecosystem with no traffic must not get a slice, got %+v", view.Slices)
	}
}

// A slice widened to stay visible must not be drawn over by its successor:
// the next offset has to start where this slice actually ends.
func TestDonutClampedSliceDoesNotOverdrawItsSuccessor(t *testing.T) {
	// Four slivers far below the minimum, followed by a large slice.
	stats := []database.EcosystemStats{
		eco("npm", 10_000_000), eco("a", 1), eco("b", 1), eco("c", 1), eco("d", 1),
	}
	view := donutView(stats, 10_000_004, "9.5 MB")

	for i := 0; i < len(view.Slices)-1; i++ {
		dash := mustFloat(t, view.Slices[i].Dash)
		start := -mustFloat(t, view.Slices[i].Offset)
		nextStart := -mustFloat(t, view.Slices[i+1].Offset)

		end := start + dash
		if nextStart < end-0.001 {
			t.Errorf("slice %d (%q) is drawn to %v but slice %d starts at %v: they overlap",
				i, view.Slices[i].Label, end, i+1, nextStart)
		}
	}
}

func mustFloat(t *testing.T, s string) float64 {
	t.Helper()
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		t.Fatalf("parsing %q: %v", s, err)
	}
	return f
}

// Widening slivers must not push the ring past a full turn: the last slice
// would wrap back over the first and repaint the leading ecosystem's arc.
func TestDonutNeverExceedsOneTurn(t *testing.T) {
	// One dominant ecosystem and five far below the visible minimum.
	stats := []database.EcosystemStats{
		eco("npm", 100_000_000),
		eco("a", 1), eco("b", 1), eco("c", 1), eco("d", 1), eco("e", 1),
	}
	view := donutView(stats, 100_000_005, "95.4 MB")

	if len(view.Slices) != 6 {
		t.Fatalf("expected 6 slices, got %d", len(view.Slices))
	}

	last := view.Slices[len(view.Slices)-1]
	end := -mustFloat(t, last.Offset) + mustFloat(t, last.Dash)
	if end > donutCircumference+0.001 {
		t.Errorf("ring is drawn to %v, past the circumference %v: the last slice "+
			"wraps over the first", end, donutCircumference)
	}

	// Every sliver still has to be visible.
	for _, s := range view.Slices[1:] {
		if dash := mustFloat(t, s.Dash); dash < donutMinSlice {
			t.Errorf("slice %q dash = %v, want at least %v", s.Label, dash, donutMinSlice)
		}
	}
}
