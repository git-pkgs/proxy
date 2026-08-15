package handler

import (
	"fmt"
	"io"

	"github.com/git-pkgs/integrity"
)

type integrityChecks struct {
	contentHash integrity.SRI
	native      integrity.SRI
	algorithms  []integrity.Algorithm
}

func newIntegrityChecks(contentHash, native string) (integrityChecks, error) {
	checks := integrityChecks{}

	if contentHash != "" {
		digest, err := integrity.ParseHex(integrity.SHA256, contentHash)
		if err != nil {
			return integrityChecks{}, fmt.Errorf("parse content_hash: %w", err)
		}
		checks.contentHash = integrity.SRI{digest}
		checks.algorithms = append(checks.algorithms, integrity.SHA256)
	}

	if native != "" {
		digests, err := integrity.ParseSRI(native)
		if err != nil {
			return integrityChecks{}, fmt.Errorf("parse integrity: %w", err)
		}
		checks.native = digests
		for _, digest := range digests {
			checks.algorithms = append(checks.algorithms, digest.Algorithm())
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

	if len(r.checks.contentHash) > 0 {
		if err := result.Verify(r.checks.contentHash); err != nil {
			r.onMismatch("content_hash: " + err.Error())
		}
	}
	if len(r.checks.native) > 0 {
		if err := result.Verify(r.checks.native); err != nil {
			r.onMismatch("integrity: " + err.Error())
		}
	}
}
