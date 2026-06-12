package scanner

import "testing"

func TestParseSeverity(t *testing.T) {
	cases := []struct {
		in   string
		want Severity
	}{
		{"", SeverityUnknown},
		{"CRITICAL", SeverityCritical},
		{"critical", SeverityCritical},
		{"High", SeverityHigh},
		{"moderate", SeverityMedium},
		{"medium", SeverityMedium},
		{"low", SeverityLow},
		{"unknown", SeverityUnknown},
		{"none", SeverityUnknown},
		{"9.5", SeverityCritical},
		{"7.0", SeverityHigh},
		{"6.9", SeverityMedium},
		{"3.9", SeverityLow},
		{"0.05", SeverityUnknown},
		{"garbage", SeverityUnknown},
	}
	for _, c := range cases {
		got := ParseSeverity(c.in)
		if got != c.want {
			t.Errorf("ParseSeverity(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestSeverityAtLeast(t *testing.T) {
	if !SeverityCritical.AtLeast(SeverityHigh) {
		t.Error("Critical >= High should be true")
	}
	if SeverityLow.AtLeast(SeverityMedium) {
		t.Error("Low >= Medium should be false")
	}
	if !SeverityMedium.AtLeast(SeverityMedium) {
		t.Error("Medium >= Medium should be true")
	}
}

func TestSeverityFromCVSS(t *testing.T) {
	cases := []struct {
		score float64
		want  Severity
	}{
		{0.0, SeverityUnknown},
		{0.09, SeverityUnknown},
		{0.1, SeverityLow},
		{3.99, SeverityLow},
		{4.0, SeverityMedium},
		{6.99, SeverityMedium},
		{7.0, SeverityHigh},
		{8.99, SeverityHigh},
		{9.0, SeverityCritical},
		{10.0, SeverityCritical},
	}
	for _, c := range cases {
		got := SeverityFromCVSS(c.score)
		if got != c.want {
			t.Errorf("SeverityFromCVSS(%v) = %v, want %v", c.score, got, c.want)
		}
	}
}
