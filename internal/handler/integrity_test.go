package handler

import (
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"io"
	"strings"
	"testing"
)

func sha256Hex(data string) string {
	sum := sha256.Sum256([]byte(data))
	return hex.EncodeToString(sum[:])
}

func sha512SRI(data string) string {
	sum := sha512.Sum512([]byte(data))
	return "sha512-" + base64.StdEncoding.EncodeToString(sum[:])
}

func TestParseSRI(t *testing.T) {
	tests := []struct {
		name  string
		input string
		algo  string
		ok    bool
	}{
		{"sha512", sha512SRI("hello"), "sha512", true},
		{"sha256", "sha256-" + base64.StdEncoding.EncodeToString([]byte("0123456789012345678901234567890123456789")), "sha256", true},
		{"empty", "", "", false},
		{"no dash", "sha512abc", "", false},
		{"bad base64", "sha512-not!base64", "", false},
		{"unsupported algo", "md5-" + base64.StdEncoding.EncodeToString([]byte("x")), "", false},
		{"multi hash takes first", sha512SRI("a") + " " + sha512SRI("b"), "sha512", true},
		{"whitespace", "  " + sha512SRI("x") + "  ", "sha512", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			algo, digest, ok := parseSRI(tt.input)
			if ok != tt.ok {
				t.Fatalf("ok = %v, want %v", ok, tt.ok)
			}
			if !tt.ok {
				return
			}
			if algo != tt.algo {
				t.Errorf("algo = %q, want %q", algo, tt.algo)
			}
			if len(digest) == 0 {
				t.Error("digest is empty")
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
		{"both match", goodSHA, goodSRI, 0},
		{"sha256 only match", goodSHA, "", 0},
		{"sri only match", "", goodSRI, 0},
		{"sha256 mismatch", sha256Hex("other"), "", 1},
		{"sri mismatch", "", sha512SRI("other"), 1},
		{"both mismatch", sha256Hex("other"), sha512SRI("other"), 2},
		{"no checks", "", "", 0},
		{"unparseable sri ignored", goodSHA, "garbage", 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls []string
			r := newVerifyingReader(io.NopCloser(strings.NewReader(data)), tt.hash, tt.sri,
				func(reason string) { calls = append(calls, reason) })

			got, err := io.ReadAll(r)
			if err != nil {
				t.Fatalf("ReadAll: %v", err)
			}
			if string(got) != data {
				t.Errorf("data corrupted: got %q", got)
			}
			if err := r.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}

			if len(calls) != tt.wantCalls {
				t.Errorf("onMismatch called %d times, want %d: %v", len(calls), tt.wantCalls, calls)
			}
		})
	}
}

func TestVerifyingReaderPassthrough(t *testing.T) {
	src := io.NopCloser(strings.NewReader("x"))
	r := newVerifyingReader(src, "", "", func(string) { t.Fatal("should not be called") })
	if r != src {
		t.Error("expected passthrough when no hashes provided")
	}
}

func TestVerifyingReaderPartialRead(t *testing.T) {
	var calls int
	r := newVerifyingReader(io.NopCloser(strings.NewReader("hello world")),
		sha256Hex("hello world"), "", func(string) { calls++ })

	buf := make([]byte, 5)
	_, _ = r.Read(buf)
	_ = r.Close()

	if calls != 0 {
		t.Errorf("onMismatch called %d times for partial read, want 0", calls)
	}
}

func TestVerifyingReaderVerifyOnce(t *testing.T) {
	var calls int
	r := newVerifyingReader(io.NopCloser(strings.NewReader("x")), sha256Hex("y"), "",
		func(string) { calls++ })
	_, _ = io.ReadAll(r)
	_ = r.Close()
	_ = r.Close()
	if calls != 1 {
		t.Errorf("onMismatch called %d times, want 1", calls)
	}
}
