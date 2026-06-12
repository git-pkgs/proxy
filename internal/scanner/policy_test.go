package scanner

import "testing"

func TestPolicyEvaluate(t *testing.T) {
	policy := Policy{
		BlockAtSeverity: SeverityCritical,
		WarnAtSeverity:  SeverityHigh,
	}
	cases := []struct {
		name        string
		findings    []Finding
		wantAction  Action
		wantHighest Severity
	}{
		{"no findings", nil, ActionAllow, SeverityUnknown},
		{
			"single low",
			[]Finding{{Severity: SeverityLow}},
			ActionAllow,
			SeverityLow,
		},
		{
			"single medium below warn",
			[]Finding{{Severity: SeverityMedium}},
			ActionAllow,
			SeverityMedium,
		},
		{
			"single high triggers warn",
			[]Finding{{Severity: SeverityHigh}},
			ActionWarn,
			SeverityHigh,
		},
		{
			"single critical blocks",
			[]Finding{{Severity: SeverityCritical}},
			ActionBlock,
			SeverityCritical,
		},
		{
			"highest of many wins",
			[]Finding{
				{Severity: SeverityLow},
				{Severity: SeverityHigh},
				{Severity: SeverityMedium},
			},
			ActionWarn,
			SeverityHigh,
		},
		{
			"mix with critical blocks",
			[]Finding{
				{Severity: SeverityLow},
				{Severity: SeverityCritical},
				{Severity: SeverityHigh},
			},
			ActionBlock,
			SeverityCritical,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			action, highest := policy.Evaluate(c.findings)
			if action != c.wantAction {
				t.Errorf("action = %v, want %v", action, c.wantAction)
			}
			if highest != c.wantHighest {
				t.Errorf("highest = %v, want %v", highest, c.wantHighest)
			}
		})
	}
}

// Policy where warn == block: every qualifying finding blocks.
func TestPolicyEvaluateWarnEqualsBlock(t *testing.T) {
	policy := Policy{
		BlockAtSeverity: SeverityHigh,
		WarnAtSeverity:  SeverityHigh,
	}
	action, _ := policy.Evaluate([]Finding{{Severity: SeverityHigh}})
	if action != ActionBlock {
		t.Errorf("expected block, got %v", action)
	}
	action, _ = policy.Evaluate([]Finding{{Severity: SeverityMedium}})
	if action != ActionAllow {
		t.Errorf("expected allow, got %v", action)
	}
}
