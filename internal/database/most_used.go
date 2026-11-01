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
}

// MostUsedPackages returns, for each ecosystem, the top packages with the most
// downloads among the cached files downloaded since cutoff, ordered by
// ecosystem and then by hits. Each lists every file cached for it. Files
// from local sources, which have no upstream, are left out.
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
			  AND COALESCE(a.last_accessed_at, a.fetched_at) >= ?
			GROUP BY p.ecosystem, p.name, p.purl
		) ranked
		WHERE rank <= ?
		ORDER BY ecosystem, hits DESC, name
	`), SourceUpstream, cutoff.UTC(), top)
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
		}
		err := db.Select(&files, db.Rebind(`
			SELECT a.version_purl, a.filename, a.upstream_url
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
			})
		}
	}
	return packages, nil
}
