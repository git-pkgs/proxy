package handler

import (
	"encoding/base64"
	"fmt"
	"io"
	"strings"

	"github.com/git-pkgs/integrity"
)

type integrityChecks struct {
	contentHash     *integrity.Digest
	native          integrity.SRI
	nativeAlgorithm integrity.Algorithm
	algorithms      []integrity.Algorithm
}

func newIntegrityChecks(contentHash, native string) (integrityChecks, error) {
	checks := integrityChecks{}
	seen := make(map[integrity.Algorithm]bool)
	addAlgorithm := func(algorithm integrity.Algorithm) {
		if seen[algorithm] {
			return
		}
		seen[algorithm] = true
		checks.algorithms = append(checks.algorithms, algorithm)
	}

	if contentHash != "" {
		digest, err := integrity.ParseHex(integrity.SHA256, contentHash)
		if err != nil {
			return integrityChecks{}, fmt.Errorf("parse content_hash: %w", err)
		}
		checks.contentHash = &digest
		addAlgorithm(integrity.SHA256)
	}

	if native != "" {
		digests, err := integrity.ParseSRI(native)
		if err != nil {
			return integrityChecks{}, fmt.Errorf("parse integrity: %w", err)
		}
		checks.native = digests
		checks.nativeAlgorithm = digests[0].Algorithm()
		for _, digest := range digests {
			algorithm := digest.Algorithm()
			addAlgorithm(algorithm)
			if algorithm > checks.nativeAlgorithm {
				checks.nativeAlgorithm = algorithm
			}
		}
	}

	return checks, nil
}

func (c integrityChecks) wrap(source io.ReadCloser, onMismatch func(string)) (io.ReadCloser, error) {
	if len(c.algorithms) == 0 {
		return source, nil
	}
	reader, err := integrity.NewReader(source, c.algorithms...)
	if err != nil {
		return nil, fmt.Errorf("create integrity reader: %w", err)
	}
	return &verifyingReader{
		source:     source,
		reader:     reader,
		checks:     c,
		onMismatch: onMismatch,
	}, nil
}

// verifyingReader forwards Close to its source and reports completed digest
// mismatches after its shared integrity reader observes EOF.
type verifyingReader struct {
	source     io.ReadCloser
	reader     *integrity.Reader
	checks     integrityChecks
	onMismatch func(reason string)
	verified   bool
}

func (r *verifyingReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if err == io.EOF {
		r.verify()
	}
	return n, err
}

func (r *verifyingReader) Close() error {
	return r.source.Close()
}

func (r *verifyingReader) verify() {
	if r.verified {
		return
	}
	r.verified = true
	result := r.reader.Result()
	if !result.Complete {
		return
	}

	if r.checks.contentHash != nil {
		expected := integrity.SRI{*r.checks.contentHash}
		if err := result.Verify(expected); err != nil {
			calculated := calculatedDigest(result, integrity.SHA256)
			r.onMismatch(fmt.Sprintf("content_hash mismatch: stored=%s computed=%s", r.checks.contentHash.Hex(), calculated.Hex()))
		}
	}
	if len(r.checks.native) > 0 {
		if err := result.Verify(r.checks.native); err != nil {
			calculated := calculatedDigest(result, r.checks.nativeAlgorithm)
			r.onMismatch(nativeMismatchReason(r.checks.native, calculated, r.checks.nativeAlgorithm))
		}
	}
}

func calculatedDigest(result integrity.Result, algorithm integrity.Algorithm) integrity.Digest {
	for _, digest := range result.Digests {
		if digest.Algorithm() == algorithm {
			return digest
		}
	}
	return integrity.Digest{}
}

func nativeMismatchReason(expected integrity.SRI, calculated integrity.Digest, algorithm integrity.Algorithm) string {
	encoded := make([]string, 0, len(expected))
	for _, digest := range expected {
		if digest.Algorithm() == algorithm {
			encoded = append(encoded, base64.StdEncoding.EncodeToString(digest.Bytes()))
		}
	}
	return fmt.Sprintf("integrity mismatch: %s expected=%s computed=%s",
		algorithm,
		strings.Join(encoded, ","),
		base64.StdEncoding.EncodeToString(calculated.Bytes()))
}
