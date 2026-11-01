package database

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Project is a repository that uploaded an SBOM to the project inventory.
type Project struct {
	ID         int64          `db:"id"`
	Name       string         `db:"name"`
	Ref        sql.NullString `db:"ref"`
	Branch     sql.NullString `db:"branch"`
	UploadedAt time.Time      `db:"uploaded_at"`
	// Components is the number of package versions recorded for it.
	Components int64 `db:"components"`
}

// ProjectComponent is a package version a project uses. PackagePURL is the
// unversioned package URL, as packages.purl records it.
type ProjectComponent struct {
	PackagePURL string `db:"package_purl"`
	Ecosystem   string `db:"ecosystem"`
	Name        string `db:"name"`
	Version     string `db:"version"`
}

// ProjectComponentInfo is a project's component together with the package
// record the proxy holds for it, if any.
type ProjectComponentInfo struct {
	ProjectComponent
	CachedEcosystem sql.NullString `db:"cached_ecosystem"`
	CachedName      sql.NullString `db:"cached_name"`
	LatestVersion   sql.NullString `db:"latest_version"`
}

// ProjectUsage is a project using a version of a package.
type ProjectUsage struct {
	Name       string         `db:"name"`
	Ref        sql.NullString `db:"ref"`
	Branch     sql.NullString `db:"branch"`
	UploadedAt time.Time      `db:"uploaded_at"`
	Version    string         `db:"version"`
}

const projectColumns = `
	SELECT pr.id, pr.name, pr.ref, pr.branch, pr.uploaded_at,
	       (SELECT COUNT(*) FROM project_components pc WHERE pc.project_id = pr.id) AS components`

// SaveProject records the components of a project, replacing everything
// recorded for it before, in one transaction. Readers see either the old or
// the new set, never a mix.
func (db *DB) SaveProject(name, ref, branch string, uploadedAt time.Time, components []ProjectComponent) error {
	tx, err := db.Beginx()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	// The upsert locks the project row, so concurrent uploads of the same
	// project and pruning take turns.
	if _, err := tx.Exec(db.Rebind(`
		INSERT INTO projects (name, ref, branch, uploaded_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET ref = excluded.ref, branch = excluded.branch, uploaded_at = excluded.uploaded_at
	`), name, nullString(ref), nullString(branch), uploadedAt.UTC()); err != nil {
		return fmt.Errorf("recording project: %w", err)
	}
	var id int64
	if err := tx.Get(&id, db.Rebind(`SELECT id FROM projects WHERE name = ?`), name); err != nil {
		return fmt.Errorf("reading project id: %w", err)
	}
	if _, err := tx.Exec(db.Rebind(`DELETE FROM project_components WHERE project_id = ?`), id); err != nil {
		return fmt.Errorf("removing old components: %w", err)
	}

	insert, err := tx.Preparex(db.Rebind(`
		INSERT INTO project_components (project_id, package_purl, ecosystem, name, version)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(project_id, package_purl, version) DO NOTHING
	`))
	if err != nil {
		return err
	}
	defer func() { _ = insert.Close() }()
	for _, c := range components {
		if _, err := insert.Exec(id, c.PackagePURL, c.Ecosystem, c.Name, c.Version); err != nil {
			return fmt.Errorf("recording component %s@%s: %w", c.PackagePURL, c.Version, err)
		}
	}
	return tx.Commit()
}

// DeleteProject removes a project and its components. It reports whether the
// project existed.
func (db *DB) DeleteProject(name string) (bool, error) {
	tx, err := db.Beginx()
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()

	var id int64
	err = tx.Get(&id, db.Rebind(`DELETE FROM projects WHERE name = ? RETURNING id`), name)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("deleting project: %w", err)
	}
	if _, err := tx.Exec(db.Rebind(`DELETE FROM project_components WHERE project_id = ?`), id); err != nil {
		return false, fmt.Errorf("deleting project components: %w", err)
	}
	return true, tx.Commit()
}

// PruneProjects removes the projects last uploaded before cutoff, with their
// components, and returns how many it removed. A project uploaded again while
// the prune runs is kept.
func (db *DB) PruneProjects(cutoff time.Time) (int, error) {
	var ids []int64
	if err := db.Select(&ids, db.Rebind(`SELECT id FROM projects WHERE uploaded_at < ?`), cutoff.UTC()); err != nil {
		return 0, fmt.Errorf("listing expired projects: %w", err)
	}
	pruned := 0
	for _, id := range ids {
		ok, err := db.pruneProject(id, cutoff)
		if err != nil {
			return pruned, err
		}
		if ok {
			pruned++
		}
	}
	return pruned, nil
}

func (db *DB) pruneProject(id int64, cutoff time.Time) (bool, error) {
	tx, err := db.Beginx()
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.Exec(db.Rebind(`DELETE FROM projects WHERE id = ? AND uploaded_at < ?`), id, cutoff.UTC())
	if err != nil {
		return false, fmt.Errorf("pruning project: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return false, err
	}
	if _, err := tx.Exec(db.Rebind(`DELETE FROM project_components WHERE project_id = ?`), id); err != nil {
		return false, fmt.Errorf("pruning project components: %w", err)
	}
	return true, tx.Commit()
}

// ListProjects returns all projects ordered by name.
func (db *DB) ListProjects() ([]Project, error) {
	projects := []Project{}
	if err := db.Select(&projects, projectColumns+` FROM projects pr ORDER BY pr.name`); err != nil {
		return nil, err
	}
	return projects, nil
}

// GetProject returns the named project, or nil if there is none.
func (db *DB) GetProject(name string) (*Project, error) {
	var p Project
	err := db.Get(&p, db.Rebind(projectColumns+` FROM projects pr WHERE pr.name = ?`), name)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// GetProjectComponents returns the components of a project, ordered by
// ecosystem, name and version, with the package record the proxy holds for
// each.
func (db *DB) GetProjectComponents(projectID int64) ([]ProjectComponentInfo, error) {
	components := []ProjectComponentInfo{}
	err := db.Select(&components, db.Rebind(`
		SELECT c.package_purl, c.ecosystem, c.name, c.version,
		       p.ecosystem AS cached_ecosystem, p.name AS cached_name, p.latest_version
		FROM project_components c
		LEFT JOIN packages p ON p.purl = c.package_purl
		WHERE c.project_id = ?
		ORDER BY c.ecosystem, c.name, c.version
	`), projectID)
	if err != nil {
		return nil, err
	}
	return components, nil
}

// GetProjectsUsingPackage returns the projects using a package, or only
// those using version when it is not empty, ordered by project and version.
func (db *DB) GetProjectsUsingPackage(packagePURL, version string) ([]ProjectUsage, error) {
	query := `
		SELECT pr.name, pr.ref, pr.branch, pr.uploaded_at, c.version
		FROM project_components c
		JOIN projects pr ON pr.id = c.project_id
		WHERE c.package_purl = ?`
	args := []any{packagePURL}
	if version != "" {
		query += ` AND c.version = ?`
		args = append(args, version)
	}
	usages := []ProjectUsage{}
	if err := db.Select(&usages, db.Rebind(query+` ORDER BY pr.name, c.version`), args...); err != nil {
		return nil, err
	}
	return usages, nil
}

func nullString(s string) sql.NullString {
	return sql.NullString{String: s, Valid: s != ""}
}
