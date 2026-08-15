package packageurl

import "testing"

func TestMakeStringSwiftNamespace(t *testing.T) {
	got := MakeString("swift", "apple/swift-argument-parser", "1.8.2")
	want := "pkg:swift/apple/swift-argument-parser@1.8.2"
	if got != want {
		t.Errorf("MakeString() = %q, want %q", got, want)
	}
}

func TestMakeStringSwiftNestedNamespace(t *testing.T) {
	got := MakeString("swift", "github.com/apple/swift-package-manager", "1.7.0")
	want := "pkg:swift/github.com/apple/swift-package-manager@1.7.0"
	if got != want {
		t.Errorf("MakeString() = %q, want %q", got, want)
	}
}

func TestMakeStringDelegatesOtherEcosystems(t *testing.T) {
	got := MakeString("npm", "@babel/core", "7.23.0")
	want := "pkg:npm/%40babel/core@7.23.0"
	if got != want {
		t.Errorf("MakeString() = %q, want %q", got, want)
	}
}
