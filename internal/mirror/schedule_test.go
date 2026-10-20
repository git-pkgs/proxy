package mirror

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math/rand/v2"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/git-pkgs/proxy/internal/handler"
)

func TestScheduleDelay(t *testing.T) {
	lowest := func(int64) int64 { return 0 }
	highest := func(n int64) int64 { return n - 1 }
	tests := []struct {
		name     string
		interval time.Duration
		first    bool
		randN    func(int64) int64
		want     time.Duration
	}{
		{name: "first run lowest", interval: 6 * time.Hour, first: true, randN: lowest, want: 0},
		{name: "first run capped at ten minutes", interval: 6 * time.Hour, first: true, randN: highest, want: 10*time.Minute - 1},
		{name: "first run within a short interval", interval: 2 * time.Minute, first: true, randN: highest, want: 2*time.Minute - 1},
		{name: "later run lowest", interval: 6 * time.Hour, randN: lowest, want: 6*time.Hour - 36*time.Minute},
		{name: "later run highest", interval: 6 * time.Hour, randN: highest, want: 6*time.Hour + 36*time.Minute},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := scheduleDelay(tt.interval, tt.first, tt.randN); got != tt.want {
				t.Errorf("scheduleDelay() = %v, want %v", got, tt.want)
			}
		})
	}
}

// scheduleImages records MirrorImage calls and can block or fail them.
type scheduleImages struct {
	mu        sync.Mutex
	refs      []handler.ImageRef
	platforms [][]handler.Platform
	calls     atomic.Int32
	running   atomic.Int32
	overlap   atomic.Bool
	block     chan struct{}
	err       error
}

func (f *scheduleImages) MirrorImage(ctx context.Context, ref handler.ImageRef, platforms []handler.Platform) (handler.ImageResult, error) {
	if f.running.Add(1) > 1 {
		f.overlap.Store(true)
	}
	defer f.running.Add(-1)
	f.mu.Lock()
	f.refs = append(f.refs, ref)
	f.platforms = append(f.platforms, platforms)
	f.mu.Unlock()
	f.calls.Add(1)
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return handler.ImageResult{}, ctx.Err()
		}
	}
	return handler.ImageResult{}, f.err
}

func newTestScheduler(images ImageMirrorer, schedules ...Schedule) *Scheduler {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	m := New(nil, nil, nil, logger, 1)
	m.Images = images
	s := NewScheduler(m, schedules, logger)
	s.randN = func(int64) int64 { return 0 }
	return s
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestSchedulerRepeatsRunsAndStops(t *testing.T) {
	images := &scheduleImages{err: errors.New("upstream down")}
	s := newTestScheduler(images, Schedule{
		PURLs:     []string{"pkg:docker/library/alpine@3.20"},
		Platforms: []string{"linux/arm64"},
		Interval:  time.Millisecond,
	})
	ctx, cancel := context.WithCancel(context.Background())
	s.Start(ctx)
	// A failing run must not end the schedule.
	waitFor(t, "three runs", func() bool { return images.calls.Load() >= 3 })
	cancel()
	s.Wait()

	images.mu.Lock()
	defer images.mu.Unlock()
	wantRef := handler.ImageRef{Name: "library/alpine", References: []string{"3.20"}}
	if !reflect.DeepEqual(images.refs[0], wantRef) {
		t.Errorf("ref = %+v, want %+v", images.refs[0], wantRef)
	}
	wantPlatforms := []handler.Platform{{OS: "linux", Architecture: "arm64"}}
	if !reflect.DeepEqual(images.platforms[0], wantPlatforms) {
		t.Errorf("platforms = %+v, want %+v", images.platforms[0], wantPlatforms)
	}
}

func TestSchedulerRunsDoNotOverlap(t *testing.T) {
	images := &scheduleImages{block: make(chan struct{})}
	s := newTestScheduler(images, Schedule{PURLs: []string{"pkg:docker/alpine@3"}, Interval: time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)

	waitFor(t, "first run", func() bool { return images.calls.Load() == 1 })
	// Many intervals pass while the run blocks; none may start a second run.
	time.Sleep(50 * time.Millisecond)
	if got := images.calls.Load(); got != 1 {
		t.Fatalf("calls while the first run blocks = %d, want 1", got)
	}
	images.block <- struct{}{}
	waitFor(t, "second run", func() bool { return images.calls.Load() == 2 })
	cancel()
	s.Wait()
	if images.overlap.Load() {
		t.Error("two runs of one schedule overlapped")
	}
}

func TestSchedulerWaitReturnsAfterCancelDuringRun(t *testing.T) {
	images := &scheduleImages{block: make(chan struct{})}
	s := newTestScheduler(images,
		Schedule{PURLs: []string{"pkg:docker/alpine@3"}, Interval: time.Hour},
		Schedule{PURLs: []string{"pkg:docker/nginx@stable"}, Interval: time.Hour})
	ctx, cancel := context.WithCancel(context.Background())
	s.Start(ctx)
	waitFor(t, "both schedules running", func() bool { return images.calls.Load() == 2 })

	cancel()
	done := make(chan struct{})
	go func() {
		s.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Wait did not return after cancel")
	}
}

func TestSchedulerSkipsScheduleWithoutInterval(t *testing.T) {
	images := &scheduleImages{}
	s := newTestScheduler(images, Schedule{PURLs: []string{"pkg:docker/alpine@3"}})
	// The real random source panics on a zero interval.
	s.randN = rand.Int64N
	ctx, cancel := context.WithCancel(context.Background())
	s.Start(ctx)
	cancel()
	s.Wait()
	if got := images.calls.Load(); got != 0 {
		t.Errorf("calls = %d, want 0", got)
	}
}
