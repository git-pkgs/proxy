package inventory

import (
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/git-pkgs/proxy/internal/packageurl"
)

func TestParseSyftCycloneDX(t *testing.T) {
	data, err := os.ReadFile("testdata/syft-cyclonedx.json")
	if err != nil {
		t.Fatal(err)
	}
	components, skipped, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := []Component{
		{Ecosystem: "npm", Name: "@babel/core", Version: "7.23.0", PackagePURL: "pkg:npm/%40babel/core"},
		{Ecosystem: "alpine", Name: "busybox", Version: "1.36.1-r15", PackagePURL: "pkg:apk/alpine/busybox"},
		{Ecosystem: "golang", Name: "github.com/inconshreveable/mousetrap", Version: "v1.1.0", PackagePURL: "pkg:golang/github.com/inconshreveable/mousetrap"},
		{Ecosystem: "golang", Name: "github.com/spf13/cobra", Version: "v1.8.0", PackagePURL: "pkg:golang/github.com/spf13/cobra"},
		{Ecosystem: "golang", Name: "github.com/spf13/pflag", Version: "v1.0.5", PackagePURL: "pkg:golang/github.com/spf13/pflag"},
		{Ecosystem: "npm", Name: "lodash", Version: "4.17.20", PackagePURL: "pkg:npm/lodash"},
		{Ecosystem: "composer", Name: "monolog/monolog", Version: "3.5.0", PackagePURL: "pkg:composer/monolog/monolog"},
		{Ecosystem: "alpine", Name: "musl", Version: "1.2.4-r2", PackagePURL: "pkg:apk/alpine/musl"},
		{Ecosystem: "composer", Name: "symfony/console", Version: "v6.4.1", PackagePURL: "pkg:composer/symfony/console"},
	}
	if !slices.Equal(components, want) {
		t.Errorf("components =\n%v\nwant\n%v", components, want)
	}
	// The operating system and the four lock files syft lists carry no PURL,
	// and webapp is the root of package-lock.json, the project itself.
	if skipped != 6 {
		t.Errorf("skipped = %d, want 6", skipped)
	}
}

func TestParseDeduplicatesAndSkipsUnknownTypes(t *testing.T) {
	doc := `{"bomFormat":"CycloneDX","specVersion":"1.5","components":[
		{"type":"library","name":"lodash","version":"4.17.21","purl":"pkg:npm/lodash@4.17.21"},
		{"type":"library","name":"lodash","version":"4.17.21","purl":"pkg:npm/lodash@4.17.21"},
		{"type":"library","name":"lodash","version":"4.17.20","purl":"pkg:npm/lodash@4.17.20"},
		{"type":"library","name":"checkout","version":"v4","purl":"pkg:github/actions/checkout@v4"},
		{"type":"library","name":"Django","version":"4.2.0","purl":"pkg:pypi/Django"},
		{"type":"library","name":"guava","version":"32.1.3-jre","purl":"pkg:maven/com.google.guava/guava@32.1.3-jre?type=jar"},
		{"type":"library","name":"libc6","version":"2.36-9","purl":"pkg:deb/debian/libc6@2.36-9?arch=amd64&distro=debian-12"},
		{"type":"library","name":"broken","purl":"not a purl"}
	]}`
	components, skipped, err := Parse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	want := []Component{
		{Ecosystem: "npm", Name: "lodash", Version: "4.17.21", PackagePURL: "pkg:npm/lodash"},
		{Ecosystem: "npm", Name: "lodash", Version: "4.17.20", PackagePURL: "pkg:npm/lodash"},
		{Ecosystem: "pypi", Name: "django", Version: "4.2.0", PackagePURL: "pkg:pypi/django"},
		{Ecosystem: "maven", Name: "com.google.guava:guava", Version: "32.1.3-jre", PackagePURL: "pkg:maven/com.google.guava/guava"},
		{Ecosystem: "deb", Name: "libc6", Version: "2.36-9", PackagePURL: "pkg:deb/libc6"},
	}
	if !slices.Equal(components, want) {
		t.Errorf("components =\n%v\nwant\n%v", components, want)
	}
	if skipped != 2 {
		t.Errorf("skipped = %d, want 2", skipped)
	}
}

func TestParseRejectsNonSBOM(t *testing.T) {
	if _, _, err := Parse([]byte(`{"hello":"world"}`)); err == nil {
		t.Error("Parse accepted a document that is not an SBOM")
	}
}

func TestFromPURLUnsupported(t *testing.T) {
	_, err := FromPURL("pkg:github/actions/checkout")
	if !errors.Is(err, ErrUnsupportedPURL) {
		t.Errorf("err = %v, want ErrUnsupportedPURL", err)
	}
}

// TestFromPURLMatchesHandlerNames checks, for every PURL type the inventory
// maps, that an SBOM's package URL ends up as the package URL the handler
// records when it caches that package. handlerName is the name the handler
// passes to GetOrFetchArtifactFromURL, taken from the handler's code.
func TestFromPURLMatchesHandlerNames(t *testing.T) {
	tests := []struct {
		purlType    string
		sbomPURL    string
		handlerName string
	}{
		{"alpm", "pkg:alpm/arch/glibc@2.39-1?arch=x86_64", "glibc"},
		{"apk", "pkg:apk/alpine/busybox@1.36.1-r15?arch=x86_64", "busybox"},
		{"bsd", "pkg:bsd/freebsd/curl@8.7.1", "curl"},
		{"cargo", "pkg:cargo/serde@1.0.200", "serde"},
		{"chef", "pkg:chef/nginx@12.0.0", "nginx"},
		{"composer", "pkg:composer/symfony/console@v6.4.1", "symfony/console"},
		{"conan", "pkg:conan/zlib@1.3.1", "zlib"},
		{"conda", "pkg:conda/numpy@1.26.4?channel=conda-forge&subdir=linux-64", "conda-forge/numpy"},
		{"cran", "pkg:cran/ggplot2@3.5.1", "ggplot2"},
		{"deb", "pkg:deb/debian/libc6@2.36-9?arch=amd64&distro=debian-12", "libc6"},
		{"gem", "pkg:gem/rails@7.1.3", "rails"},
		// The go handler stores the module path with its case; PURLs of
		// golang modules are lowercased on both sides.
		{"golang", "pkg:golang/github.com/Azure/azure-sdk-for-go@v68.0.0", "github.com/Azure/azure-sdk-for-go"},
		{"hex", "pkg:hex/phoenix@1.7.12", "phoenix"},
		{"julia", "pkg:julia/Example@0.5.3?uuid=7876af07-990d-54b4-ab0e-23690620f79a", "Example"},
		{"maven", "pkg:maven/org.slf4j/slf4j-api@2.0.13?type=jar", "org.slf4j:slf4j-api"},
		{"npm", "pkg:npm/%40babel/core@7.23.0", "@babel/core"},
		// The NuGet handler lowercases the package ID.
		{"nuget", "pkg:nuget/Newtonsoft.Json@13.0.3", "newtonsoft.json"},
		{"pub", "pkg:pub/http@1.2.1", "http"},
		// The PyPI handler takes the name from the wheel or sdist file name,
		// which spells "." and "-" as "_".
		{"pypi", "pkg:pypi/zope.interface@6.4", "zope_interface"},
		{"rpm", "pkg:rpm/fedora/curl@8.6.0-7.fc40?arch=x86_64", "curl"},
		{"swift", "pkg:swift/github.com/apple/swift-argument-parser@1.3.1", "github.com/apple/swift-argument-parser"},
	}
	covered := map[string]bool{}
	for _, tt := range tests {
		t.Run(tt.purlType, func(t *testing.T) {
			covered[tt.purlType] = true
			c, err := FromPURL(tt.sbomPURL)
			if err != nil {
				t.Fatalf("FromPURL(%q): %v", tt.sbomPURL, err)
			}
			want, _ := packageurl.MakeCacheStrings(ecosystems[tt.purlType], tt.handlerName, "")
			if want == "" {
				t.Fatalf("handler name %q has no package URL", tt.handlerName)
			}
			if c.PackagePURL != want {
				t.Errorf("PackagePURL = %q, want %q as the handler records it", c.PackagePURL, want)
			}
		})
	}
	for purlType := range ecosystems {
		if !covered[purlType] {
			t.Errorf("no case for PURL type %q", purlType)
		}
	}
}

func TestFromPURLNormalizesPyPINames(t *testing.T) {
	for _, s := range []string{"pkg:pypi/zope.interface", "pkg:pypi/Zope_Interface", "pkg:pypi/zope--interface", "pkg:pypi/zope._-interface"} {
		c, err := FromPURL(s)
		if err != nil {
			t.Fatal(err)
		}
		if c.Name != "zope-interface" || c.PackagePURL != "pkg:pypi/zope-interface" {
			t.Errorf("FromPURL(%q) = %+v, want zope-interface", s, c)
		}
	}
}

func TestFromPURLCondaNeedsChannel(t *testing.T) {
	if _, err := FromPURL("pkg:conda/numpy@1.26.4"); !errors.Is(err, ErrNoCondaChannel) {
		t.Errorf("err = %v, want ErrNoCondaChannel", err)
	}
	doc := `{"bomFormat":"CycloneDX","specVersion":"1.5","components":[
		{"type":"library","name":"numpy","version":"1.26.4","purl":"pkg:conda/numpy@1.26.4"},
		{"type":"library","name":"numpy","version":"1.26.4","purl":"pkg:conda/numpy@1.26.4?channel=conda-forge"}
	]}`
	components, skipped, err := Parse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	if len(components) != 1 || components[0].Name != "conda-forge/numpy" || skipped != 1 {
		t.Errorf("components = %+v, skipped = %d; want conda-forge/numpy and 1 skipped", components, skipped)
	}
}

func TestValidateProjectName(t *testing.T) {
	valid := []string{"repo", "group/repo", "group/sub/my_repo-2.0", strings.Repeat("a", MaxProjectNameLength)}
	for _, name := range valid {
		if err := ValidateProjectName(name); err != nil {
			t.Errorf("ValidateProjectName(%q) = %v, want nil", name, err)
		}
	}
	invalid := []string{
		"", "/repo", "repo/", "group//repo", ".", "..", "group/../repo", "group/./repo",
		"group/re po", "group/repo?x", "group\\repo", "grüppe/repo", "group/repo%2F",
		strings.Repeat("a", MaxProjectNameLength+1),
	}
	for _, name := range invalid {
		if err := ValidateProjectName(name); err == nil {
			t.Errorf("ValidateProjectName(%q) = nil, want an error", name)
		}
	}
}

func TestOutdated(t *testing.T) {
	tests := []struct {
		name      string
		ecosystem string
		version   string
		other     string
		outdated  bool
	}{
		{"npm older", "npm", "4.17.20", "4.17.21", true},
		{"npm same", "npm", "4.17.21", "4.17.21", false},
		{"npm newer", "npm", "4.18.0", "4.17.21", false},
		{"composer v prefix", "composer", "v6.4.1", "v6.4.2", true},
		{"golang", "golang", "v1.8.0", "v1.10.0", true},
		{"alpine revisions", "alpine", "1.36.1-r15", "1.36.1-r2", false},
		{"empty version", "npm", "", "1.0.0", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Outdated(tt.ecosystem, tt.version, tt.other); got != tt.outdated {
				t.Errorf("Outdated = %v, want %v", got, tt.outdated)
			}
		})
	}
}

func TestParseSkipsTheProjectsOwnPackages(t *testing.T) {
	doc := `{"bomFormat":"CycloneDX","specVersion":"1.6","components":[
		{"bom-ref":"mod","type":"library","name":"github.com/acme/app","version":"UNKNOWN","purl":"pkg:golang/github.com/acme/app"},
		{"bom-ref":"bin","type":"library","name":"github.com/acme/app","version":"v0.0.0-20261027162038-a3e9dd9fd3f4+dirty","purl":"pkg:golang/github.com/acme/app@v0.0.0-20261027162038-a3e9dd9fd3f4%2Bdirty",
		 "properties":[{"name":"syft:metadata:mainModule","value":"github.com/acme/app"}]},
		{"bom-ref":"cobra","type":"library","name":"github.com/spf13/cobra","version":"v1.8.0","purl":"pkg:golang/github.com/spf13/cobra@v1.8.0",
		 "properties":[{"name":"syft:metadata:mainModule","value":"github.com/acme/app"}]},
		{"bom-ref":"root","type":"library","name":"app-frontend","version":"2.1.0","purl":"pkg:npm/app-frontend@2.1.0",
		 "properties":[{"name":"syft:package:metadataType","value":"javascript-npm-package-lock-entry"}]},
		{"bom-ref":"nover","type":"library","name":"app-tools","purl":"pkg:npm/app-tools"},
		{"bom-ref":"jsdom","type":"library","name":"jsdom","version":"30.1.3","purl":"pkg:npm/jsdom@30.1.3",
		 "properties":[{"name":"syft:package:metadataType","value":"javascript-npm-package-lock-entry"}]}
	],"dependencies":[{"ref":"root","dependsOn":["jsdom"]}]}`
	components, skipped, err := Parse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	want := []Component{
		{Ecosystem: "golang", Name: "github.com/spf13/cobra", Version: "v1.8.0", PackagePURL: "pkg:golang/github.com/spf13/cobra"},
		{Ecosystem: "npm", Name: "jsdom", Version: "30.1.3", PackagePURL: "pkg:npm/jsdom"},
	}
	if !slices.Equal(components, want) {
		t.Errorf("components =\n%v\nwant\n%v", components, want)
	}
	if skipped != 4 {
		t.Errorf("skipped = %d, want 4", skipped)
	}
}

func TestParseSkipsMainModuleRegardlessOfCase(t *testing.T) {
	// syft keeps the module path's case in mainModule; package URLs of Go
	// modules are lowercased.
	doc := `{"bomFormat":"CycloneDX","specVersion":"1.6","components":[
		{"bom-ref":"bin","type":"library","name":"github.com/Azure/foo","version":"v1.2.0","purl":"pkg:golang/github.com/Azure/foo@v1.2.0",
		 "properties":[{"name":"syft:metadata:mainModule","value":"github.com/Azure/foo"}]},
		{"bom-ref":"cobra","type":"library","name":"github.com/spf13/cobra","version":"v1.8.0","purl":"pkg:golang/github.com/spf13/cobra@v1.8.0",
		 "properties":[{"name":"syft:metadata:mainModule","value":"github.com/Azure/foo"}]}
	]}`
	components, skipped, err := Parse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	if len(components) != 1 || components[0].Name != "github.com/spf13/cobra" || skipped != 1 {
		t.Errorf("components = %+v, skipped = %d; want only cobra, 1 skipped", components, skipped)
	}
}
