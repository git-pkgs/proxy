package mirror

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/git-pkgs/proxy/internal/handler"
)

// fakeBottles resolves every formula version to homebrew/core/<name> tagged
// <version>-1, like a formula whose bottles were rebuilt once.
type fakeBottles struct {
	err error
}

func (f fakeBottles) BottleImage(_ context.Context, formula, version string) (handler.ImageRef, error) {
	if f.err != nil {
		return handler.ImageRef{}, f.err
	}
	return handler.ImageRef{Name: "homebrew/core/" + formula, References: []string{version + "-1"}}, nil
}

// platformImages records the images mirrored and the platforms asked for.
type platformImages struct {
	mu        sync.Mutex
	refs      []handler.ImageRef
	platforms [][]handler.Platform
}

func (p *platformImages) MirrorImage(_ context.Context, ref handler.ImageRef, platforms []handler.Platform) (handler.ImageResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.refs = append(p.refs, ref)
	p.platforms = append(p.platforms, platforms)
	return handler.ImageResult{Fetched: 2, Bytes: 10}, nil
}

type stubVersions map[string][]string

func (s stubVersions) ListVersions(_ context.Context, ecosystem, name string) ([]string, error) {
	return s[ecosystem+"/"+name], nil
}

func TestMirrorBrewMirrorsBottleImage(t *testing.T) {
	m := setupTestMirror(t, 1)
	images := &platformImages{}
	m.Images, m.Bottles = images, fakeBottles{}

	source := &PURLSource{
		PURLs:     []string{"pkg:brew/hello@2.12.3", "pkg:brew/jq"},
		Platforms: []string{"linux/arm64"},
		Versions:  stubVersions{"brew/jq": {"1.8.1"}},
	}
	progress, err := m.Run(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	if progress.Completed != 2 || progress.Failed != 0 {
		t.Fatalf("progress = %+v, errors %v", progress, progress.Errors)
	}
	got := map[string]string{}
	for i, ref := range images.refs {
		if len(images.platforms[i]) != 1 || images.platforms[i][0].OS != "linux" || images.platforms[i][0].Architecture != "arm64" {
			t.Errorf("%s: platforms = %+v, want linux/arm64", ref.Name, images.platforms[i])
		}
		got[ref.Name] = strings.Join(ref.References, ",")
	}
	if got["homebrew/core/hello"] != "2.12.3-1" || got["homebrew/core/jq"] != "1.8.1-1" {
		t.Errorf("images = %v", got)
	}
}

func TestMirrorBrewFailures(t *testing.T) {
	m := setupTestMirror(t, 1)
	progress, err := m.Run(context.Background(), &PURLSource{PURLs: []string{"pkg:brew/hello@2.12.3"}})
	if err != nil {
		t.Fatal(err)
	}
	if progress.Failed != 1 || len(progress.Errors) != 1 || !strings.Contains(progress.Errors[0].Error, "isn't available") {
		t.Errorf("without a bottle resolver: %+v", progress)
	}

	images := &platformImages{}
	m.Images, m.Bottles = images, fakeBottles{err: errors.New(`homebrew/core has no formula "nope"`)}
	progress, err = m.Run(context.Background(), &PURLSource{PURLs: []string{"pkg:brew/nope@1.0"}})
	if err != nil {
		t.Fatal(err)
	}
	if progress.Failed != 1 || len(images.refs) != 0 {
		t.Errorf("unknown formula: %+v, mirrored %v", progress, images.refs)
	}
}
