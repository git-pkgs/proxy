package server

import (
	"context"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/git-pkgs/proxy/internal/handler"
	"github.com/git-pkgs/proxy/internal/mirror"
)

// blockingImages blocks every mirror until release is closed, and with
// ignoreCancel even past the end of its context.
type blockingImages struct {
	started      atomic.Bool
	release      chan struct{}
	ignoreCancel bool
}

func (b *blockingImages) MirrorImage(ctx context.Context, _ handler.ImageRef, _ []handler.Platform) (handler.ImageResult, error) {
	b.started.Store(true)
	if b.ignoreCancel {
		<-b.release
		return handler.ImageResult{}, nil
	}
	select {
	case <-b.release:
	case <-ctx.Done():
	}
	return handler.ImageResult{}, ctx.Err()
}

func startBlockingSchedule(t *testing.T, images *blockingImages) (*Server, context.CancelFunc) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	m := mirror.New(nil, nil, nil, logger, 1)
	m.Images = images
	// A 1ns interval starts the first run right away.
	s := &Server{logger: logger, scheduler: mirror.NewScheduler(m, []mirror.Schedule{
		{PURLs: []string{"pkg:docker/alpine@3"}, Interval: time.Nanosecond},
	}, logger)}
	ctx, cancel := context.WithCancel(context.Background())
	s.scheduler.Start(ctx)
	deadline := time.Now().Add(5 * time.Second)
	for !images.started.Load() {
		if time.Now().After(deadline) {
			t.Fatal("scheduled run did not start")
		}
		time.Sleep(time.Millisecond)
	}
	return s, cancel
}

func TestWaitForMirrorSchedulesWaitsForRunningRun(t *testing.T) {
	images := &blockingImages{release: make(chan struct{}), ignoreCancel: true}
	s, cancel := startBlockingSchedule(t, images)
	cancel()

	done := make(chan struct{})
	go func() {
		s.waitForMirrorSchedules(context.Background())
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("waitForMirrorSchedules returned while a run was still going")
	case <-time.After(50 * time.Millisecond):
	}
	close(images.release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("waitForMirrorSchedules did not return after the run ended")
	}
}

func TestWaitForMirrorSchedulesGivesUpAtDeadline(t *testing.T) {
	images := &blockingImages{release: make(chan struct{}), ignoreCancel: true}
	s, cancel := startBlockingSchedule(t, images)
	defer close(images.release)
	cancel()

	ctx, stop := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer stop()
	started := time.Now()
	s.waitForMirrorSchedules(ctx)
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Errorf("waitForMirrorSchedules took %v past its deadline", elapsed)
	}
}

func TestWaitForMirrorSchedulesWithoutSchedules(t *testing.T) {
	s := &Server{}
	s.waitForMirrorSchedules(context.Background())
}
