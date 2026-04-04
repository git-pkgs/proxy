# Database Migrations

Schema changes are tracked in a `migrations` table. Each migration has a name and a function. On startup, `MigrateSchema()` loads the set of already-applied names in one query and runs anything new.

Fresh databases created via `Create()` get the full schema and all migrations are recorded as already applied.

## Adding a migration

In `internal/database/schema.go`:

1. Write a migration function:

```go
func migrateAddWidgetColumn(db *DB) error {
    hasCol, err := db.HasColumn("packages", "widget")
    if err != nil {
        return fmt.Errorf("checking column widget: %w", err)
    }
    if !hasCol {
        colType := "TEXT"
        if db.dialect == DialectPostgres {
            colType = "TEXT" // adjust if types differ
        }
        if _, err := db.Exec(fmt.Sprintf("ALTER TABLE packages ADD COLUMN widget %s", colType)); err != nil {
            return fmt.Errorf("adding column widget: %w", err)
        }
    }
    return nil
}
```

2. Append it to the `migrations` slice with the next sequential prefix:

```go
var migrations = []migration{
    {"001_add_packages_enrichment_columns", migrateAddPackagesEnrichmentColumns},
    {"002_add_versions_enrichment_columns", migrateAddVersionsEnrichmentColumns},
    {"003_ensure_artifacts_table", migrateEnsureArtifactsTable},
    {"004_ensure_vulnerabilities_table", migrateEnsureVulnerabilitiesTable},
    {"005_add_widget_column", migrateAddWidgetColumn}, // new
}
```

3. Add the same column to both `schemaSQLite` and `schemaPostgres` at the top of the file so fresh databases start with the full schema.

## Rules

- Migration functions must be idempotent. Use `HasColumn`/`HasTable` checks or `IF NOT EXISTS` clauses so they're safe to run against a database that already has the change.
- Handle both SQLite and Postgres dialects. Common differences: `DATETIME` vs `TIMESTAMP`, `INTEGER DEFAULT 0` vs `BOOLEAN DEFAULT FALSE`, `INTEGER PRIMARY KEY` vs `SERIAL PRIMARY KEY`.
- Never reorder or rename existing entries. The name string is the migration's identity in the database.
- Never remove old migrations from the list. They won't run on already-migrated databases, but they need to exist for older databases upgrading for the first time.
