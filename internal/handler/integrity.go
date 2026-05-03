package handler

import (
	"crypto/sha256"
	"crypto/sha512"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"strings"
)

// parseSRI parses a Subresource Integrity string (e.g. "sha512-abc==") into
// an algorithm name and raw digest bytes. Returns ok=false for empty,
// malformed, or unsupported entries. Only the first hash in a multi-hash
// SRI string is considered.
func parseSRI(s string) (algo string, digest []byte, ok bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", nil, false
	}
	if i := strings.IndexByte(s, ' '); i >= 0 {
		s = s[:i]
	}
	algo, b64, found := strings.Cut(s, "-")
	if !found {
		return "", nil, false
	}
	d, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return "", nil, false
	}
	switch algo {
	case "sha256", "sha384", "sha512":
		return algo, d, true
	default:
		return "", nil, false
	}
}

func newSRIHash(algo string) hash.Hash {
	switch algo {
	case "sha256":
		return sha256.New()
	case "sha384":
		return sha512.New384()
	case "sha512":
		return sha512.New()
	}
	return nil
}

// verifyingReader wraps an io.ReadCloser and computes SHA256 (and optionally
// a second SRI hash) as bytes are read. When the underlying reader reaches
// EOF it compares the digests against the expected values and calls
// onMismatch for each failure. Verification is skipped if the stream was
// not fully consumed (e.g. client disconnect) to avoid false positives.
type verifyingReader struct {
	r          io.ReadCloser
	sha256     hash.Hash
	wantSHA256 string
	sri        hash.Hash
	sriAlgo    string
	wantSRI    []byte
	onMismatch func(reason string)
	eof        bool
	verified   bool
}

func newVerifyingReader(r io.ReadCloser, contentHash, sri string, onMismatch func(string)) io.ReadCloser {
	if contentHash == "" && sri == "" {
		return r
	}
	v := &verifyingReader{
		r:          r,
		onMismatch: onMismatch,
	}
	if contentHash != "" {
		v.sha256 = sha256.New()
		v.wantSHA256 = contentHash
	}
	if algo, digest, ok := parseSRI(sri); ok {
		v.sri = newSRIHash(algo)
		v.sriAlgo = algo
		v.wantSRI = digest
	}
	if v.sha256 == nil && v.sri == nil {
		return r
	}
	return v
}

func (v *verifyingReader) Read(p []byte) (int, error) {
	n, err := v.r.Read(p)
	if n > 0 {
		if v.sha256 != nil {
			v.sha256.Write(p[:n])
		}
		if v.sri != nil {
			v.sri.Write(p[:n])
		}
	}
	if err == io.EOF {
		v.eof = true
		v.verify()
	}
	return n, err
}

func (v *verifyingReader) Close() error {
	if v.eof {
		v.verify()
	}
	return v.r.Close()
}

func (v *verifyingReader) verify() {
	if v.verified {
		return
	}
	v.verified = true

	if v.sha256 != nil {
		got := hex.EncodeToString(v.sha256.Sum(nil))
		if subtle.ConstantTimeCompare([]byte(got), []byte(v.wantSHA256)) != 1 {
			v.onMismatch(fmt.Sprintf("content_hash mismatch: stored=%s computed=%s", v.wantSHA256, got))
		}
	}
	if v.sri != nil {
		got := v.sri.Sum(nil)
		if subtle.ConstantTimeCompare(got, v.wantSRI) != 1 {
			v.onMismatch(fmt.Sprintf("integrity mismatch: %s expected=%s computed=%s",
				v.sriAlgo,
				base64.StdEncoding.EncodeToString(v.wantSRI),
				base64.StdEncoding.EncodeToString(got)))
		}
	}
}
