package server

import (
	"strings"
	"testing"
)

func TestStartRejectsInvalidCooldownPatterns(t *testing.T) {
	for _, tc := range []struct {
		name     string
		patterns map[string]string
		message  string
	}{
		{"glob", map[string]string{"pkg:npm/[": "0"}, "invalid cooldown package pattern"},
		{"duration", map[string]string{"pkg:npm/*": "invalid"}, "invalid cooldown duration"},
		{"aliases", map[string]string{"pkg:npm/@example/*": "0", "pkg:npm/%40example/*": "7d"}, "conflicting cooldown package patterns"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer(t)
			defer s.close()
			s.server.cfg.Cooldown.PackagePatterns = tc.patterns
			if err := s.server.Start(); err == nil || !strings.Contains(err.Error(), tc.message) {
				t.Fatalf("Start error = %v, want %q", err, tc.message)
			}
		})
	}
}
