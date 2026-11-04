package handler

import (
	"errors"
	"io"
)

// upstreamBodyReader marks a failure to read a document from upstream, so a
// body upstream cuts off is told apart from one that ends, and from a
// failure to store it.
type upstreamBodyReader struct{ r io.Reader }

func (u upstreamBodyReader) Read(b []byte) (int, error) {
	n, err := u.r.Read(b)
	if err != nil && !errors.Is(err, io.EOF) {
		err = &upstreamBodyError{err}
	}
	return n, err
}

// upstreamBodyError is a failure to read a document from upstream. It is
// handled as an upstream failure: the stored copy, if any, is served.
type upstreamBodyError struct{ err error }

func (e *upstreamBodyError) Error() string { return "reading upstream metadata: " + e.err.Error() }
func (e *upstreamBodyError) Unwrap() error { return e.err }
