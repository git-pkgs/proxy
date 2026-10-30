package handler

import (
	"reflect"
	"testing"
)

func TestSwiftLinkQueries(t *testing.T) {
	const manifest = "https://registry.example/swift/apple/example/1.0.0/Package.swift"
	link := `</swift/apple/example/1.0.0/Package.swift?swift-version=5.8>; rel="alternate"; filename="Package@swift-5.8.swift", ` +
		`<https://registry.example/swift/apple/example/1.0.0/Package.swift?swift-version=5.7>; rel=alternate, ` +
		`<https://other.example/swift/apple/example/1.0.0/Package.swift?swift-version=5.6>; rel="alternate", ` +
		`<https://registry.example/swift/apple/other/1.0.0/Package.swift?swift-version=5.5>; rel="alternate", ` +
		`<https://github.com/apple/example>; rel="canonical"`

	got := swiftLinkQueries(link, "alternate", manifest)

	want := []string{"swift-version=5.8", "swift-version=5.7"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("swiftLinkQueries() = %q, want %q", got, want)
	}
}
