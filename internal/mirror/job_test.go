package mirror

import (
	"context"
	"testing"
	"time"
)

func TestJobStoreCreateAndGet(t *testing.T) {
	m := setupTestMirror(t, 1)
	js := NewJobStore(context.Background(), m)

	id, err := js.Create(JobRequest{
		PURLs: []string{"pkg:npm/lodash@4.17.21"},
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	if id == "" {
		t.Fatal("expected non-empty job ID")
	}

	// Wait for the job to start (it runs async)
	time.Sleep(100 * time.Millisecond)

	job := js.Get(id)
	if job == nil {
		t.Fatal("Get() returned nil")
	}
	if job.ID != id {
		t.Errorf("job ID = %q, want %q", job.ID, id)
	}
}

func TestJobStoreGetNotFound(t *testing.T) {
	m := setupTestMirror(t, 1)
	js := NewJobStore(context.Background(), m)

	job := js.Get("nonexistent")
	if job != nil {
		t.Errorf("expected nil for nonexistent job, got %v", job)
	}
}

func TestJobStoreCancelNotFound(t *testing.T) {
	m := setupTestMirror(t, 1)
	js := NewJobStore(context.Background(), m)

	if js.Cancel("nonexistent") {
		t.Error("expected Cancel to return false for nonexistent job")
	}
}

func TestJobStoreCreateInvalidRequest(t *testing.T) {
	m := setupTestMirror(t, 1)
	js := NewJobStore(context.Background(), m)

	_, err := js.Create(JobRequest{})
	if err == nil {
		t.Fatal("expected error for empty request")
	}
}

func TestJobStoreMultipleJobs(t *testing.T) {
	m := setupTestMirror(t, 1)
	js := NewJobStore(context.Background(), m)

	id1, err := js.Create(JobRequest{PURLs: []string{"pkg:npm/lodash@4.17.21"}})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	id2, err := js.Create(JobRequest{PURLs: []string{"pkg:cargo/serde@1.0.0"}})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	if id1 == id2 {
		t.Error("expected different job IDs")
	}

	job1 := js.Get(id1)
	job2 := js.Get(id2)
	if job1 == nil || job2 == nil {
		t.Fatal("expected both jobs to exist")
	}
}

func TestSourceFromRequestPURLs(t *testing.T) {
	m := setupTestMirror(t, 1)
	js := NewJobStore(context.Background(), m)

	source, err := js.sourceFromRequest(JobRequest{PURLs: []string{"pkg:npm/lodash@1.0.0"}})
	if err != nil {
		t.Fatalf("sourceFromRequest() error = %v", err)
	}
	if _, ok := source.(*PURLSource); !ok {
		t.Errorf("expected *PURLSource, got %T", source)
	}
}

func TestSourceFromRequestRegistryRejected(t *testing.T) {
	m := setupTestMirror(t, 1)
	js := NewJobStore(context.Background(), m)

	_, err := js.sourceFromRequest(JobRequest{Registry: "npm"})
	if err == nil {
		t.Fatal("expected error for registry request")
	}
}

func TestJobStoreCleanup(t *testing.T) {
	m := setupTestMirror(t, 1)
	js := NewJobStore(context.Background(), m)

	// Add a completed job with old CreatedAt
	js.mu.Lock()
	js.jobs["old-job"] = &Job{
		ID:        "old-job",
		State:     JobStateComplete,
		CreatedAt: time.Now().Add(-2 * time.Hour),
	}
	js.jobs["recent-job"] = &Job{
		ID:        "recent-job",
		State:     JobStateComplete,
		CreatedAt: time.Now(),
	}
	js.jobs["running-job"] = &Job{
		ID:        "running-job",
		State:     JobStateRunning,
		CreatedAt: time.Now().Add(-2 * time.Hour),
	}
	js.mu.Unlock()

	js.Cleanup()

	if js.Get("old-job") != nil {
		t.Error("expected old completed job to be cleaned up")
	}
	if js.Get("recent-job") == nil {
		t.Error("expected recent completed job to be kept")
	}
	if js.Get("running-job") == nil {
		t.Error("expected running job to be kept regardless of age")
	}
}

func TestJobStoreCancelPreservesStateAfterRunJob(t *testing.T) {
	m := setupTestMirror(t, 1)
	js := NewJobStore(context.Background(), m)

	// Create a job with a PURL that will fail (no real upstream in test)
	id, err := js.Create(JobRequest{PURLs: []string{"pkg:npm/nonexistent-pkg@0.0.0"}})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	// Cancel immediately -- the job may already be running
	js.Cancel(id)

	// Wait for runJob goroutine to finish
	time.Sleep(200 * time.Millisecond)

	job := js.Get(id)
	if job == nil {
		t.Fatal("Get() returned nil")
	}
	if job.State != JobStateCanceled {
		t.Errorf("state = %q, want %q (cancel should not be overwritten by runJob)", job.State, JobStateCanceled)
	}
}

func TestNewJobIDUnique(t *testing.T) {
	ids := make(map[string]bool)
	for range 100 {
		id := newJobID()
		if ids[id] {
			t.Fatalf("duplicate job ID: %s", id)
		}
		ids[id] = true
	}
}
