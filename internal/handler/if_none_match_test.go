package handler

import "testing"

func TestIfNoneMatchHits(t *testing.T) {
	tests := []struct {
		header string
		etag   string
		want   bool
	}{
		{`"abc"`, `"abc"`, true},
		{`"abc"`, `"def"`, false},
		{"", `"abc"`, false},
		{`"abc"`, "", false},
		{"*", `"abc"`, true},
		{"*", "", false},
		{`W/"abc"`, `"abc"`, true},
		{`"abc"`, `W/"abc"`, true},
		{`W/"abc"`, `W/"abc"`, true},
		{`"abc", "def"`, `"def"`, true},
		{`"abc","def"`, `"def"`, true},
		{` "abc" ,  W/"def" `, `"def"`, true},
		{`"abc", "def"`, `"ghi"`, false},
	}

	for _, tt := range tests {
		if got := ifNoneMatchHits(tt.header, tt.etag); got != tt.want {
			t.Errorf("ifNoneMatchHits(%q, %q) = %v, want %v", tt.header, tt.etag, got, tt.want)
		}
	}
}
