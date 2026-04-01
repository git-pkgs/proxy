package mirror

import (
	"context"
	"crypto/rand"
	"fmt"
	"sync"
	"time"
)

// JobState represents the current state of a mirror job.
type JobState string

const (
	JobStatePending  JobState = "pending"
	JobStateRunning  JobState = "running"
	JobStateComplete JobState = "complete"
	JobStateFailed   JobState = "failed"
	JobStateCanceled JobState = "canceled"
)

const jobTTL = 1 * time.Hour
const cleanupInterval = 5 * time.Minute //nolint:mnd // cleanup ticker

// Job represents an async mirror operation.
type Job struct {
	ID        string    `json:"id"`
	State     JobState  `json:"state"`
	Progress  Progress  `json:"progress"`
	CreatedAt time.Time `json:"created_at"`
	Error     string    `json:"error,omitempty"`

	cancel context.CancelFunc
}

// JobRequest is the JSON body for starting a mirror job via the API.
type JobRequest struct {
	PURLs    []string `json:"purls,omitempty"`
	Registry string   `json:"registry,omitempty"`
}

// JobStore manages in-memory mirror jobs.
type JobStore struct {
	mu      sync.RWMutex
	jobs    map[string]*Job
	mirror  *Mirror
	parentCtx context.Context
}

// NewJobStore creates a new job store. The parent context is used as the base
// for all job contexts so that jobs are canceled when the server shuts down.
func NewJobStore(ctx context.Context, m *Mirror) *JobStore {
	return &JobStore{
		jobs:      make(map[string]*Job),
		mirror:    m,
		parentCtx: ctx,
	}
}

// Create starts a new mirror job and returns its ID.
func (js *JobStore) Create(req JobRequest) (string, error) {
	source, err := js.sourceFromRequest(req)
	if err != nil {
		return "", err
	}

	id := newJobID()
	ctx, cancel := context.WithCancel(js.parentCtx)

	job := &Job{
		ID:        id,
		State:     JobStatePending,
		CreatedAt: time.Now(),
		cancel:    cancel,
	}

	js.mu.Lock()
	js.jobs[id] = job
	js.mu.Unlock()

	go js.runJob(ctx, cancel, job, source)

	return id, nil
}

// Get returns a snapshot of a job by ID. The returned copy is safe to
// serialize without holding the lock.
func (js *JobStore) Get(id string) *Job {
	js.mu.RLock()
	defer js.mu.RUnlock()
	job := js.jobs[id]
	if job == nil {
		return nil
	}
	snapshot := *job
	snapshot.cancel = nil // don't leak cancel func
	if len(job.Progress.Errors) > 0 {
		snapshot.Progress.Errors = make([]MirrorError, len(job.Progress.Errors))
		copy(snapshot.Progress.Errors, job.Progress.Errors)
	}
	return &snapshot
}

// Cancel cancels a running job.
func (js *JobStore) Cancel(id string) bool {
	js.mu.Lock()
	defer js.mu.Unlock()

	job := js.jobs[id]
	if job == nil || job.cancel == nil {
		return false
	}

	if job.State != JobStatePending && job.State != JobStateRunning {
		return false
	}

	job.cancel()
	job.State = JobStateCanceled
	return true
}

// Cleanup removes completed/failed/canceled jobs older than jobTTL.
func (js *JobStore) Cleanup() {
	js.mu.Lock()
	defer js.mu.Unlock()
	for id, job := range js.jobs {
		if job.State == JobStateComplete || job.State == JobStateFailed || job.State == JobStateCanceled {
			if time.Since(job.CreatedAt) > jobTTL {
				delete(js.jobs, id)
			}
		}
	}
}

// StartCleanup runs periodic cleanup of old jobs until the context is canceled.
func (js *JobStore) StartCleanup(ctx context.Context) {
	ticker := time.NewTicker(cleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			js.Cleanup()
		}
	}
}

func (js *JobStore) runJob(ctx context.Context, cancel context.CancelFunc, job *Job, source Source) {
	defer cancel()

	js.mu.Lock()
	if job.State == JobStateCanceled {
		js.mu.Unlock()
		return
	}
	job.State = JobStateRunning
	js.mu.Unlock()

	progress, err := js.mirror.Run(ctx, source, func(p Progress) {
		js.mu.Lock()
		defer js.mu.Unlock()
		if job.State == JobStateRunning {
			job.Progress = p
		}
	})

	js.mu.Lock()
	defer js.mu.Unlock()

	// Cancel() may have already set the state; don't overwrite it.
	if job.State == JobStateCanceled {
		return
	}

	if err != nil {
		job.State = JobStateFailed
		job.Error = err.Error()
		return
	}

	job.Progress = *progress
	if progress.Failed > 0 && progress.Completed == 0 {
		job.State = JobStateFailed
	} else {
		job.State = JobStateComplete
	}
}

func (js *JobStore) sourceFromRequest(req JobRequest) (Source, error) { //nolint:ireturn // interface return is the design
	switch {
	case len(req.PURLs) > 0:
		return &PURLSource{PURLs: req.PURLs}, nil
	case req.Registry != "":
		return nil, fmt.Errorf("registry mirroring is not yet implemented; use purls instead")
	default:
		return nil, fmt.Errorf("request must include purls")
	}
}

// newJobID generates a random hex job ID.
func newJobID() string {
	b := make([]byte, 16) //nolint:mnd // 128-bit ID
	_, _ = rand.Read(b)
	return fmt.Sprintf("%x", b)
}
