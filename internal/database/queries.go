package database

import (
	"database/sql"
	"fmt"
	"time"
)

// Package queries

func (db *DB) GetPackageByPURL(purl string) (*Package, error) {
	var pkg Package
	query := db.Rebind(`
		SELECT id, purl, ecosystem, name, latest_version, license,
		       description, homepage, repository_url, registry_url,
		       supplier_name, supplier_type, source, enriched_at,
		       vulns_synced_at, created_at, updated_at
		FROM packages WHERE purl = ?
	`)
	err := db.Get(&pkg, query, purl)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &pkg, nil
}

func (db *DB) GetPackageByEcosystemName(ecosystem, name string) (*Package, error) {
	var pkg Package
	query := db.Rebind(`
		SELECT id, purl, ecosystem, name, latest_version, license,
		       description, homepage, repository_url, registry_url,
		       supplier_name, supplier_type, source, enriched_at,
		       vulns_synced_at, created_at, updated_at
		FROM packages WHERE ecosystem = ? AND name = ?
	`)
	err := db.Get(&pkg, query, ecosystem, name)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &pkg, nil
}

func (db *DB) UpsertPackage(pkg *Package) error {
	now := time.Now()
	var query string

	if db.dialect == DialectPostgres {
		query = `
			INSERT INTO packages (purl, ecosystem, name, latest_version, license,
			                      description, homepage, repository_url, registry_url,
			                      enriched_at, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
			ON CONFLICT(purl) DO UPDATE SET
				latest_version = EXCLUDED.latest_version,
				license = EXCLUDED.license,
				description = EXCLUDED.description,
				homepage = EXCLUDED.homepage,
				repository_url = EXCLUDED.repository_url,
				registry_url = EXCLUDED.registry_url,
				enriched_at = EXCLUDED.enriched_at,
				updated_at = EXCLUDED.updated_at
		`
	} else {
		query = `
			INSERT INTO packages (purl, ecosystem, name, latest_version, license,
			                      description, homepage, repository_url, registry_url,
			                      enriched_at, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(purl) DO UPDATE SET
				latest_version = excluded.latest_version,
				license = excluded.license,
				description = excluded.description,
				homepage = excluded.homepage,
				repository_url = excluded.repository_url,
				registry_url = excluded.registry_url,
				enriched_at = excluded.enriched_at,
				updated_at = excluded.updated_at
		`
	}

	_, err := db.Exec(query,
		pkg.PURL, pkg.Ecosystem, pkg.Name, pkg.LatestVersion,
		pkg.License, pkg.Description, pkg.Homepage, pkg.RepositoryURL,
		pkg.RegistryURL, pkg.EnrichedAt, now, now,
	)
	if err != nil {
		return fmt.Errorf("upserting package: %w", err)
	}
	return nil
}

// Version queries

func (db *DB) GetVersionByPURL(purl string) (*Version, error) {
	var v Version
	query := db.Rebind(`
		SELECT id, purl, package_purl, license, integrity, published_at,
		       yanked, source, enriched_at, created_at, updated_at
		FROM versions WHERE purl = ?
	`)
	err := db.Get(&v, query, purl)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &v, nil
}

func (db *DB) GetVersionsByPackagePURL(packagePURL string) ([]Version, error) {
	var versions []Version
	query := db.Rebind(`
		SELECT id, purl, package_purl, license, integrity, published_at,
		       yanked, source, enriched_at, created_at, updated_at
		FROM versions WHERE package_purl = ?
		ORDER BY created_at DESC
	`)
	err := db.Select(&versions, query, packagePURL)
	if err != nil {
		return nil, err
	}
	return versions, nil
}

func (db *DB) UpsertVersion(v *Version) error {
	now := time.Now()
	var query string

	if db.dialect == DialectPostgres {
		query = `
			INSERT INTO versions (purl, package_purl, license, integrity, published_at,
			                      yanked, enriched_at, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
			ON CONFLICT(purl) DO UPDATE SET
				license = EXCLUDED.license,
				integrity = EXCLUDED.integrity,
				published_at = EXCLUDED.published_at,
				yanked = EXCLUDED.yanked,
				enriched_at = EXCLUDED.enriched_at,
				updated_at = EXCLUDED.updated_at
		`
	} else {
		query = `
			INSERT INTO versions (purl, package_purl, license, integrity, published_at,
			                      yanked, enriched_at, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(purl) DO UPDATE SET
				license = excluded.license,
				integrity = excluded.integrity,
				published_at = excluded.published_at,
				yanked = excluded.yanked,
				enriched_at = excluded.enriched_at,
				updated_at = excluded.updated_at
		`
	}

	_, err := db.Exec(query,
		v.PURL, v.PackagePURL, v.License, v.Integrity,
		v.PublishedAt, v.Yanked, v.EnrichedAt, now, now,
	)
	if err != nil {
		return fmt.Errorf("upserting version: %w", err)
	}
	return nil
}

// Artifact queries

func (db *DB) GetArtifact(versionPURL, filename string) (*Artifact, error) {
	var a Artifact
	query := db.Rebind(`
		SELECT id, version_purl, filename, upstream_url, storage_path, content_hash,
		       size, content_type, fetched_at, hit_count, last_accessed_at,
		       created_at, updated_at
		FROM artifacts WHERE version_purl = ? AND filename = ?
	`)
	err := db.Get(&a, query, versionPURL, filename)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

func (db *DB) GetArtifactByPath(storagePath string) (*Artifact, error) {
	var a Artifact
	query := db.Rebind(`
		SELECT id, version_purl, filename, upstream_url, storage_path, content_hash,
		       size, content_type, fetched_at, hit_count, last_accessed_at,
		       created_at, updated_at
		FROM artifacts WHERE storage_path = ?
	`)
	err := db.Get(&a, query, storagePath)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

func (db *DB) UpsertArtifact(a *Artifact) error {
	now := time.Now()
	var query string

	if db.dialect == DialectPostgres {
		query = `
			INSERT INTO artifacts (version_purl, filename, upstream_url, storage_path, content_hash,
			                       size, content_type, fetched_at, hit_count, last_accessed_at,
			                       created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
			ON CONFLICT(version_purl, filename) DO UPDATE SET
				storage_path = EXCLUDED.storage_path,
				content_hash = EXCLUDED.content_hash,
				size = EXCLUDED.size,
				content_type = EXCLUDED.content_type,
				fetched_at = EXCLUDED.fetched_at,
				updated_at = EXCLUDED.updated_at
		`
	} else {
		query = `
			INSERT INTO artifacts (version_purl, filename, upstream_url, storage_path, content_hash,
			                       size, content_type, fetched_at, hit_count, last_accessed_at,
			                       created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(version_purl, filename) DO UPDATE SET
				storage_path = excluded.storage_path,
				content_hash = excluded.content_hash,
				size = excluded.size,
				content_type = excluded.content_type,
				fetched_at = excluded.fetched_at,
				updated_at = excluded.updated_at
		`
	}

	_, err := db.Exec(query,
		a.VersionPURL, a.Filename, a.UpstreamURL, a.StoragePath, a.ContentHash,
		a.Size, a.ContentType, a.FetchedAt, a.HitCount, a.LastAccessedAt, now, now,
	)
	if err != nil {
		return fmt.Errorf("upserting artifact: %w", err)
	}
	return nil
}

func (db *DB) RecordArtifactHit(versionPURL, filename string) error {
	now := time.Now()
	query := db.Rebind(`
		UPDATE artifacts
		SET hit_count = hit_count + 1, last_accessed_at = ?, updated_at = ?
		WHERE version_purl = ? AND filename = ?
	`)
	_, err := db.Exec(query, now, now, versionPURL, filename)
	return err
}

func (db *DB) MarkArtifactCached(versionPURL, filename, storagePath, contentHash string, size int64, contentType string) error {
	now := time.Now()
	query := db.Rebind(`
		UPDATE artifacts
		SET storage_path = ?, content_hash = ?, size = ?, content_type = ?,
		    fetched_at = ?, updated_at = ?
		WHERE version_purl = ? AND filename = ?
	`)
	_, err := db.Exec(query, storagePath, contentHash, size, contentType, now, now, versionPURL, filename)
	return err
}

// Cache management queries

func (db *DB) GetLeastRecentlyUsedArtifacts(limit int) ([]Artifact, error) {
	var artifacts []Artifact
	query := db.Rebind(`
		SELECT id, version_purl, filename, upstream_url, storage_path, content_hash,
		       size, content_type, fetched_at, hit_count, last_accessed_at,
		       created_at, updated_at
		FROM artifacts
		WHERE storage_path IS NOT NULL
		ORDER BY last_accessed_at ASC NULLS FIRST
		LIMIT ?
	`)
	err := db.Select(&artifacts, query, limit)
	if err != nil {
		return nil, err
	}
	return artifacts, nil
}

func (db *DB) GetTotalCacheSize() (int64, error) {
	var total sql.NullInt64
	err := db.Get(&total, `SELECT SUM(size) FROM artifacts WHERE storage_path IS NOT NULL`)
	if err != nil {
		return 0, err
	}
	if !total.Valid {
		return 0, nil
	}
	return total.Int64, nil
}

func (db *DB) GetCachedArtifactCount() (int64, error) {
	var count int64
	err := db.Get(&count, `SELECT COUNT(*) FROM artifacts WHERE storage_path IS NOT NULL`)
	return count, err
}

func (db *DB) ClearArtifactCache(versionPURL, filename string) error {
	query := db.Rebind(`
		UPDATE artifacts
		SET storage_path = NULL, content_hash = NULL, size = NULL,
		    content_type = NULL, fetched_at = NULL, updated_at = ?
		WHERE version_purl = ? AND filename = ?
	`)
	_, err := db.Exec(query, time.Now(), versionPURL, filename)
	return err
}

// Stats queries

type CacheStats struct {
	TotalPackages   int64
	TotalVersions   int64
	TotalArtifacts  int64
	TotalSize       int64
	TotalHits       int64
	EcosystemCounts map[string]int64
}

func (db *DB) GetCacheStats() (*CacheStats, error) {
	stats := &CacheStats{
		EcosystemCounts: make(map[string]int64),
	}

	if err := db.Get(&stats.TotalPackages, `SELECT COUNT(*) FROM packages`); err != nil {
		return nil, err
	}

	if err := db.Get(&stats.TotalVersions, `SELECT COUNT(*) FROM versions`); err != nil {
		return nil, err
	}

	// Check if artifacts table exists (might not if using git-pkgs db without proxy tables)
	hasArtifacts, err := db.HasTable("artifacts")
	if err != nil {
		return nil, err
	}

	if hasArtifacts {
		row := db.QueryRow(`
			SELECT COUNT(*), COALESCE(SUM(size), 0)
			FROM artifacts WHERE storage_path IS NOT NULL
		`)
		if err := row.Scan(&stats.TotalArtifacts, &stats.TotalSize); err != nil {
			return nil, err
		}

		var totalHits sql.NullInt64
		if err := db.Get(&totalHits, `SELECT SUM(hit_count) FROM artifacts`); err != nil {
			return nil, err
		}
		if totalHits.Valid {
			stats.TotalHits = totalHits.Int64
		}
	}

	rows, err := db.Query(`SELECT ecosystem, COUNT(*) FROM packages GROUP BY ecosystem`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var ecosystem string
		var count int64
		if err := rows.Scan(&ecosystem, &count); err != nil {
			return nil, err
		}
		stats.EcosystemCounts[ecosystem] = count
	}

	return stats, rows.Err()
}

type PopularPackage struct {
	Ecosystem string `db:"ecosystem"`
	Name      string `db:"name"`
	Hits      int64  `db:"hits"`
	Size      int64  `db:"size"`
}

func (db *DB) GetMostPopularPackages(limit int) ([]PopularPackage, error) {
	// Check if artifacts table exists
	hasArtifacts, err := db.HasTable("artifacts")
	if err != nil {
		return nil, err
	}
	if !hasArtifacts {
		return nil, nil
	}

	var packages []PopularPackage
	query := db.Rebind(`
		SELECT p.ecosystem, p.name, COALESCE(SUM(a.hit_count), 0) as hits, COALESCE(SUM(a.size), 0) as size
		FROM packages p
		JOIN versions v ON v.package_purl = p.purl
		JOIN artifacts a ON a.version_purl = v.purl
		WHERE a.storage_path IS NOT NULL
		GROUP BY p.purl, p.ecosystem, p.name
		ORDER BY hits DESC
		LIMIT ?
	`)
	err = db.Select(&packages, query, limit)
	if err != nil {
		return nil, err
	}
	return packages, nil
}

type RecentPackage struct {
	Ecosystem string    `db:"ecosystem"`
	Name      string    `db:"name"`
	Version   string    `db:"version"`
	CachedAt  time.Time `db:"fetched_at"`
	Size      int64     `db:"size"`
}

func (db *DB) GetRecentlyCachedPackages(limit int) ([]RecentPackage, error) {
	// Check if artifacts table exists
	hasArtifacts, err := db.HasTable("artifacts")
	if err != nil {
		return nil, err
	}
	if !hasArtifacts {
		return nil, nil
	}

	var packages []RecentPackage
	// We need to extract version from the purl since there's no separate version column
	query := db.Rebind(`
		SELECT p.ecosystem, p.name,
		       SUBSTR(v.purl, INSTR(v.purl, '@') + 1) as version,
		       a.fetched_at, COALESCE(a.size, 0) as size
		FROM artifacts a
		JOIN versions v ON v.purl = a.version_purl
		JOIN packages p ON p.purl = v.package_purl
		WHERE a.storage_path IS NOT NULL AND a.fetched_at IS NOT NULL
		ORDER BY a.fetched_at DESC
		LIMIT ?
	`)

	// For postgres, use different string function
	if db.dialect == DialectPostgres {
		query = db.Rebind(`
			SELECT p.ecosystem, p.name,
			       SUBSTRING(v.purl FROM POSITION('@' IN v.purl) + 1) as version,
			       a.fetched_at, COALESCE(a.size, 0) as size
			FROM artifacts a
			JOIN versions v ON v.purl = a.version_purl
			JOIN packages p ON p.purl = v.package_purl
			WHERE a.storage_path IS NOT NULL AND a.fetched_at IS NOT NULL
			ORDER BY a.fetched_at DESC
			LIMIT ?
		`)
	}

	err = db.Select(&packages, query, limit)
	if err != nil {
		return nil, err
	}
	return packages, nil
}
