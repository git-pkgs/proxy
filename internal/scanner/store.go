package scanner

import (
	"database/sql"
	"encoding/json"
	"strings"
	"time"

	"github.com/git-pkgs/proxy/internal/database"
)

// PersistDecision writes findings and a scan summary row for an artifact.
// The findings table is fully replaced for the artifact (so re-scanning
// cannot leave stale rows), and one artifact_scans row is recorded per
// scanner that contributed to the decision.
func PersistDecision(db *database.DB, artifactID int64, versionPURL, contentHash string, d Decision) error {
	if db == nil {
		return nil
	}
	if err := db.ClearArtifactFindings(artifactID); err != nil {
		return err
	}
	now := time.Now().UTC()
	scanners := make(map[string]bool)
	for _, f := range d.Findings {
		scanners[f.Scanner] = true
		row := &database.ArtifactFinding{
			ArtifactID:   artifactID,
			VersionPURL:  versionPURL,
			ContentHash:  contentHash,
			Scanner:      f.Scanner,
			FindingID:    f.ID,
			Severity:     f.Severity.String(),
			Summary:      nullString(f.Summary),
			FixedVersion: nullString(f.FixedVersion),
			References:   nullString(joinReferences(f.References)),
			Raw:          nullString(f.Raw),
			ScannedAt:    now,
		}
		if err := db.UpsertArtifactFinding(row); err != nil {
			return err
		}
	}
	// For every scanner that contributed, record an "ok" scan row so the
	// cache treats this content_hash as freshly scanned. The pipeline's
	// per-run Cache.Record may have already done this for scanners that
	// produced findings; UpsertArtifactScan is idempotent.
	for name := range scanners {
		_ = db.UpsertArtifactScan(&database.ArtifactScan{
			ContentHash: contentHash,
			Scanner:     name,
			Status:      "ok",
			ScannedAt:   now,
		})
	}
	return nil
}

func nullString(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

// joinReferences encodes references as a JSON array. We pick JSON over
// newline-separated values so consumers can tell empty from absent and
// don't need to guess about whitespace inside URLs.
func joinReferences(refs []string) string {
	if len(refs) == 0 {
		return ""
	}
	b, err := json.Marshal(refs)
	if err != nil {
		return strings.Join(refs, "\n")
	}
	return string(b)
}

// splitReferences is the inverse of joinReferences.
func splitReferences(s string) []string {
	if s == "" {
		return nil
	}
	var refs []string
	if err := json.Unmarshal([]byte(s), &refs); err == nil {
		return refs
	}
	return strings.Split(s, "\n")
}
