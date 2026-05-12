// Package server implements the proxy HTTP server.
package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"strconv"
	"time"

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
func storageProbe(ctx context.Context, s storage.Storage) error {
	suffix, err := randomSuffix()
	if err != nil {
		return &probeError{step: "write", err: fmt.Errorf("generating random suffix: %w", err)}
	}
	path := probePathPrefix + strconv.FormatInt(time.Now().UnixNano(), 10) + "-" + suffix
	payload := []byte(probeMarker + suffix)

	// 1. Store
	size, _, err := s.Store(ctx, path, bytes.NewReader(payload))
	if err != nil {
		return &probeError{step: "write", err: err}
	}
	// 2. Size check
	if size != int64(len(payload)) {
		return &probeError{step: "size", err: fmt.Errorf("wrote %d bytes, expected %d", size, len(payload))}
	}
	// 3. Open
	rc, err := s.Open(ctx, path)
	if err != nil {
		return &probeError{step: "read", err: err}
	}
	defer func() {
		if cerr := rc.Close(); cerr != nil {
			// Logged at the caller level; not fatal.
			_ = cerr
		}
	}()
	// 4. Read all (classify mid-stream errors as read, not verify)
	data, err := io.ReadAll(rc)
	if err != nil {
		return &probeError{step: "read", err: err}
	}
	// 5. Verify
	if !bytes.Equal(data, payload) {
		return &probeError{step: "verify", err: fmt.Errorf("content mismatch")}
	}
	// 6. Delete
	if err := s.Delete(ctx, path); err != nil {
		return &probeError{step: "delete", err: err}
	}
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
