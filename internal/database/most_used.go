package database

import (
	"time"
)

// UsedPackage is a package clients download from the cache, with the files
// cached for it.
type UsedPackage struct {
	Ecosystem string
	Name      string
	PURL      string
	// Hits counts the downloads of the files downloaded since the cutoff.
	Hits  int64
	Files []UsedFile
}

// UsedFile is a cached file of a UsedPackage.
type UsedFile struct {
	Version     string
	Filename    string
	UpstreamURL string
	// Downloaded is false for a file a mirror run fetched that no client
	// has downloaded since.
	Downloaded bool
}

// MostUsedPackages returns, for each ecosystem, the top packages with the most
// downloads among the cached files downloaded since cutoff, ordered by
// ecosystem and then by hits. Files a mirror run fetched that no client has
// downloaded since don't count toward the ranking. Each package lists every
// file cached for it, those included, and marks the ones clients downloaded.
// Files from local sources, which have no upstream, are left out.
func (db *DB) MostUsedPackages(cutoff time.Time, top int) ([]UsedPackage, error) {
	var rows []struct {
		Ecosystem string `db:"ecosystem"`
		Name      string `db:"name"`
		PURL      string `db:"purl"`
		Hits      int64  `db:"hits"`
	}
	err := db.Select(&rows, db.Rebind(`
		SELECT ecosystem, name, purl, hits FROM (
			SELECT p.ecosystem, p.name, p.purl, SUM(a.hit_count) AS hits,
			       ROW_NUMBER() OVER (PARTITION BY p.ecosystem ORDER BY SUM(a.hit_count) DESC, p.name) AS rank
			FROM artifacts a
			JOIN versions v ON v.purl = a.version_purl
			JOIN packages p ON p.purl = v.package_purl
			WHERE a.storage_path IS NOT NULL AND a.source = ?
			  AND (a.last_accessed_at >= ? OR (a.last_accessed_at IS NULL AND a.fetched_at >= ?
			       AND (a.mirrored_at IS NULL OR a.mirrored_at < a.fetched_at)))
			GROUP BY p.ecosystem, p.name, p.purl
		) ranked
		WHERE rank <= ?
		ORDER BY ecosystem, hits DESC, name
	`), SourceUpstream, cutoff.UTC(), cutoff.UTC(), top)
	if err != nil {
		return nil, err
	}
	packages := make([]UsedPackage, len(rows))
	for i, row := range rows {
		packages[i] = UsedPackage{Ecosystem: row.Ecosystem, Name: row.Name, PURL: row.PURL, Hits: row.Hits}
		var files []struct {
			VersionPURL string `db:"version_purl"`
			Filename    string `db:"filename"`
			UpstreamURL string `db:"upstream_url"`
			Downloaded  int    `db:"downloaded"`
		}
		err := db.Select(&files, db.Rebind(`
			SELECT a.version_purl, a.filename, a.upstream_url,
			       CASE WHEN a.last_accessed_at IS NOT NULL OR a.mirrored_at IS NULL OR a.mirrored_at < a.fetched_at
			            THEN 1 ELSE 0 END AS downloaded
			FROM artifacts a
			JOIN versions v ON v.purl = a.version_purl
			WHERE v.package_purl = ? AND a.storage_path IS NOT NULL AND a.source = ?
			ORDER BY a.version_purl, a.filename
		`), row.PURL, SourceUpstream)
		if err != nil {
			return nil, err
		}
		for _, f := range files {
			packages[i].Files = append(packages[i].Files, UsedFile{
				Version: VersionFromPURL(f.VersionPURL), Filename: f.Filename, UpstreamURL: f.UpstreamURL,
				Downloaded: f.Downloaded == 1,
			})
		}
	}
	return packages, nil
}
