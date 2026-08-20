package packageurl

import "testing"

func TestMakeSwiftRegistryIdentityUnsupported(t *testing.T) {
	identities := []string{"apple.swift-argument-parser", "apple/swift-argument-parser"}
	for _, identity := range identities {
		t.Run(identity, func(t *testing.T) {
			if got := Make("swift", identity, "1.8.2"); got != nil {
				t.Errorf("Make() = %q, want nil", got.String())
			}
			if got := MakeString("swift", identity, "1.8.2"); got != "" {
				t.Errorf("MakeString() = %q, want empty string", got)
			}
		})
	}
}

func TestMakeStringSwiftSourceCoordinate(t *testing.T) {
	got := MakeString("swift", "github.com/apple/swift-package-manager", "1.7.0")
	want := "pkg:swift/github.com/apple/swift-package-manager@1.7.0"
	if got != want {
		t.Errorf("MakeString() = %q, want %q", got, want)
	}
}

func TestWithVersionStringPreservesQualifiers(t *testing.T) {
	packagePURL := "pkg:generic/swift-registry/apple.example?repository_url=https:%2F%2Fold.example%2Fswift"
	got := WithVersionString(packagePURL, "1.2.3")
	want := "pkg:generic/swift-registry/apple.example@1.2.3?repository_url=https:%2F%2Fold.example%2Fswift"
	if got != want {
		t.Errorf("WithVersionString() = %q, want %q", got, want)
	}

	if got := WithVersionString("not a purl", "1.2.3"); got != "" {
		t.Errorf("WithVersionString() = %q for invalid PURL, want empty string", got)
	}
}

func TestMakeCacheStringsSwiftRegistryIdentity(t *testing.T) {
	packagePURL, versionPURL := MakeCacheStrings("swift", "APPLE/EXAMPLE", "1.2.3")

	wantPackage := "pkg:generic/swift-registry/apple.example"
	if packagePURL != wantPackage {
		t.Errorf("package PURL = %q, want %q", packagePURL, wantPackage)
	}
	wantVersion := "pkg:generic/swift-registry/apple.example@1.2.3"
	if versionPURL != wantVersion {
		t.Errorf("version PURL = %q, want %q", versionPURL, wantVersion)
	}

	dottedPackage, dottedVersion := MakeCacheStrings("swift", "apple.example", "1.2.3")
	if dottedPackage != packagePURL || dottedVersion != versionPURL {
		t.Errorf("dotted identity cache PURLs = %q, %q; want %q, %q", dottedPackage, dottedVersion, packagePURL, versionPURL)
	}
}

func TestMakeCacheStringsUsesSourcePURLWhenAvailable(t *testing.T) {
	packagePURL, versionPURL := MakeCacheStrings("swift", "github.com/apple/swift-package-manager", "1.7.0")

	if packagePURL != "pkg:swift/github.com/apple/swift-package-manager" {
		t.Errorf("package PURL = %q", packagePURL)
	}
	if versionPURL != "pkg:swift/github.com/apple/swift-package-manager@1.7.0" {
		t.Errorf("version PURL = %q", versionPURL)
	}
}

func TestMakeStringDelegatesOtherEcosystems(t *testing.T) {
	got := MakeString("npm", "@babel/core", "7.23.0")
	want := "pkg:npm/%40babel/core@7.23.0"
	if got != want {
		t.Errorf("MakeString() = %q, want %q", got, want)
	}
}
