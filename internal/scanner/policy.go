package scanner

// Action is the outcome of evaluating a policy against a set of findings.
type Action int

const (
	ActionAllow Action = iota
	ActionWarn
	ActionBlock
)

// String returns the lowercase action name for logging and metrics.
func (a Action) String() string {
	switch a {
	case ActionWarn:
		return "warn"
	case ActionBlock:
		return "block"
	default:
		return "allow"
	}
}

// Policy decides whether a set of findings should allow, warn, or block
// an artifact based on severity thresholds.
type Policy struct {
	// BlockAtSeverity is the minimum severity that causes ActionBlock.
	// Findings strictly less severe than this are not blocking.
	BlockAtSeverity Severity

	// WarnAtSeverity is the minimum severity that causes ActionWarn when
	// no finding triggers a block. Set equal to BlockAtSeverity to
	// disable the warn level effectively (all qualifying findings block).
	WarnAtSeverity Severity
}

// Evaluate inspects findings and returns the action to take plus the
// highest severity observed across all findings.
func (p Policy) Evaluate(findings []Finding) (Action, Severity) {
	highest := SeverityUnknown
	for _, f := range findings {
		if f.Severity > highest {
			highest = f.Severity
		}
	}
	switch {
	case highest.AtLeast(p.BlockAtSeverity):
		return ActionBlock, highest
	case highest.AtLeast(p.WarnAtSeverity):
		return ActionWarn, highest
	default:
		return ActionAllow, highest
	}
}
