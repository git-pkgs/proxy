package gem

import (
	"slices"
	"strings"
	"testing"
	"time"
)

const index = "---\n" +
	"1 |checksum:zzz\n" +
	"1.0.0 |checksum:aaa\n" +
	"1.1.0 dep:>= 1|checksum:bbb\n" +
	"\n" +
	"2.0.0-x86_64-linux |checksum:ccc\n" +
	"2.0.0"

func TestIndexVersions(t *testing.T) {
	got := IndexVersions([]byte(index))
	want := []string{"1", "1.0.0", "1.1.0", "2.0.0-x86_64-linux", "2.0.0"}
	if !slices.Equal(got, want) {
		t.Errorf("IndexVersions = %q, want %q", got, want)
	}
	if got := IndexVersions(nil); got != nil {
		t.Errorf("IndexVersions(nil) = %q, want nil", got)
	}
}

func TestFilterIndex(t *testing.T) {
	var reported []string
	out, kept := FilterIndex([]byte(index), func(v string) bool {
		return v == "1.1.0" || v == "2.0.0-x86_64-linux"
	}, func(v string) { reported = append(reported, v) })

	wantOut := "---\n1 |checksum:zzz\n1.0.0 |checksum:aaa\n\n2.0.0\n"
	if string(out) != wantOut {
		t.Errorf("FilterIndex output = %q, want %q", out, wantOut)
	}
	if want := []string{"1", "1.0.0", "2.0.0"}; !slices.Equal(kept, want) {
		t.Errorf("kept = %q, want %q", kept, want)
	}
	if want := []string{"1.1.0", "2.0.0-x86_64-linux"}; !slices.Equal(reported, want) {
		t.Errorf("onWithheld got %q, want %q", reported, want)
	}
}

func TestFilterIndexWithholdsNothing(t *testing.T) {
	out, kept := FilterIndex([]byte(index), func(string) bool { return false }, nil)
	if string(out) != index+"\n" {
		t.Errorf("FilterIndex output = %q, want the index with a final newline", out)
	}
	if len(kept) != 5 {
		t.Errorf("kept = %q, want 5 versions", kept)
	}
}

func TestFilterIndexNeverAsksAboutHeaderOrBlankLines(t *testing.T) {
	FilterIndex([]byte(index), func(v string) bool {
		if v == "---" || v == "" || strings.Contains(v, " ") {
			t.Errorf("withheld asked about %q", v)
		}
		return false
	}, nil)
}

func TestFilterIndexWithoutOnWithheld(t *testing.T) {
	out, _ := FilterIndex([]byte("1.0.0 |x\n"), func(string) bool { return true }, nil)
	if string(out) != "" {
		t.Errorf("FilterIndex output = %q, want empty", out)
	}
}

func TestPublishTimes(t *testing.T) {
	body := `[
		{"number":"1.0.0","platform":"ruby","created_at":"2024-01-02T03:04:05Z"},
		{"number":"2.0.0","platform":"x86_64-linux","created_at":"2024-02-03T04:05:06.000Z"},
		{"number":"3.0.0","platform":"","created_at":"not a time"}
	]`
	times, err := PublishTimes([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]time.Time{
		"1.0.0":              time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC),
		"2.0.0-x86_64-linux": time.Date(2024, 2, 3, 4, 5, 6, 0, time.UTC),
		"3.0.0":              {},
	}
	if len(times) != len(want) {
		t.Fatalf("PublishTimes = %v, want %v", times, want)
	}
	for v, w := range want {
		if got, ok := times[v]; !ok || !got.Equal(w) {
			t.Errorf("PublishTimes[%q] = %v (%v), want %v", v, got, ok, w)
		}
	}
	if _, err := PublishTimes([]byte("{")); err == nil {
		t.Error("PublishTimes accepted invalid JSON")
	}
}

func TestFilenameIdentity(t *testing.T) {
	tests := []struct{ filename, name, version string }{
		{"rails-7.1.0.gem", "rails", "7.1.0"},
		{"aws-sdk-s3-1.142.0.gem", "aws-sdk-s3", "1.142.0"},
		{"sorbet-static-0.5.11002-universal-darwin-22.gem", "sorbet-static", "0.5.11002-universal-darwin-22"},
		{"devise-2fa-1.0.0.gem", "devise-2fa", "1.0.0"},
		{"jruby-launcher-1.1.19-java.gem", "jruby-launcher", "1.1.19-java"},
		{"nokogiri-1.16.0-universal-java-17.gem", "nokogiri", "1.16.0-universal-java-17"},
		{"foo-1.0.0.pre.1.gem", "foo", "1.0.0.pre.1"},
		{"odd-1-+.gem", "odd", "1-+"},
		{"a-1-2-+.gem", "a", "1-2-+"},
		{"noversion.gem", "", ""},
		{"x" + strings.Repeat("-1", 300) + ".gem", "", ""},
	}
	for _, tt := range tests {
		name, version := FilenameIdentity(tt.filename)
		if name != tt.name || version != tt.version {
			t.Errorf("FilenameIdentity(%q) = (%q, %q), want (%q, %q)", tt.filename, name, version, tt.name, tt.version)
		}
	}
}

func TestFilenameIdentityLengthBound(t *testing.T) {
	name := strings.Repeat("a", maxFilenameLength-len("-1.0.gem"))
	if n, v := FilenameIdentity(name + "-1.0.gem"); n != name || v != "1.0" {
		t.Errorf("FilenameIdentity at the bound = (%q, %q)", n, v)
	}
	if n, v := FilenameIdentity(name + "a-1.0.gem"); n != "" || v != "" {
		t.Errorf("FilenameIdentity past the bound = (%q, %q), want empty", n, v)
	}
}
