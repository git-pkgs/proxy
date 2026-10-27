package database

import (
	"database/sql"
	"slices"
	"testing"
	"time"
)

var (
	lodash20 = ProjectComponent{PackagePURL: "pkg:npm/lodash", Ecosystem: "npm", Name: "lodash", Version: "4.17.20"}
	lodash21 = ProjectComponent{PackagePURL: "pkg:npm/lodash", Ecosystem: "npm", Name: "lodash", Version: "4.17.21"}
	console  = ProjectComponent{PackagePURL: "pkg:composer/symfony/console", Ecosystem: "composer", Name: "symfony/console", Version: "v6.4.1"}
)

func saveProject(t *testing.T, db *DB, name string, at time.Time, components ...ProjectComponent) {
	t.Helper()
	if err := db.SaveProject(name, "", "", at, components); err != nil {
		t.Fatalf("SaveProject(%s): %v", name, err)
	}
}

func usageNames(usages []ProjectUsage) []string {
	var names []string
	for _, u := range usages {
		names = append(names, u.Name+"@"+u.Version)
	}
	return names
}

func TestSaveProjectReplacesComponents(t *testing.T) {
	runWithBothDatabases(t, func(t *testing.T, db *DB) {
		now := time.Now().UTC().Truncate(time.Second)
		if err := db.SaveProject("acme/web", "abc123", "main", now, []ProjectComponent{lodash20, console, lodash20}); err != nil {
			t.Fatal(err)
		}
		saveProject(t, db, "acme/web", now.Add(time.Minute), lodash21)

		project, err := db.GetProject("acme/web")
		if err != nil || project == nil {
			t.Fatalf("GetProject = %v, %v", project, err)
		}
		if project.Components != 1 || project.Ref.Valid || project.Branch.Valid {
			t.Errorf("project = %+v, want one component and ref and branch cleared", project)
		}
		if !project.UploadedAt.Equal(now.Add(time.Minute)) {
			t.Errorf("uploaded_at = %v, want %v", project.UploadedAt, now.Add(time.Minute))
		}
		components, err := db.GetProjectComponents(project.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(components) != 1 || components[0].ProjectComponent != lodash21 {
			t.Errorf("components = %+v, want only lodash 4.17.21", components)
		}

		missing, err := db.GetProject("acme/other")
		if err != nil || missing != nil {
			t.Errorf("GetProject(missing) = %v, %v", missing, err)
		}
	})
}

func TestGetProjectsUsingPackage(t *testing.T) {
	runWithBothDatabases(t, func(t *testing.T, db *DB) {
		now := time.Now()
		saveProject(t, db, "acme/web", now, lodash20, console)
		saveProject(t, db, "acme/api", now, lodash21)
		saveProject(t, db, "acme/cli", now, console)

		all, err := db.GetProjectsUsingPackage("pkg:npm/lodash", "")
		if err != nil {
			t.Fatal(err)
		}
		if got, want := usageNames(all), []string{"acme/api@4.17.21", "acme/web@4.17.20"}; !slices.Equal(got, want) {
			t.Errorf("any version = %v, want %v", got, want)
		}
		one, err := db.GetProjectsUsingPackage("pkg:npm/lodash", "4.17.20")
		if err != nil {
			t.Fatal(err)
		}
		if got, want := usageNames(one), []string{"acme/web@4.17.20"}; !slices.Equal(got, want) {
			t.Errorf("4.17.20 = %v, want %v", got, want)
		}
		none, err := db.GetProjectsUsingPackage("pkg:npm/lodash", "1.0.0")
		if err != nil || len(none) != 0 {
			t.Errorf("unused version = %v, %v", none, err)
		}
	})
}

func TestListAndDeleteProjects(t *testing.T) {
	runWithBothDatabases(t, func(t *testing.T, db *DB) {
		now := time.Now()
		saveProject(t, db, "acme/web", now, lodash20, console)
		saveProject(t, db, "acme/api", now)

		projects, err := db.ListProjects()
		if err != nil {
			t.Fatal(err)
		}
		if len(projects) != 2 || projects[0].Name != "acme/api" || projects[0].Components != 0 || projects[1].Components != 2 {
			t.Fatalf("projects = %+v", projects)
		}

		deleted, err := db.DeleteProject("acme/web")
		if err != nil || !deleted {
			t.Fatalf("DeleteProject = %v, %v", deleted, err)
		}
		deleted, err = db.DeleteProject("acme/web")
		if err != nil || deleted {
			t.Errorf("second DeleteProject = %v, %v, want false", deleted, err)
		}
		var rows int
		if err := db.Get(&rows, `SELECT COUNT(*) FROM project_components`); err != nil {
			t.Fatal(err)
		}
		if rows != 0 {
			t.Errorf("%d component rows left after the delete", rows)
		}
	})
}

func TestPruneProjects(t *testing.T) {
	runWithBothDatabases(t, func(t *testing.T, db *DB) {
		now := time.Now()
		saveProject(t, db, "acme/old", now.Add(-100*24*time.Hour), lodash20)
		saveProject(t, db, "acme/new", now.Add(-time.Hour), lodash21)

		pruned, err := db.PruneProjects(now.Add(-90 * 24 * time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if pruned != 1 {
			t.Errorf("pruned = %d, want 1", pruned)
		}
		projects, err := db.ListProjects()
		if err != nil {
			t.Fatal(err)
		}
		if len(projects) != 1 || projects[0].Name != "acme/new" {
			t.Errorf("projects after prune = %+v", projects)
		}
		usages, err := db.GetProjectsUsingPackage("pkg:npm/lodash", "")
		if err != nil {
			t.Fatal(err)
		}
		if got, want := usageNames(usages), []string{"acme/new@4.17.21"}; !slices.Equal(got, want) {
			t.Errorf("usages after prune = %v, want %v", got, want)
		}
	})
}

func TestProjectComponentsJoinPackages(t *testing.T) {
	runWithBothDatabases(t, func(t *testing.T, db *DB) {
		if err := db.UpsertPackage(&Package{
			PURL: "pkg:npm/lodash", Ecosystem: "npm", Name: "lodash",
			LatestVersion: sql.NullString{String: "4.17.21", Valid: true},
		}); err != nil {
			t.Fatal(err)
		}
		saveProject(t, db, "acme/web", time.Now(), lodash20, console)
		saveProject(t, db, "acme/api", time.Now(), lodash21)

		project, err := db.GetProject("acme/web")
		if err != nil || project == nil {
			t.Fatalf("GetProject = %v, %v", project, err)
		}
		components, err := db.GetProjectComponents(project.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(components) != 2 {
			t.Fatalf("components = %+v", components)
		}
		// Ordered by ecosystem: composer first, which the proxy has no record of.
		if components[0].CachedName.Valid {
			t.Errorf("uncached component has a package record: %+v", components[0])
		}
		if components[1].CachedName.String != "lodash" || components[1].LatestVersion.String != "4.17.21" {
			t.Errorf("cached component = %+v", components[1])
		}

	})
}

func TestMigrateAddsProjects(t *testing.T) {
	runWithBothDatabases(t, func(t *testing.T, db *DB) {
		// A database from before the project inventory.
		for _, table := range []string{"project_components", "projects"} {
			if _, err := db.Exec("DROP TABLE " + table); err != nil {
				t.Fatalf("dropping %s: %v", table, err)
			}
		}
		if _, err := db.Exec(db.Rebind("DELETE FROM migrations WHERE name = ?"), "020_add_projects"); err != nil {
			t.Fatalf("forgetting the migration: %v", err)
		}
		if err := db.MigrateSchema(); err != nil {
			t.Fatalf("MigrateSchema: %v", err)
		}
		saveProject(t, db, "acme/web", time.Now(), lodash20)
		usages, err := db.GetProjectsUsingPackage("pkg:npm/lodash", "4.17.20")
		if err != nil || len(usages) != 1 {
			t.Fatalf("GetProjectsUsingPackage after migration = %v, %v", usages, err)
		}
	})
}
