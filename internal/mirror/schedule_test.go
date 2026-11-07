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

func TestSchedulerSkipsRunClaimedElsewhere(t *testing.T) {
	images := &scheduleImages{}
	schedule := Schedule{PURLs: []string{"pkg:docker/alpine@3"}, Interval: time.Hour}
	s := newTestScheduler(images, schedule)
	var mu sync.Mutex
	var keys []string
	var gaps []time.Duration
	claim := true
	s.Claim = func(key string, minGap time.Duration) (bool, error) {
		mu.Lock()
		defer mu.Unlock()
		keys = append(keys, key)
		gaps = append(gaps, minGap)
		return claim, nil
	}

	s.run(context.Background(), 0, schedule)
	if got := images.calls.Load(); got != 1 {
		t.Fatalf("calls after a claimed run = %d, want 1", got)
	}
	claim = false
	s.run(context.Background(), 0, schedule)
	if got := images.calls.Load(); got != 1 {
		t.Fatalf("calls after a run claimed elsewhere = %d, want 1", got)
	}
	s.Claim = func(string, time.Duration) (bool, error) { return false, errors.New("database down") }
	s.run(context.Background(), 0, schedule)
	if got := images.calls.Load(); got != 1 {
		t.Fatalf("calls after a failed claim = %d, want 1", got)
	}

	if keys[0] != keys[1] {
		t.Errorf("claim keys differ between runs: %q, %q", keys[0], keys[1])
	}
	// Runs come at least 0.9 intervals apart; the gap stays below that.
	if want := 48 * time.Minute; gaps[0] != want {
		t.Errorf("minGap = %s, want %s", gaps[0], want)
	}
}

// TestSchedulerReleasesInterruptedRun checks that a run cut off by shutdown
// does not count as done: the next attempt, on a restarted instance or
// another one, runs the schedule again.
func TestSchedulerReleasesInterruptedRun(t *testing.T) {
	images := &scheduleImages{block: make(chan struct{})}
	schedule := Schedule{PURLs: []string{"pkg:docker/alpine@3"}, Interval: time.Hour}
	s := newTestScheduler(images, schedule)
	// The claims of a shared database: a claimed key is not claimed again.
	var mu sync.Mutex
	claims := map[string]bool{}
	s.Claim = func(key string, _ time.Duration) (bool, error) {
		mu.Lock()
		defer mu.Unlock()
		if claims[key] {
			return false, nil
		}
		claims[key] = true
		return true, nil
	}
	s.Release = func(key string) error {
		mu.Lock()
		defer mu.Unlock()
		delete(claims, key)
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		s.run(ctx, 0, schedule)
		close(done)
	}()
	waitFor(t, "the run to start", func() bool { return images.calls.Load() == 1 })
	cancel()
	<-done

	close(images.block)
	s.run(context.Background(), 0, schedule)
	if got := images.calls.Load(); got != 2 {
		t.Fatalf("calls = %d, want 2: the interrupted run blocked the next one", got)
	}
	// A finished run keeps its claim.
	s.run(context.Background(), 0, schedule)
	if got := images.calls.Load(); got != 2 {
		t.Fatalf("calls = %d, want 2 after a finished run", got)
	}
}

func TestScheduleKey(t *testing.T) {
	base := Schedule{PURLs: []string{"pkg:docker/alpine@3"}, Platforms: []string{"linux/amd64"}, Interval: time.Hour}
	if scheduleKey(base) != scheduleKey(Schedule{PURLs: []string{"pkg:docker/alpine@3"}, Platforms: []string{"linux/amd64"}, Interval: time.Hour}) {
		t.Error("same schedule gave different keys")
	}
	for name, other := range map[string]Schedule{
		"purls":     {PURLs: []string{"pkg:docker/alpine@4"}, Platforms: base.Platforms, Interval: base.Interval},
		"platforms": {PURLs: base.PURLs, Platforms: []string{"linux/arm64"}, Interval: base.Interval},
		"interval":  {PURLs: base.PURLs, Platforms: base.Platforms, Interval: 2 * time.Hour},
	} {
		if scheduleKey(other) == scheduleKey(base) {
			t.Errorf("changing %s kept the key", name)
		}
	}
}

func TestSchedulerRunsRefresh(t *testing.T) {
	m, fetcher := setupRefreshMirror(t, "tarball")
	cacheNPM(t, m, "lodash", "4.17.20")
	m.Versions = stubReleases{"npm/lodash": oldReleases("4.17.20", "4.17.21")}
	fetcher.reset()

	schedule := Schedule{Refresh: &Refresh{Top: 10, Since: time.Hour}, Interval: time.Hour}
	s := NewScheduler(m, []Schedule{schedule}, m.logger)
	s.run(context.Background(), 0, schedule)

	if urls := fetcher.fetched(); len(urls) != 1 || len(fetchedMatching(urls, "lodash-4.17.21.tgz")) != 1 {
		t.Errorf("fetched %v, want lodash 4.17.21", urls)
	}
}

func TestScheduleKeyRefresh(t *testing.T) {
	base := Schedule{Refresh: &Refresh{Top: 50, Since: time.Hour}, Interval: time.Hour}
	same := Schedule{Refresh: &Refresh{Top: 50, Since: time.Hour}, Interval: time.Hour}
	if scheduleKey(base) != scheduleKey(same) {
		t.Error("same refresh gave different keys")
	}
	for name, other := range map[string]Schedule{
		"no refresh": {Interval: time.Hour},
		"top":        {Refresh: &Refresh{Top: 10, Since: time.Hour}, Interval: time.Hour},
		"since":      {Refresh: &Refresh{Top: 50, Since: 2 * time.Hour}, Interval: time.Hour},
		"interval":   {Refresh: &Refresh{Top: 50, Since: time.Hour}, Interval: 2 * time.Hour},
	} {
		if scheduleKey(other) == scheduleKey(base) {
			t.Errorf("changing %s kept the key", name)
		}
	}
}
