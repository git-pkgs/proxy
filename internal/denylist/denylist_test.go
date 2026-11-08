package denylist

import "testing"

func TestPolicy(t *testing.T) {
	p, err := New([]string{"pkg:pypi/My_Package@1.0", "pkg:npm/@scope/name@2.0.0", "pkg:npm/%40scope/name@2.0.0"})
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"pkg:pypi/my-package@1.0", "pkg:npm/%40scope/name@2.0.0"} {
		if !p.Denied(value) {
			t.Errorf("not denied: %s", value)
		}
	}
	for _, value := range []string{"pkg:pypi/my-package@1.1", "pkg:pypi/my-package", "pkg:npm/other@2.0.0", "invalid"} {
		if p.Denied(value) {
			t.Errorf("unexpected denial: %s", value)
		}
	}
	versions := p.Versions("pkg:pypi/my-package")
	delete(versions, "1.0")
	if !p.Denied("pkg:pypi/my-package@1.0") {
		t.Fatal("caller mutated policy")
	}
	var empty *Policy
	if empty.Denied("pkg:npm/example@1") || empty.Versions("pkg:npm/example") != nil {
		t.Fatal("nil policy must allow everything")
	}
}

func TestInvalidPolicy(t *testing.T) {
	for _, value := range []string{"", "not-a-purl", "pkg:npm/foo", "pkg:npm/foo@*", "pkg:npm/foo@>=1", "pkg:npm/foo@1?arch=x86", "pkg:npm/foo@1#src", "pkg:conda/numpy@1?subdir=linux-64", "pkg:pypi/foo@1?channel=x"} {
		t.Run(value, func(t *testing.T) {
			if _, err := New([]string{value}); err == nil {
				t.Fatal("invalid denylist accepted")
			}
		})
	}
}

func TestEcosystemSpellings(t *testing.T) {
	p, err := New([]string{
		"pkg:pypi/zope.interface@6.0",
		"pkg:nuget/Newtonsoft.Json@13.0.1-Beta",
		"pkg:gem/nokogiri@1.16.0",
		"pkg:gem/sorbet-static@0.5.1-universal-darwin-22",
		"pkg:conda/numpy@1.26.4",
		"pkg:conda/scipy@1.0?channel=conda-forge",
		"pkg:conda/bioconda/samtools@1.0",
		"pkg:composer/symfony/console@6.1.7",
		"pkg:composer/monolog/monolog@v3.5.0",
		"pkg:oci/library/alpine@sha256:abc",
		"pkg:oci/upstream%2Fghcr%2Forg%2Fimage@sha256:def",
		"pkg:swift/Apple/Swift-NIO@2.0.0",
		"pkg:generic/swift-registry/apple.example@1.0.0",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{
		"pkg:pypi/zope-interface@6.0", "pkg:pypi/Zope__Interface@6.0",
		"pkg:nuget/newtonsoft.json@13.0.1-beta",
		"pkg:gem/nokogiri@1.16.0", "pkg:gem/nokogiri@1.16.0-x86_64-linux",
		"pkg:gem/sorbet-static@0.5.1-universal-darwin-22",
		"pkg:conda/numpy@1.26.4", "pkg:conda/conda-forge%2Fnumpy@1.26.4",
		"pkg:conda/conda-forge%2Fscipy@1.0", "pkg:conda/bioconda%2Fsamtools@1.0",
		"pkg:composer/symfony/console@v6.1.7", "pkg:composer/symfony/console@V6.1.7",
		"pkg:composer/monolog/monolog@3.5.0",
		"pkg:oci/library%2Falpine@sha256:abc", "pkg:oci/library/alpine@sha256:abc",
		"pkg:oci/upstream/ghcr/org/image@sha256:def",
		"pkg:generic/swift-registry/apple.swift-nio@2.0.0", "pkg:swift/apple/swift-nio@2.0.0",
		"pkg:swift/apple/example@1.0.0",
	} {
		if !p.Denied(value) {
			t.Errorf("not denied: %s", value)
		}
	}
	for _, value := range []string{
		"pkg:pypi/zopeinterface@6.0", "pkg:gem/nokogiri@1.16.1-x86_64-linux", "pkg:gem/Nokogiri@1.16.0",
		"pkg:conda/bioconda%2Fscipy@1.0", "pkg:conda/scipy@1.0", "pkg:conda/conda-forge%2Fsamtools@1.0",
		"pkg:composer/symfony/console@6.1.70", "pkg:composer/symfony/console@vv6.1.7", "pkg:npm/demo@v6.1.7",
		"pkg:oci/alpine@sha256:abc", "pkg:oci/library%2Falpine@sha256:abd",
		"pkg:generic/other/apple.swift-nio@2.0.0", "pkg:swift/github.com/apple/swift-nio@2.0.0",
	} {
		if p.Denied(value) {
			t.Errorf("unexpected denial: %s", value)
		}
	}
	if !p.Versions("pkg:pypi/zope_interface")["6.0"] {
		t.Error("pypi versions not found under another spelling")
	}
}

// TestDistroNamespaces checks that the distribution namespace of a distro
// package is ignored on both sides, as handlers build some of these PURLs
// without one and some with one.
func TestDistroNamespaces(t *testing.T) {
	p, err := New([]string{"pkg:deb/debian/curl@8.5.0-2", "pkg:rpm/fedora/curl@8.6.0-1.fc40", "pkg:apk/busybox@1.36.1-r15", "pkg:alpm/glibc@2.39-1"})
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"pkg:deb/curl@8.5.0-2", "pkg:rpm/curl@8.6.0-1.fc40", "pkg:apk/alpine/busybox@1.36.1-r15", "pkg:alpm/arch/glibc@2.39-1"} {
		if !p.Denied(value) {
			t.Errorf("not denied: %s", value)
		}
	}
	if p.Denied("pkg:deb/curl@8.5.0-3") {
		t.Error("other version denied")
	}
}

// TestDistroEpochs checks that a deb or rpm entry with an epoch matches the
// version as it appears in a filename, which leaves the epoch out.
func TestDistroEpochs(t *testing.T) {
	p, err := New([]string{"pkg:deb/foo@1:2.3-1", "pkg:rpm/bar@2:1.0-1.fc40"})
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"pkg:deb/foo@2.3-1", "pkg:deb/foo@1:2.3-1", "pkg:rpm/bar@1.0-1.fc40"} {
		if !p.Denied(value) {
			t.Errorf("not denied: %s", value)
		}
	}
	if p.Denied("pkg:deb/foo@2.3-2") {
		t.Error("other version denied")
	}
}
