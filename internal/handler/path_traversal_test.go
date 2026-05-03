package handler

import "testing"

func TestContainsPathTraversal(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{"pool/main/n/nginx/nginx_1.0.deb", false},
		{"releases/39/Packages/test.rpm", false},
		{"../etc/passwd", true},
		{"pool/../../etc/passwd", true},
		{"pool/main/../../../etc/shadow", true},
		{"pool/..hidden/file", false}, // ".." as a segment, not "..hidden"
		{"", false},
		{"%2e%2e/etc/passwd", true},
		{"%2e%2e%2fetc%2fpasswd", true},
		{"pool/%2e%2e/%2e%2e/etc/shadow", true},
		{"%2E%2E%2Fetc", true},
		{`..\\etc\\passwd`, true},
		{`pool\\..\\..\\etc`, true},
		{"%2e%2e%5cetc%5cpasswd", true},
		{"pool/%2e%2ehidden/file", false},
		{"pool/%zz/bad-encoding", false},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			got := containsPathTraversal(tt.path)
			if got != tt.want {
				t.Errorf("containsPathTraversal(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}
