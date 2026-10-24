package mirror

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

// fakeVersions lists fixed versions and records what it was asked for.
type fakeVersions struct {
	versions []string
	err      error
	asked    []string
}

func (f *fakeVersions) ListVersions(_ context.Context, ecosystem, name string) ([]string, error) {
	f.asked = append(f.asked, ecosystem+"/"+name)
	return f.versions, f.err
}

func enumerate(t *testing.T, source Source) []PackageVersion {
	t.Helper()
	var items []PackageVersion
	if err := source.Enumerate(context.Background(), func(pv PackageVersion) error {
		items = append(items, pv)
		return nil
	}); err != nil {
		t.Fatalf("Enumerate() error = %v", err)
	}
	return items
}

func TestPURLSourceListsVersionsThroughLister(t *testing.T) {
	versions := &fakeVersions{versions: []string{"1.0.0", "1.1.0"}}
	source := &PURLSource{PURLs: []string{"pkg:npm/%40acme/widget"}, Versions: versions}

	items := enumerate(t, source)

	want := []PackageVersion{
		{Ecosystem: "npm", Name: "@acme/widget", Version: "1.0.0"},
		{Ecosystem: "npm", Name: "@acme/widget", Version: "1.1.0"},
	}
	if !reflect.DeepEqual(items, want) {
		t.Errorf("items = %v, want %v", items, want)
	}
	if !reflect.DeepEqual(versions.asked, []string{"npm/@acme/widget"}) {
		t.Errorf("lister asked for %v, want npm/@acme/widget", versions.asked)
	}
}

func TestPURLSourceReportsListerError(t *testing.T) {
	source := &PURLSource{PURLs: []string{"pkg:npm/left-pad"}, Versions: &fakeVersions{err: errors.New("upstream down")}}
	err := source.Enumerate(context.Background(), func(PackageVersion) error { return nil })
	if err == nil {
		t.Fatal("Enumerate() succeeded although listing versions failed")
	}
}

func TestSBOMSourceListsVersionsThroughLister(t *testing.T) {
	bom := map[string]any{
		"bomFormat":   "CycloneDX",
		"specVersion": "1.4",
		"components":  []map[string]any{{"type": "library", "name": "left-pad", "purl": "pkg:npm/left-pad"}},
	}
	versions := &fakeVersions{versions: []string{"1.3.0"}}
	source := &SBOMSource{Data: marshalJSON(t, bom), Versions: versions}

	items := enumerate(t, source)

	if len(items) != 1 || items[0].Version != "1.3.0" {
		t.Errorf("items = %v, want left-pad@1.3.0 from the lister", items)
	}
}

func TestSourceFromRequestUsesMirrorVersions(t *testing.T) {
	m := setupTestMirror(t, 1)
	versions := &fakeVersions{}
	m.Versions = versions
	js := NewJobStore(context.Background(), m)

	source, err := js.sourceFromRequest(JobRequest{PURLs: []string{"pkg:npm/left-pad"}})
	if err != nil {
		t.Fatalf("sourceFromRequest() error = %v", err)
	}
	if got := source.(*PURLSource).Versions; got != versions {
		t.Errorf("PURL source lists versions through %v, want the mirror's lister", got)
	}

	source, err = js.sourceFromRequest(JobRequest{SBOM: json.RawMessage(`{"bomFormat":"CycloneDX","components":[]}`)})
	if err != nil {
		t.Fatalf("sourceFromRequest() error = %v", err)
	}
	if got := source.(*SBOMSource).Versions; got != versions {
		t.Errorf("SBOM source lists versions through %v, want the mirror's lister", got)
	}
}
