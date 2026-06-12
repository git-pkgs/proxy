package scanner

import (
	"errors"
	"fmt"
)

// ErrArtifactQuarantined is the sentinel returned by the pipeline when a
// blocking policy action fires. Handlers use errors.Is to map this to an
// HTTP 451 response.
var ErrArtifactQuarantined = errors.New("artifact quarantined by scanner")

// QuarantineError carries scanner findings alongside the quarantine
// signal so HTTP handlers can surface severity and finding counts to
// clients without re-running scanners.
type QuarantineError struct {
	Highest  Severity
	Findings []Finding
}

func (e *QuarantineError) Error() string {
	return fmt.Sprintf("artifact quarantined (severity=%s, findings=%d)", e.Highest, len(e.Findings))
}

// Unwrap lets errors.Is(err, ErrArtifactQuarantined) succeed.
func (e *QuarantineError) Unwrap() error {
	return ErrArtifactQuarantined
}
