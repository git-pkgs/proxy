package database

import (
	"fmt"
	"strings"
	"time"
)

const postgresTimestamp = "TIMESTAMP"

// Schema for proxy-specific tables. The packages and versions tables
// are compatible with git-pkgs, allowing the proxy to use an existing
// git-pkgs database as a starting point.

var schemaSQLite = `
CREATE TABLE IF NOT EXISTS schema_info (
	version INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS packages (
	id INTEGER PRIMARY KEY,
	purl TEXT NOT NULL,
	ecosystem TEXT NOT NULL,
	name TEXT NOT NULL,
	latest_version TEXT,
	license TEXT,
	description TEXT,
	homepage TEXT,
	repository_url TEXT,
	registry_url TEXT,
	supplier_name TEXT,
	supplier_type TEXT,
	source TEXT,
	enriched_at DATETIME,
	vulns_synced_at DATETIME,
	created_at DATETIME,
	updated_at DATETIME
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_packages_purl ON packages(purl);
CREATE INDEX IF NOT EXISTS idx_packages_ecosystem_name ON packages(ecosystem, name);

CREATE TABLE IF NOT EXISTS versions (
	id INTEGER PRIMARY KEY,
	purl TEXT NOT NULL,
	package_purl TEXT NOT NULL,
	license TEXT,
	published_at DATETIME,
	integrity TEXT,
	yanked INTEGER DEFAULT 0,
	source TEXT,
	enriched_at DATETIME,
	created_at DATETIME,
	updated_at DATETIME
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_versions_purl ON versions(purl);
CREATE INDEX IF NOT EXISTS idx_versions_package_purl ON versions(package_purl);

CREATE TABLE IF NOT EXISTS artifacts (
	id INTEGER PRIMARY KEY,
	version_purl TEXT NOT NULL,
	filename TEXT NOT NULL,
	upstream_url TEXT NOT NULL,
	storage_path TEXT,
	content_hash TEXT,
	size INTEGER,
	content_type TEXT,
	fetched_at DATETIME,
	hit_count INTEGER DEFAULT 0,
	last_accessed_at DATETIME,
	created_at DATETIME,
	updated_at DATETIME
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_artifacts_version_filename ON artifacts(version_purl, filename);
CREATE INDEX IF NOT EXISTS idx_artifacts_storage_path ON artifacts(storage_path);
CREATE INDEX IF NOT EXISTS idx_artifacts_last_accessed ON artifacts(last_accessed_at);

CREATE TABLE IF NOT EXISTS vulnerabilities (
	id INTEGER PRIMARY KEY,
	vuln_id TEXT NOT NULL,
	ecosystem TEXT NOT NULL,
	package_name TEXT NOT NULL,
	severity TEXT,
	summary TEXT,
	fixed_version TEXT,
	cvss_score REAL,
	"references" TEXT,
	fetched_at DATETIME,
	created_at DATETIME,
	updated_at DATETIME
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_vulns_id_pkg ON vulnerabilities(vuln_id, ecosystem, package_name);
CREATE INDEX IF NOT EXISTS idx_vulns_ecosystem_pkg ON vulnerabilities(ecosystem, package_name);

CREATE TABLE IF NOT EXISTS metadata_cache (
	id INTEGER PRIMARY KEY,
	ecosystem TEXT NOT NULL,
	name TEXT NOT NULL,
	storage_path TEXT NOT NULL,
	etag TEXT,
	content_type TEXT,
	size INTEGER,
	last_modified DATETIME,
	fetched_at DATETIME,
	created_at DATETIME,
	updated_at DATETIME
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_metadata_eco_name ON metadata_cache(ecosystem, name);

CREATE TABLE IF NOT EXISTS migrations (
	name TEXT NOT NULL PRIMARY KEY,
	applied_at DATETIME NOT NULL
);
`

var schemaPostgres = `
CREATE TABLE IF NOT EXISTS schema_info (
	version INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS packages (
	id SERIAL PRIMARY KEY,
	purl TEXT NOT NULL,
	ecosystem TEXT NOT NULL,
	name TEXT NOT NULL,
	latest_version TEXT,
	license TEXT,
	description TEXT,
	homepage TEXT,
	repository_url TEXT,
	registry_url TEXT,
	supplier_name TEXT,
	supplier_type TEXT,
	source TEXT,
	enriched_at TIMESTAMP,
	vulns_synced_at TIMESTAMP,
	created_at TIMESTAMP,
	updated_at TIMESTAMP
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_packages_purl ON packages(purl);
CREATE INDEX IF NOT EXISTS idx_packages_ecosystem_name ON packages(ecosystem, name);

CREATE TABLE IF NOT EXISTS versions (
	id SERIAL PRIMARY KEY,
	purl TEXT NOT NULL,
	package_purl TEXT NOT NULL,
	license TEXT,
	published_at TIMESTAMP,
	integrity TEXT,
	yanked BOOLEAN DEFAULT FALSE,
	source TEXT,
	enriched_at TIMESTAMP,
	created_at TIMESTAMP,
	updated_at TIMESTAMP
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_versions_purl ON versions(purl);
CREATE INDEX IF NOT EXISTS idx_versions_package_purl ON versions(package_purl);

CREATE TABLE IF NOT EXISTS artifacts (
	id SERIAL PRIMARY KEY,
	version_purl TEXT NOT NULL,
	filename TEXT NOT NULL,
	upstream_url TEXT NOT NULL,
	storage_path TEXT,
	content_hash TEXT,
	size BIGINT,
	content_type TEXT,
	fetched_at TIMESTAMP,
	hit_count BIGINT DEFAULT 0,
	last_accessed_at TIMESTAMP,
	created_at TIMESTAMP,
	updated_at TIMESTAMP
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_artifacts_version_filename ON artifacts(version_purl, filename);
CREATE INDEX IF NOT EXISTS idx_artifacts_storage_path ON artifacts(storage_path);
CREATE INDEX IF NOT EXISTS idx_artifacts_last_accessed ON artifacts(last_accessed_at);

CREATE TABLE IF NOT EXISTS vulnerabilities (
	id SERIAL PRIMARY KEY,
	vuln_id TEXT NOT NULL,
	ecosystem TEXT NOT NULL,
	package_name TEXT NOT NULL,
	severity TEXT,
	summary TEXT,
	fixed_version TEXT,
	cvss_score REAL,
	"references" TEXT,
	fetched_at TIMESTAMP,
	created_at TIMESTAMP,
	updated_at TIMESTAMP
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_vulns_id_pkg ON vulnerabilities(vuln_id, ecosystem, package_name);
CREATE INDEX IF NOT EXISTS idx_vulns_ecosystem_pkg ON vulnerabilities(ecosystem, package_name);

CREATE TABLE IF NOT EXISTS metadata_cache (
	id SERIAL PRIMARY KEY,
	ecosystem TEXT NOT NULL,
	name TEXT NOT NULL,
	storage_path TEXT NOT NULL,
	etag TEXT,
	content_type TEXT,
	size BIGINT,
	last_modified TIMESTAMP,
	fetched_at TIMESTAMP,
	created_at TIMESTAMP,
	updated_at TIMESTAMP
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_metadata_eco_name ON metadata_cache(ecosystem, name);

CREATE TABLE IF NOT EXISTS migrations (
	name TEXT NOT NULL PRIMARY KEY,
	applied_at TIMESTAMP NOT NULL
);
`

// schemaArtifactsOnly contains just the artifacts table for adding to existing git-pkgs databases.
var schemaArtifactsSQLite = `
CREATE TABLE IF NOT EXISTS artifacts (
	id INTEGER PRIMARY KEY,
	version_purl TEXT NOT NULL,
	filename TEXT NOT NULL,
	upstream_url TEXT NOT NULL,
	storage_path TEXT,
	content_hash TEXT,
	size INTEGER,
	content_type TEXT,
	fetched_at DATETIME,
	hit_count INTEGER DEFAULT 0,
	last_accessed_at DATETIME,
	created_at DATETIME,
	updated_at DATETIME
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_artifacts_version_filename ON artifacts(version_purl, filename);
CREATE INDEX IF NOT EXISTS idx_artifacts_storage_path ON artifacts(storage_path);
CREATE INDEX IF NOT EXISTS idx_artifacts_last_accessed ON artifacts(last_accessed_at);
`

var schemaArtifactsPostgres = `
CREATE TABLE IF NOT EXISTS artifacts (
	id SERIAL PRIMARY KEY,
	version_purl TEXT NOT NULL,
	filename TEXT NOT NULL,
	upstream_url TEXT NOT NULL,
	storage_path TEXT,
	content_hash TEXT,
	size BIGINT,
	content_type TEXT,
	fetched_at TIMESTAMP,
	hit_count BIGINT DEFAULT 0,
	last_accessed_at TIMESTAMP,
	created_at TIMESTAMP,
	updated_at TIMESTAMP
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_artifacts_version_filename ON artifacts(version_purl, filename);
CREATE INDEX IF NOT EXISTS idx_artifacts_storage_path ON artifacts(storage_path);
CREATE INDEX IF NOT EXISTS idx_artifacts_last_accessed ON artifacts(last_accessed_at);
`

func (db *DB) CreateSchema() error {
	if err := db.OptimizeForBulkWrites(); err != nil {
		return err
	}

	var schema string
	if db.dialect == DialectPostgres {
		schema = schemaPostgres
	} else {
		schema = schemaSQLite
	}

	if _, err := db.Exec(schema); err != nil {
		return fmt.Errorf("executing schema: %w", err)
	}

	query := db.Rebind("INSERT INTO schema_info (version) VALUES (?)")
	if _, err := db.Exec(query, SchemaVersion); err != nil {
		return fmt.Errorf("setting schema version: %w", err)
	}

	// Record all migrations as applied since the full schema is already current.
	if err := db.recordAllMigrations(); err != nil {
		return fmt.Errorf("recording migrations: %w", err)
	}

	return db.OptimizeForReads()
}

// EnsureArtifactsTable adds the artifacts table to an existing database
// (e.g., a git-pkgs database) if it doesn't already exist.
func (db *DB) EnsureArtifactsTable() error {
	var schema string
	if db.dialect == DialectPostgres {
		schema = schemaArtifactsPostgres
	} else {
		schema = schemaArtifactsSQLite
	}

	if _, err := db.Exec(schema); err != nil {
		return fmt.Errorf("creating artifacts table: %w", err)
	}

	return nil
}

func (db *DB) SchemaVersion() (int, error) {
	var version int
	err := db.Get(&version, "SELECT version FROM schema_info LIMIT 1")
	if err != nil {
		return 0, err
	}
	return version, nil
}

// HasTable checks if a table exists in the database.
func (db *DB) HasTable(name string) (bool, error) {
	var exists bool
	var query string

	if db.dialect == DialectPostgres {
		query = "SELECT EXISTS (SELECT FROM information_schema.tables WHERE table_name = $1)"
	} else {
		query = "SELECT EXISTS (SELECT 1 FROM sqlite_master WHERE type='table' AND name=?)"
	}

	err := db.Get(&exists, query, name)
	return exists, err
}

// HasColumn checks if a column exists in a table.
func (db *DB) HasColumn(table, column string) (bool, error) {
	var exists bool
	var query string

	if db.dialect == DialectPostgres {
		query = "SELECT EXISTS (SELECT FROM information_schema.columns WHERE table_name = $1 AND column_name = $2)"
	} else {
		// For SQLite, check table_info
		query = "SELECT COUNT(*) > 0 FROM pragma_table_info(?) WHERE name = ?"
	}

	err := db.Get(&exists, query, table, column)
	return exists, err
}

// migration represents a named schema migration.
type migration struct {
	name string
	fn   func(db *DB) error
}

// migrations is the ordered list of all schema migrations. See
// docs/migrations.md for how to add new ones.
var migrations = []migration{
	{"001_add_packages_enrichment_columns", migrateAddPackagesEnrichmentColumns},
	{"002_add_versions_enrichment_columns", migrateAddVersionsEnrichmentColumns},
	{"003_ensure_artifacts_table", migrateEnsureArtifactsTable},
	{"004_ensure_vulnerabilities_table", migrateEnsureVulnerabilitiesTable},
	{"005_ensure_metadata_cache_table", migrateEnsureMetadataCacheTable},
}

// isTableNotFound returns true if the error indicates a missing table.
// SQLite returns "no such table: X", Postgres returns "relation \"X\" does not exist".
func isTableNotFound(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "no such table") ||
		strings.Contains(msg, "does not exist")
}

// createMigrationsTable creates the migrations table.
func (db *DB) createMigrationsTable() error {
	var ts string
	if db.dialect == DialectPostgres {
		ts = "TIMESTAMP"
	} else {
		ts = "DATETIME"
	}

	query := fmt.Sprintf(`CREATE TABLE IF NOT EXISTS migrations (
		name TEXT NOT NULL PRIMARY KEY,
		applied_at %s NOT NULL
	)`, ts)

	if _, err := db.Exec(query); err != nil {
		return fmt.Errorf("creating migrations table: %w", err)
	}
	return nil
}

// appliedMigrations returns the set of migration names that have been recorded.
// Returns nil if the migrations table does not exist yet.
func (db *DB) appliedMigrations() (map[string]bool, error) {
	var names []string
	err := db.Select(&names, "SELECT name FROM migrations")
	if err != nil {
		// Table doesn't exist yet — this is a pre-migration database.
		if isTableNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("loading applied migrations: %w", err)
	}

	applied := make(map[string]bool, len(names))
	for _, name := range names {
		applied[name] = true
	}
	return applied, nil
}

// recordMigration inserts a migration name into the migrations table.
func (db *DB) recordMigration(name string) error {
	query := db.Rebind("INSERT INTO migrations (name, applied_at) VALUES (?, ?)")
	if _, err := db.Exec(query, name, time.Now().UTC()); err != nil {
		return fmt.Errorf("recording migration %s: %w", name, err)
	}
	return nil
}

// recordAllMigrations marks every known migration as applied.
func (db *DB) recordAllMigrations() error {
	for _, m := range migrations {
		if err := db.recordMigration(m.name); err != nil {
			return err
		}
	}
	return nil
}

// MigrateSchema applies any unapplied migrations in order.
// For a fully migrated database this executes a single SELECT query.
func (db *DB) MigrateSchema() error {
	applied, err := db.appliedMigrations()
	if err != nil {
		return err
	}

	// If the migrations table didn't exist, create it now.
	if applied == nil {
		if err := db.createMigrationsTable(); err != nil {
			return err
		}
		applied = make(map[string]bool)
	}

	for _, m := range migrations {
		if applied[m.name] {
			continue
		}
		if err := m.fn(db); err != nil {
			return fmt.Errorf("migration %s: %w", m.name, err)
		}
		if err := db.recordMigration(m.name); err != nil {
			return err
		}
	}

	return nil
}

func migrateAddPackagesEnrichmentColumns(db *DB) error {
	columns := map[string]string{
		"registry_url":    "TEXT",
		"supplier_name":   "TEXT",
		"supplier_type":   "TEXT",
		"source":          "TEXT",
		"enriched_at":     "DATETIME",
		"vulns_synced_at": "DATETIME",
	}

	if db.dialect == DialectPostgres {
		columns["enriched_at"] = postgresTimestamp
		columns["vulns_synced_at"] = postgresTimestamp
	}

	for column, colType := range columns {
		hasCol, err := db.HasColumn("packages", column)
		if err != nil {
			return fmt.Errorf("checking column %s: %w", column, err)
		}
		if !hasCol {
			alterQuery := fmt.Sprintf("ALTER TABLE packages ADD COLUMN %s %s", column, colType)
			if _, err := db.Exec(alterQuery); err != nil {
				return fmt.Errorf("adding column %s to packages: %w", column, err)
			}
		}
	}
	return nil
}

func migrateAddVersionsEnrichmentColumns(db *DB) error {
	columns := map[string]string{
		"integrity":   "TEXT",
		"yanked":      "INTEGER DEFAULT 0",
		"source":      "TEXT",
		"enriched_at": "DATETIME",
	}

	if db.dialect == DialectPostgres {
		columns["yanked"] = "BOOLEAN DEFAULT FALSE"
		columns["enriched_at"] = postgresTimestamp
	}

	for column, colType := range columns {
		hasCol, err := db.HasColumn("versions", column)
		if err != nil {
			return fmt.Errorf("checking column %s: %w", column, err)
		}
		if !hasCol {
			alterQuery := fmt.Sprintf("ALTER TABLE versions ADD COLUMN %s %s", column, colType)
			if _, err := db.Exec(alterQuery); err != nil {
				return fmt.Errorf("adding column %s to versions: %w", column, err)
			}
		}
	}
	return nil
}

func migrateEnsureArtifactsTable(db *DB) error {
	return db.EnsureArtifactsTable()
}

func migrateEnsureVulnerabilitiesTable(db *DB) error {
	hasVulns, err := db.HasTable("vulnerabilities")
	if err != nil {
		return fmt.Errorf("checking vulnerabilities table: %w", err)
	}
	if hasVulns {
		return nil
	}

	var vulnSchema string
	if db.dialect == DialectPostgres {
		vulnSchema = `
			CREATE TABLE vulnerabilities (
				id SERIAL PRIMARY KEY,
				vuln_id TEXT NOT NULL,
				ecosystem TEXT NOT NULL,
				package_name TEXT NOT NULL,
				severity TEXT,
				summary TEXT,
				fixed_version TEXT,
				cvss_score REAL,
				"references" TEXT,
				fetched_at TIMESTAMP,
				created_at TIMESTAMP,
				updated_at TIMESTAMP
			);
			CREATE UNIQUE INDEX IF NOT EXISTS idx_vulns_id_pkg ON vulnerabilities(vuln_id, ecosystem, package_name);
			CREATE INDEX IF NOT EXISTS idx_vulns_ecosystem_pkg ON vulnerabilities(ecosystem, package_name);
		`
	} else {
		vulnSchema = `
			CREATE TABLE vulnerabilities (
				id INTEGER PRIMARY KEY,
				vuln_id TEXT NOT NULL,
				ecosystem TEXT NOT NULL,
				package_name TEXT NOT NULL,
				severity TEXT,
				summary TEXT,
				fixed_version TEXT,
				cvss_score REAL,
				"references" TEXT,
				fetched_at DATETIME,
				created_at DATETIME,
				updated_at DATETIME
			);
			CREATE UNIQUE INDEX IF NOT EXISTS idx_vulns_id_pkg ON vulnerabilities(vuln_id, ecosystem, package_name);
			CREATE INDEX IF NOT EXISTS idx_vulns_ecosystem_pkg ON vulnerabilities(ecosystem, package_name);
		`
	}
	if _, err := db.Exec(vulnSchema); err != nil {
		return fmt.Errorf("creating vulnerabilities table: %w", err)
	}

	return nil
}

func migrateEnsureMetadataCacheTable(db *DB) error {
	return db.EnsureMetadataCacheTable()
}

// EnsureMetadataCacheTable creates the metadata_cache table if it doesn't exist.
func (db *DB) EnsureMetadataCacheTable() error {
	has, err := db.HasTable("metadata_cache")
	if err != nil {
		return fmt.Errorf("checking metadata_cache table: %w", err)
	}
	if has {
		return nil
	}

	var schema string
	if db.dialect == DialectPostgres {
		schema = `
			CREATE TABLE metadata_cache (
				id SERIAL PRIMARY KEY,
				ecosystem TEXT NOT NULL,
				name TEXT NOT NULL,
				storage_path TEXT NOT NULL,
				etag TEXT,
				content_type TEXT,
				size BIGINT,
				last_modified TIMESTAMP,
				fetched_at TIMESTAMP,
				created_at TIMESTAMP,
				updated_at TIMESTAMP
			);
			CREATE UNIQUE INDEX IF NOT EXISTS idx_metadata_eco_name ON metadata_cache(ecosystem, name);
		`
	} else {
		schema = `
			CREATE TABLE metadata_cache (
				id INTEGER PRIMARY KEY,
				ecosystem TEXT NOT NULL,
				name TEXT NOT NULL,
				storage_path TEXT NOT NULL,
				etag TEXT,
				content_type TEXT,
				size INTEGER,
				last_modified DATETIME,
				fetched_at DATETIME,
				created_at DATETIME,
				updated_at DATETIME
			);
			CREATE UNIQUE INDEX IF NOT EXISTS idx_metadata_eco_name ON metadata_cache(ecosystem, name);
		`
	}
	if _, err := db.Exec(schema); err != nil {
		return fmt.Errorf("creating metadata_cache table: %w", err)
	}
	return nil
}
