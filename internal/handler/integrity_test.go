package handler

import (
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"testing"
)

func sha256Hex(data string) string {
	sum := sha256.Sum256([]byte(data))
	return hex.EncodeToString(sum[:])
}

func sha256SRI(data string) string {
	sum := sha256.Sum256([]byte(data))
	return "sha256-" + base64.StdEncoding.EncodeToString(sum[:])
}

func sha384SRI(data string) string {
	sum := sha512.Sum384([]byte(data))
	return "sha384-" + base64.StdEncoding.EncodeToString(sum[:])
}

func sha512SRI(data string) string {
	sum := sha512.Sum512([]byte(data))
	return "sha512-" + base64.StdEncoding.EncodeToString(sum[:])
}

func wrapIntegrityReader(t *testing.T, source io.ReadCloser, contentHash, native string, onMismatch func(string)) io.ReadCloser {
	t.Helper()
	checks, err := newIntegrityChecks(contentHash, native)
	if err != nil {
		t.Fatalf("newIntegrityChecks: %v", err)
	}
	reader, err := checks.wrap(source, onMismatch)
	if err != nil {
		t.Fatalf("wrap: %v", err)
	}
	return reader
}

func TestNewIntegrityChecksCollectsAlgorithms(t *testing.T) {
	checks, err := newIntegrityChecks(
		sha256Hex("hello"),
		strings.Join([]string{sha256SRI("first"), sha512SRI("second"), sha384SRI("third"), sha512SRI("alternative")}, " "),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(checks.algorithms) != 5 {
		t.Fatalf("algorithms = %v, want 5 entries", checks.algorithms)
	}
	if len(checks.native) != 4 {
		t.Errorf("native digests = %d, want 4", len(checks.native))
	}
}

func TestNewIntegrityChecksRejectsMalformedMetadata(t *testing.T) {
	tests := []struct {
		name        string
		contentHash string
		native      string
	}{
		{name: "short content hash", contentHash: "abc123"},
		{name: "non-hex content hash", contentHash: strings.Repeat("z", sha256.Size*2)},
		{name: "missing SRI separator", native: "sha512"},
		{name: "malformed SRI base64", native: "sha512-not!base64"},
		{name: "wrong SRI length", native: "sha512-" + base64.StdEncoding.EncodeToString([]byte("short"))},
		{name: "unsupported SRI algorithm", native: "md5-1B2M2Y8AsgTpgAmY7PhCfg=="},
		{name: "invalid SRI alternative", native: sha512SRI("valid") + " sha384-nope"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := newIntegrityChecks(test.contentHash, test.native); err == nil {
				t.Fatal("newIntegrityChecks returned nil error")
			}
		})
	}
}

func TestVerifyingReader(t *testing.T) {
	const data = "hello world"
	goodSHA := sha256Hex(data)
	goodSRI := sha512SRI(data)

	tests := []struct {
		name      string
		hash      string
		sri       string
		wantCalls int
	}{
		{name: "both match", hash: goodSHA, sri: goodSRI},
		{name: "SHA-256 only match", hash: goodSHA},
		{name: "SRI only match", sri: goodSRI},
		{name: "SHA-256 mismatch", hash: sha256Hex("other"), wantCalls: 1},
		{name: "SRI mismatch", sri: sha512SRI("other"), wantCalls: 1},
		{name: "both mismatch", hash: sha256Hex("other"), sri: sha512SRI("other"), wantCalls: 2},
		{name: "no checks"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var calls []string
			reader := wrapIntegrityReader(t, io.NopCloser(strings.NewReader(data)), test.hash, test.sri,
				func(reason string) { calls = append(calls, reason) })

			got, err := io.ReadAll(reader)
			if err != nil {
				t.Fatalf("ReadAll: %v", err)
			}
			if string(got) != data {
				t.Errorf("data corrupted: got %q", got)
			}
			if err := reader.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
			if len(calls) != test.wantCalls {
				t.Errorf("onMismatch called %d times, want %d: %v", len(calls), test.wantCalls, calls)
			}
		})
	}
}

func TestVerifyingReaderUsesStrongestNativeAlgorithm(t *testing.T) {
	const data = "artifact"
	tests := []struct {
		name      string
		native    string
		wantCalls int
	}{
		{
			name:      "weaker match does not override stronger mismatch",
			native:    sha256SRI(data) + " " + sha512SRI("other"),
			wantCalls: 1,
		},
		{
			name:   "stronger match ignores weaker mismatch",
			native: sha256SRI("other") + " " + sha512SRI(data),
		},
		{
			name:   "same algorithm alternative matches",
			native: sha512SRI("other") + " " + sha512SRI(data),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var calls int
			reader := wrapIntegrityReader(t, io.NopCloser(strings.NewReader(data)), "", test.native, func(string) { calls++ })
			if _, err := io.Copy(io.Discard, reader); err != nil {
				t.Fatal(err)
			}
			if calls != test.wantCalls {
				t.Errorf("onMismatch called %d times, want %d", calls, test.wantCalls)
			}
		})
	}
}

func TestVerifyingReaderMismatchMessages(t *testing.T) {
	const data = "actual"
	wantHash := sha256Hex("expected")
	wantSRI := sha512SRI("expected")
	var reasons []string
	reader := wrapIntegrityReader(t, io.NopCloser(strings.NewReader(data)), wantHash, wantSRI,
		func(reason string) { reasons = append(reasons, reason) })
	if _, err := io.Copy(io.Discard, reader); err != nil {
		t.Fatal(err)
	}
	if len(reasons) != 2 {
		t.Fatalf("reasons = %v, want two", reasons)
	}
	wantContentReason := "content_hash: integrity mismatch: expected " + sha256SRI("expected") + ", calculated " + sha256SRI(data)
	if reasons[0] != wantContentReason {
		t.Errorf("content reason = %q, want %q", reasons[0], wantContentReason)
	}
	wantNativeReason := "integrity: integrity mismatch: expected " + wantSRI + ", calculated " + sha512SRI(data)
	if reasons[1] != wantNativeReason {
		t.Errorf("native reason = %q, want %q", reasons[1], wantNativeReason)
	}
}

func TestVerifyingReaderPassthrough(t *testing.T) {
	source := io.NopCloser(strings.NewReader("x"))
	reader := wrapIntegrityReader(t, source, "", "", func(string) { t.Fatal("should not be called") })
	if reader != source {
		t.Error("expected passthrough when no hashes were provided")
	}
}

type closeTrackingReader struct {
	io.Reader
	closed bool
}

func (r *closeTrackingReader) Close() error {
	r.closed = true
	return nil
}

func TestVerifyingReaderPartialRead(t *testing.T) {
	source := &closeTrackingReader{Reader: strings.NewReader("hello world")}
	var calls int
	reader := wrapIntegrityReader(t, source, sha256Hex("other"), "", func(string) { calls++ })

	buffer := make([]byte, 5)
	_, _ = reader.Read(buffer)
	_ = reader.Close()

	if calls != 0 {
		t.Errorf("onMismatch called %d times for partial read, want 0", calls)
	}
	if !source.closed {
		t.Error("Close was not forwarded to the source")
	}
}

func TestVerifyingReaderNonEOFError(t *testing.T) {
	var calls int
	reader := wrapIntegrityReader(t, io.NopCloser(errorFixtureReader{}), sha256Hex("data"), "", func(string) { calls++ })
	if _, err := io.ReadAll(reader); !errors.Is(err, errIntegrityReadFixture) {
		t.Fatalf("ReadAll error = %v", err)
	}
	if calls != 0 {
		t.Errorf("onMismatch called %d times after non-EOF error", calls)
	}
}

var errIntegrityReadFixture = errors.New("integrity read fixture")

type errorFixtureReader struct{}

func (errorFixtureReader) Read(p []byte) (int, error) {
	return copy(p, "data"), errIntegrityReadFixture
}

func TestVerifyingReaderVerifyOnce(t *testing.T) {
	var calls int
	reader := wrapIntegrityReader(t, io.NopCloser(strings.NewReader("x")), sha256Hex("y"), "", func(string) { calls++ })
	_, _ = io.ReadAll(reader)
	_ = reader.Close()
	_ = reader.Close()
	if calls != 1 {
		t.Errorf("onMismatch called %d times, want 1", calls)
	}
}
