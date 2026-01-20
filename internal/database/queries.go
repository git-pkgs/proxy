package database

import (
	"database/sql"
	"fmt"
	"time"
)

// Package queries

func (db *DB) GetPackageByPURL(purl string) (*Package, error) {
	var pkg Package
	err := db.QueryRow(`
		SELECT id, purl, ecosystem, name, namespace, latest_version, license,
		       description, homepage, repository_url, upstream_url,
		       metadata_fetched_at, created_at, updated_at
		FROM packages WHERE purl = ?
	`, purl).Scan(
		&pkg.ID, &pkg.PURL, &pkg.Ecosystem, &pkg.Name, &pkg.Namespace,
		&pkg.LatestVersion, &pkg.License, &pkg.Description, &pkg.Homepage,
		&pkg.RepositoryURL, &pkg.UpstreamURL, &pkg.MetadataFetchedAt,
		&pkg.CreatedAt, &pkg.UpdatedAt,
	)
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
	err := db.QueryRow(`
		SELECT id, purl, ecosystem, name, namespace, latest_version, license,
		       description, homepage, repository_url, upstream_url,
		       metadata_fetched_at, created_at, updated_at
		FROM packages WHERE ecosystem = ? AND name = ?
	`, ecosystem, name).Scan(
		&pkg.ID, &pkg.PURL, &pkg.Ecosystem, &pkg.Name, &pkg.Namespace,
		&pkg.LatestVersion, &pkg.License, &pkg.Description, &pkg.Homepage,
		&pkg.RepositoryURL, &pkg.UpstreamURL, &pkg.MetadataFetchedAt,
		&pkg.CreatedAt, &pkg.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &pkg, nil
}

func (db *DB) UpsertPackage(pkg *Package) (int64, error) {
	now := time.Now()
	result, err := db.Exec(`
		INSERT INTO packages (purl, ecosystem, name, namespace, latest_version, license,
		                      description, homepage, repository_url, upstream_url,
		                      metadata_fetched_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(purl) DO UPDATE SET
			latest_version = excluded.latest_version,
			license = excluded.license,
			description = excluded.description,
			homepage = excluded.homepage,
			repository_url = excluded.repository_url,
			metadata_fetched_at = excluded.metadata_fetched_at,
			updated_at = excluded.updated_at
	`,
		pkg.PURL, pkg.Ecosystem, pkg.Name, pkg.Namespace, pkg.LatestVersion,
		pkg.License, pkg.Description, pkg.Homepage, pkg.RepositoryURL,
		pkg.UpstreamURL, pkg.MetadataFetchedAt, now, now,
	)
	if err != nil {
		return 0, fmt.Errorf("upserting package: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		// On conflict, fetch the existing ID
		existing, err := db.GetPackageByPURL(pkg.PURL)
		if err != nil {
			return 0, err
		}
		return existing.ID, nil
	}
	return id, nil
}

// Version queries

func (db *DB) GetVersionByPURL(purl string) (*Version, error) {
	var v Version
	err := db.QueryRow(`
		SELECT id, purl, package_id, version, license, integrity, published_at,
		       yanked, metadata_fetched_at, created_at, updated_at
		FROM versions WHERE purl = ?
	`, purl).Scan(
		&v.ID, &v.PURL, &v.PackageID, &v.Version, &v.License, &v.Integrity,
		&v.PublishedAt, &v.Yanked, &v.MetadataFetchedAt, &v.CreatedAt, &v.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &v, nil
}

func (db *DB) GetVersionByPackageAndVersion(packageID int64, version string) (*Version, error) {
	var v Version
	err := db.QueryRow(`
		SELECT id, purl, package_id, version, license, integrity, published_at,
		       yanked, metadata_fetched_at, created_at, updated_at
		FROM versions WHERE package_id = ? AND version = ?
	`, packageID, version).Scan(
		&v.ID, &v.PURL, &v.PackageID, &v.Version, &v.License, &v.Integrity,
		&v.PublishedAt, &v.Yanked, &v.MetadataFetchedAt, &v.CreatedAt, &v.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &v, nil
}

func (db *DB) GetVersionsByPackageID(packageID int64) ([]Version, error) {
	rows, err := db.Query(`
		SELECT id, purl, package_id, version, license, integrity, published_at,
		       yanked, metadata_fetched_at, created_at, updated_at
		FROM versions WHERE package_id = ?
		ORDER BY created_at DESC
	`, packageID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var versions []Version
	for rows.Next() {
		var v Version
		if err := rows.Scan(
			&v.ID, &v.PURL, &v.PackageID, &v.Version, &v.License, &v.Integrity,
			&v.PublishedAt, &v.Yanked, &v.MetadataFetchedAt, &v.CreatedAt, &v.UpdatedAt,
		); err != nil {
			return nil, err
		}
		versions = append(versions, v)
	}
	return versions, rows.Err()
}

func (db *DB) UpsertVersion(v *Version) (int64, error) {
	now := time.Now()
	result, err := db.Exec(`
		INSERT INTO versions (purl, package_id, version, license, integrity, published_at,
		                      yanked, metadata_fetched_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(purl) DO UPDATE SET
			license = excluded.license,
			integrity = excluded.integrity,
			published_at = excluded.published_at,
			yanked = excluded.yanked,
			metadata_fetched_at = excluded.metadata_fetched_at,
			updated_at = excluded.updated_at
	`,
		v.PURL, v.PackageID, v.Version, v.License, v.Integrity,
		v.PublishedAt, v.Yanked, v.MetadataFetchedAt, now, now,
	)
	if err != nil {
		return 0, fmt.Errorf("upserting version: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		existing, err := db.GetVersionByPURL(v.PURL)
		if err != nil {
			return 0, err
		}
		return existing.ID, nil
	}
	return id, nil
}

// Artifact queries

func (db *DB) GetArtifact(versionID int64, filename string) (*Artifact, error) {
	var a Artifact
	err := db.QueryRow(`
		SELECT id, version_id, filename, upstream_url, storage_path, content_hash,
		       size, content_type, fetched_at, hit_count, last_accessed_at,
		       created_at, updated_at
		FROM artifacts WHERE version_id = ? AND filename = ?
	`, versionID, filename).Scan(
		&a.ID, &a.VersionID, &a.Filename, &a.UpstreamURL, &a.StoragePath,
		&a.ContentHash, &a.Size, &a.ContentType, &a.FetchedAt, &a.HitCount,
		&a.LastAccessedAt, &a.CreatedAt, &a.UpdatedAt,
	)
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
	err := db.QueryRow(`
		SELECT id, version_id, filename, upstream_url, storage_path, content_hash,
		       size, content_type, fetched_at, hit_count, last_accessed_at,
		       created_at, updated_at
		FROM artifacts WHERE storage_path = ?
	`, storagePath).Scan(
		&a.ID, &a.VersionID, &a.Filename, &a.UpstreamURL, &a.StoragePath,
		&a.ContentHash, &a.Size, &a.ContentType, &a.FetchedAt, &a.HitCount,
		&a.LastAccessedAt, &a.CreatedAt, &a.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

func (db *DB) UpsertArtifact(a *Artifact) (int64, error) {
	now := time.Now()
	result, err := db.Exec(`
		INSERT INTO artifacts (version_id, filename, upstream_url, storage_path, content_hash,
		                       size, content_type, fetched_at, hit_count, last_accessed_at,
		                       created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(version_id, filename) DO UPDATE SET
			storage_path = excluded.storage_path,
			content_hash = excluded.content_hash,
			size = excluded.size,
			content_type = excluded.content_type,
			fetched_at = excluded.fetched_at,
			updated_at = excluded.updated_at
	`,
		a.VersionID, a.Filename, a.UpstreamURL, a.StoragePath, a.ContentHash,
		a.Size, a.ContentType, a.FetchedAt, a.HitCount, a.LastAccessedAt, now, now,
	)
	if err != nil {
		return 0, fmt.Errorf("upserting artifact: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		existing, err := db.GetArtifact(a.VersionID, a.Filename)
		if err != nil {
			return 0, err
		}
		return existing.ID, nil
	}
	return id, nil
}

func (db *DB) RecordArtifactHit(artifactID int64) error {
	now := time.Now()
	_, err := db.Exec(`
		UPDATE artifacts
		SET hit_count = hit_count + 1, last_accessed_at = ?, updated_at = ?
		WHERE id = ?
	`, now, now, artifactID)
	return err
}

func (db *DB) MarkArtifactCached(artifactID int64, storagePath, contentHash string, size int64, contentType string) error {
	now := time.Now()
	_, err := db.Exec(`
		UPDATE artifacts
		SET storage_path = ?, content_hash = ?, size = ?, content_type = ?,
		    fetched_at = ?, updated_at = ?
		WHERE id = ?
	`, storagePath, contentHash, size, contentType, now, now, artifactID)
	return err
}

// Cache management queries

func (db *DB) GetLeastRecentlyUsedArtifacts(limit int) ([]Artifact, error) {
	rows, err := db.Query(`
		SELECT id, version_id, filename, upstream_url, storage_path, content_hash,
		       size, content_type, fetched_at, hit_count, last_accessed_at,
		       created_at, updated_at
		FROM artifacts
		WHERE storage_path IS NOT NULL
		ORDER BY last_accessed_at ASC NULLS FIRST
		LIMIT ?
	`, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var artifacts []Artifact
	for rows.Next() {
		var a Artifact
		if err := rows.Scan(
			&a.ID, &a.VersionID, &a.Filename, &a.UpstreamURL, &a.StoragePath,
			&a.ContentHash, &a.Size, &a.ContentType, &a.FetchedAt, &a.HitCount,
			&a.LastAccessedAt, &a.CreatedAt, &a.UpdatedAt,
		); err != nil {
			return nil, err
		}
		artifacts = append(artifacts, a)
	}
	return artifacts, rows.Err()
}

func (db *DB) GetTotalCacheSize() (int64, error) {
	var total sql.NullInt64
	err := db.QueryRow(`
		SELECT SUM(size) FROM artifacts WHERE storage_path IS NOT NULL
	`).Scan(&total)
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
	err := db.QueryRow(`
		SELECT COUNT(*) FROM artifacts WHERE storage_path IS NOT NULL
	`).Scan(&count)
	return count, err
}

func (db *DB) ClearArtifactCache(artifactID int64) error {
	_, err := db.Exec(`
		UPDATE artifacts
		SET storage_path = NULL, content_hash = NULL, size = NULL,
		    content_type = NULL, fetched_at = NULL, updated_at = ?
		WHERE id = ?
	`, time.Now(), artifactID)
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

	// Total packages
	if err := db.QueryRow(`SELECT COUNT(*) FROM packages`).Scan(&stats.TotalPackages); err != nil {
		return nil, err
	}

	// Total versions
	if err := db.QueryRow(`SELECT COUNT(*) FROM versions`).Scan(&stats.TotalVersions); err != nil {
		return nil, err
	}

	// Total cached artifacts and size
	if err := db.QueryRow(`
		SELECT COUNT(*), COALESCE(SUM(size), 0)
		FROM artifacts WHERE storage_path IS NOT NULL
	`).Scan(&stats.TotalArtifacts, &stats.TotalSize); err != nil {
		return nil, err
	}

	// Total hits
	var totalHits sql.NullInt64
	if err := db.QueryRow(`SELECT SUM(hit_count) FROM artifacts`).Scan(&totalHits); err != nil {
		return nil, err
	}
	if totalHits.Valid {
		stats.TotalHits = totalHits.Int64
	}

	// Packages per ecosystem
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
	Ecosystem string
	Name      string
	Hits      int64
	Size      int64
}

func (db *DB) GetMostPopularPackages(limit int) ([]PopularPackage, error) {
	rows, err := db.Query(`
		SELECT p.ecosystem, p.name, COALESCE(SUM(a.hit_count), 0) as hits, COALESCE(SUM(a.size), 0) as size
		FROM packages p
		JOIN versions v ON v.package_id = p.id
		JOIN artifacts a ON a.version_id = v.id
		WHERE a.storage_path IS NOT NULL
		GROUP BY p.id
		ORDER BY hits DESC
		LIMIT ?
	`, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var packages []PopularPackage
	for rows.Next() {
		var pkg PopularPackage
		if err := rows.Scan(&pkg.Ecosystem, &pkg.Name, &pkg.Hits, &pkg.Size); err != nil {
			return nil, err
		}
		packages = append(packages, pkg)
	}
	return packages, rows.Err()
}

type RecentPackage struct {
	Ecosystem string
	Name      string
	Version   string
	CachedAt  time.Time
	Size      int64
}

func (db *DB) GetRecentlyCachedPackages(limit int) ([]RecentPackage, error) {
	rows, err := db.Query(`
		SELECT p.ecosystem, p.name, v.version, a.fetched_at, a.size
		FROM artifacts a
		JOIN versions v ON v.id = a.version_id
		JOIN packages p ON p.id = v.package_id
		WHERE a.storage_path IS NOT NULL AND a.fetched_at IS NOT NULL
		ORDER BY a.fetched_at DESC
		LIMIT ?
	`, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var packages []RecentPackage
	for rows.Next() {
		var pkg RecentPackage
		var fetchedAt sql.NullTime
		if err := rows.Scan(&pkg.Ecosystem, &pkg.Name, &pkg.Version, &fetchedAt, &pkg.Size); err != nil {
			return nil, err
		}
		if fetchedAt.Valid {
			pkg.CachedAt = fetchedAt.Time
		}
		packages = append(packages, pkg)
	}
	return packages, rows.Err()
}
