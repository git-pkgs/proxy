package database

import (
	"database/sql"
	"testing"
)

func TestGetMetadataCacheStats(t *testing.T) {
	db := newTestDB(t)

	stats, err := db.GetMetadataCacheStats()
	if err != nil {
		t.Fatalf("GetMetadataCacheStats on an empty cache: %v", err)
	}
	if len(stats) != 0 {
		t.Fatalf("empty cache returned %+v, want no rows", stats)
	}

	rows := []struct {
		ecosystem, name string
		size            sql.NullInt64
	}{
		{"npm", "lodash", sql.NullInt64{Int64: 300, Valid: true}},
		{"npm", "react", sql.NullInt64{Int64: 200, Valid: true}},
		// gem and rubygems are one ecosystem and must share a row.
		{"gem", "rails", sql.NullInt64{Int64: 1000, Valid: true}},
		{"rubygems", "rack", sql.NullInt64{Int64: 24, Valid: true}},
		// A row written before sizes were recorded still counts as an entry.
		{"pypi", "requests", sql.NullInt64{}},
	}
	for _, r := range rows {
		if err := db.UpsertMetadataCache(&MetadataCacheEntry{
			Ecosystem:   r.ecosystem,
			Name:        r.name,
			StoragePath: "_metadata/" + r.ecosystem + "/" + r.name,
			Size:        r.size,
		}); err != nil {
			t.Fatalf("UpsertMetadataCache(%s/%s): %v", r.ecosystem, r.name, err)
		}
	}

	stats, err = db.GetMetadataCacheStats()
	if err != nil {
		t.Fatalf("GetMetadataCacheStats: %v", err)
	}

	want := []MetadataCacheStats{
		{Ecosystem: "rubygems", Entries: 2, Size: 1024},
		{Ecosystem: "npm", Entries: 2, Size: 500},
		{Ecosystem: "pypi", Entries: 1, Size: 0},
	}
	if len(stats) != len(want) {
		t.Fatalf("got %+v, want %+v", stats, want)
	}
	for i := range want {
		if stats[i] != want[i] {
			t.Errorf("row %d = %+v, want %+v", i, stats[i], want[i])
		}
	}
}
