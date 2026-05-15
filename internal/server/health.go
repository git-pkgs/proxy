// Package server implements the proxy HTTP server.
package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/git-pkgs/proxy/internal/metrics"
	"github.com/git-pkgs/proxy/internal/storage"
)

const (
	probePathPrefix     = ".healthcheck/"
	probeMarker         = "proxy-healthcheck:"
	probeSuffixBytes    = 8
	defaultProbeTTL     = 30 * time.Second
	defaultProbeTimeout = 10 * time.Second
)

// HealthResponse is the JSON payload returned by /health.
type HealthResponse struct {
	Status string                 `json:"status"`
	Checks map[string]HealthCheck `json:"checks"`
}

// HealthCheck reports the status of a single subsystem check.
type HealthCheck struct {
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
	Step   string `json:"step,omitempty"`
}

// probeError tags a storage probe failure with the step that failed.
type probeError struct {
	step string
	err  error
}

func (e *probeError) Error() string { return e.step + ": " + e.err.Error() }
func (e *probeError) Unwrap() error { return e.err }

// storageProbe runs a write → size-check → read → verify → delete round-trip
// against the storage backend. Returns nil on success or a *probeError on failure.
func storageProbe(ctx context.Context, s storage.Storage) (err error) {
	suffix, suffixErr := randomSuffix()
	if suffixErr != nil {
		return &probeError{step: "write", err: fmt.Errorf("generating random suffix: %w", suffixErr)}
	}
	path := probePathPrefix + strconv.FormatInt(time.Now().UnixNano(), 10) + "-" + suffix
	payload := []byte(probeMarker + suffix)

	// 1. Store
	size, _, storeErr := s.Store(ctx, path, bytes.NewReader(payload))
	if storeErr != nil {
		return &probeError{step: "write", err: storeErr}
	}
	// After Store succeeds, always attempt to delete on the way out so probe
	// objects don't accumulate when a later step (size/open/read/verify) fails.
	// Delete is reported as the primary error only if no earlier failure
	// already set one.
	defer func() {
		if delErr := s.Delete(ctx, path); delErr != nil && err == nil {
			err = &probeError{step: "delete", err: delErr}
		}
	}()
	// 2. Size check
	if size != int64(len(payload)) {
		return &probeError{step: "size", err: fmt.Errorf("wrote %d bytes, expected %d", size, len(payload))}
	}
	// 3. Open
	rc, openErr := s.Open(ctx, path)
	if openErr != nil {
		return &probeError{step: "read", err: openErr}
	}
	// 4. Read all (classify mid-stream errors as read, not verify).
	// Close explicitly (not deferred) so the file handle is released before
	// Delete — on Windows, an open handle prevents deletion.
	data, readErr := io.ReadAll(rc)
	_ = rc.Close()
	if readErr != nil {
		return &probeError{step: "read", err: readErr}
	}
	// 5. Verify
	if !bytes.Equal(data, payload) {
		return &probeError{step: "verify", err: fmt.Errorf("content mismatch")}
	}
	// 6. Delete is handled via the deferred cleanup above.
	return nil
}

// randomSuffix returns 8 cryptographically random bytes hex-encoded.
func randomSuffix() (string, error) {
	b := make([]byte, probeSuffixBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// healthCache memoizes the result of storageProbe for a configurable TTL.
// It is safe for concurrent use.
type healthCache struct {
	storage      storage.Storage
	interval     time.Duration
	probeTimeout time.Duration
	logger       *slog.Logger

	mu      sync.Mutex
	lastAt  time.Time
	lastErr error
}

// newHealthCache builds a cache, parsing the interval from a duration string.
// Empty interval string defaults to 30s. "0" or "0s" disables caching.
func newHealthCache(s storage.Storage, intervalStr string, logger *slog.Logger) (*healthCache, error) {
	interval := defaultProbeTTL
	if intervalStr != "" {
		d, err := time.ParseDuration(intervalStr)
		if err != nil {
			return nil, fmt.Errorf("parsing storage_probe_interval %q: %w", intervalStr, err)
		}
		interval = d
	}
	return &healthCache{
		storage:      s,
		interval:     interval,
		probeTimeout: defaultProbeTimeout,
		logger:       logger,
	}, nil
}

// Check returns the cached probe result if still fresh, otherwise runs a fresh probe.
// The probe runs under a context derived from context.Background() with a fixed
// timeout so that caller cancellation (e.g. client disconnect) cannot poison the
// cache with context.Canceled.
func (c *healthCache) Check() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Cache hit
	if c.interval > 0 && !c.lastAt.IsZero() && time.Since(c.lastAt) < c.interval {
		return c.lastErr
	}

	// Fresh probe under a detached context
	probeCtx, cancel := context.WithTimeout(context.Background(), c.probeTimeout)
	defer cancel()
	err := storageProbe(probeCtx, c.storage)

	// Transition logging and metric increment happen only on the fresh-probe path.
	c.logTransition(c.lastErr, err)
	if err != nil {
		var pe *probeError
		if errors.As(err, &pe) {
			metrics.RecordHealthProbeFailure(pe.step)
		} else {
			metrics.RecordHealthProbeFailure("unknown")
		}
	}

	c.lastErr = err
	c.lastAt = time.Now()
	return err
}

func (c *healthCache) logTransition(prev, curr error) {
	switch {
	case prev != nil && curr == nil:
		c.logger.Info("storage probe recovered")
	case prev == nil && curr != nil:
		c.logger.Error("storage probe failed", "error", curr.Error())
	}
}
