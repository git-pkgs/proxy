package handler

import (
	"fmt"
	"io"

	"github.com/git-pkgs/integrity"
)

type integrityChecks struct {
	contentHash integrity.SRI
	native      integrity.SRI
	// checksum is native metadata given as a single algorithm-prefixed hex
	// checksum, such as a registry's SHA-1 shasum, which is not SRI.
	checksum    integrity.Digest
	hasChecksum bool
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
			checksum, checksumErr := integrity.ParseChecksum(native)
			if checksumErr != nil {
				return integrityChecks{}, fmt.Errorf("parse integrity: %w", err)
			}
			checks.checksum, checks.hasChecksum = checksum, true
			checks.algorithms = append(checks.algorithms, checksum.Algorithm())
			return checks, nil
		}
		checks.native = digests
		for _, digest := range digests {
			checks.algorithms = append(checks.algorithms, digest.Algorithm())
		}
	}

	return checks, nil
}

// verifyIntegrity wraps body to check it against the declared integrity as
// it is read. verified, called once body has been read, reports a mismatch as
// ErrArtifactDigestMismatch, and also a body that was not read to its end,
// since only a complete body can be checked. An empty declared checks
// nothing.
func verifyIntegrity(body io.ReadCloser, declared string) (_ io.Reader, verified func() error, _ error) {
	if declared == "" {
		return body, func() error { return nil }, nil
	}
	checks, err := newIntegrityChecks("", declared)
	if err != nil {
		return nil, nil, err
	}
	var mismatch string
	reader, err := checks.newVerifyingReader(body, func(reason string) { mismatch = reason }, false)
	if err != nil {
		return nil, nil, err
	}
	return reader, func() error {
		if !reader.(*verifyingReader).verified {
			return fmt.Errorf("%w: body was not read to its end", ErrArtifactDigestMismatch)
		}
		if mismatch != "" {
			return fmt.Errorf("%w: %s", ErrArtifactDigestMismatch, mismatch)
		}
		return nil
	}, nil
}

func (c integrityChecks) wrap(source io.ReadCloser, onMismatch func(string)) (io.ReadCloser, error) {
	return c.wrapSeekable(source, onMismatch, false)
}

// wrapFailOnMismatch is wrap for bytes that are served to a client: the
// final chunk is held back until the digest is checked, and a mismatch drops
// it and is returned from Read as ErrArtifactDigestMismatch in place of
// io.EOF, so the caller can abort rather than complete the response.
func (c integrityChecks) wrapFailOnMismatch(source io.ReadCloser, onMismatch func(string)) (io.ReadCloser, error) {
	return c.wrapSeekable(source, onMismatch, true)
}

func (c integrityChecks) wrapSeekable(source io.ReadCloser, onMismatch func(string), failOnMismatch bool) (io.ReadCloser, error) {
	if len(c.algorithms) == 0 {
		return source, nil
	}
	verified, err := c.newVerifyingReader(source, onMismatch, failOnMismatch)
	if err != nil {
		return nil, err
	}
	seeker, ok := source.(io.Seeker)
	if !ok {
		return verified, nil
	}
	return &seekableVerifyingReader{
		source:   source,
		verified: verified,
		seeker:   seeker,
	}, nil
}

func (c integrityChecks) newVerifyingReader(source io.ReadCloser, onMismatch func(string), failOnMismatch bool) (io.ReadCloser, error) {
	if len(c.algorithms) == 0 {
		return source, nil
	}
	reader, err := integrity.NewReader(source, c.algorithms...)
	if err != nil {
		return nil, fmt.Errorf("create integrity reader: %w", err)
	}
	return &verifyingReader{
		source:         source,
		reader:         reader,
		checks:         c,
		onMismatch:     onMismatch,
		failOnMismatch: failOnMismatch,
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

	failOnMismatch bool
	mismatched     bool

	// With failOnMismatch, held (backed by buf) is read but not yet
	// returned, and only its first releasable bytes are known not to end the
	// body. The rest is the latest chunk, returned once the digest matches.
	buf        []byte
	held       []byte
	releasable int
	eof        bool
}

// seekableVerifyingReader verifies ordinary reads until a successful seek,
// after which reads bypass whole-object verification for range responses.
type seekableVerifyingReader struct {
	source   io.ReadCloser
	verified io.ReadCloser
	seeker   io.Seeker
	bypass   bool
}

func (r *seekableVerifyingReader) Read(p []byte) (int, error) {
	if r.bypass {
		return r.source.Read(p)
	}
	return r.verified.Read(p)
}

func (r *seekableVerifyingReader) Seek(offset int64, whence int) (int64, error) {
	position, err := r.seeker.Seek(offset, whence)
	if err == nil {
		r.bypass = true
	}
	return position, err
}

func (r *seekableVerifyingReader) Close() error {
	return r.verified.Close()
}

func (r *verifyingReader) Read(p []byte) (int, error) {
	if r.failOnMismatch {
		return r.readHoldingBack(p)
	}
	n, err := r.reader.Read(p)
	if err == io.EOF {
		r.verify()
	}
	return n, err
}

// readHoldingBack returns bytes one chunk behind the source, so the final
// chunk is released only after the digest over the whole body has matched.
func (r *verifyingReader) readHoldingBack(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for r.releasable == 0 && !r.eof {
		// Move the held chunk to the front of the buffer and read the next
		// chunk after it; once that arrives, the held chunk is not the last.
		var pending int
		if cap(r.buf) < len(r.held)+len(p) {
			buf := make([]byte, len(r.held)+len(p))
			pending = copy(buf, r.held)
			r.buf = buf
		} else {
			pending = copy(r.buf[:cap(r.buf)], r.held)
		}
		n, err := r.reader.Read(r.buf[pending : pending+len(p)])
		r.held = r.buf[:pending+n]
		if n > 0 {
			r.releasable = pending
		}
		switch {
		case err == io.EOF:
			r.verify()
			r.eof = true
		case err != nil:
			r.held, r.releasable = nil, 0
			return 0, err
		case n == 0:
			return 0, nil
		}
	}
	if r.eof {
		if r.mismatched {
			r.held, r.releasable = nil, 0
			return 0, ErrArtifactDigestMismatch
		}
		r.releasable = len(r.held)
		if r.releasable == 0 {
			return 0, io.EOF
		}
	}
	n := copy(p, r.held[:r.releasable])
	r.held = r.held[n:]
	r.releasable -= n
	return n, nil
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
			r.mismatched = true
			r.onMismatch("content_hash: " + err.Error())
		}
	}
	if len(r.checks.native) > 0 {
		if err := result.Verify(r.checks.native); err != nil {
			r.mismatched = true
			r.onMismatch("integrity: " + err.Error())
		}
	}
	if r.checks.hasChecksum {
		if err := result.VerifyDigest(r.checks.checksum); err != nil {
			r.mismatched = true
			r.onMismatch("integrity: " + err.Error())
		}
	}
}

// sha1Checksum returns a registry's hexadecimal SHA-1 shasum as the
// "sha1-<hex>" checksum newIntegrityChecks accepts, or "" when it is empty or
// not a SHA-1 digest.
func sha1Checksum(shasum string) string {
	if shasum == "" {
		return ""
	}
	digest, err := integrity.ParseHex(integrity.SHA1, shasum)
	if err != nil {
		return ""
	}
	return digest.Checksum()
}
