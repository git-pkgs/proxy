package server

import (
	"context"
	"testing"
	"time"

	"github.com/git-pkgs/proxy/internal/config"
)

func TestProjectPruningRemovesStaleProjects(t *testing.T) {
	ts := newProjectsTestServer(t)
	ts.server.cfg.Projects = config.ProjectsConfig{Enabled: true, Token: testProjectsToken, MaxAge: "30d"}
	if err := ts.server.cfg.Projects.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := ts.db.SaveProject("acme/old", "", "", time.Now().Add(-31*24*time.Hour), nil); err != nil {
		t.Fatal(err)
	}
	if err := ts.db.SaveProject("acme/new", "", "", time.Now(), nil); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	ts.server.startProjectPruning(ctx)
	defer func() {
		cancel()
		ts.server.background.Wait()
	}()

	deadline := time.Now().Add(5 * time.Second)
	for {
		old, err := ts.db.GetProject("acme/old")
		if err != nil {
			t.Fatal(err)
		}
		if old == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("acme/old was not pruned")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if current, err := ts.db.GetProject("acme/new"); err != nil || current == nil {
		t.Errorf("acme/new was pruned: %v, %v", current, err)
	}
}
