package mirror

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/git-pkgs/proxy/internal/handler"
	"github.com/git-pkgs/registries"
)

// apkIndexTTL is how long a repository's APKINDEX is reused, so mirroring
// the packages of an image reads each index once.
const apkIndexTTL = 5 * time.Minute

var (
	// apkDistroPattern takes the release from syft's distro qualifier, such
	// as alpine-3.20.3 for v3.20. Edge, as alpine-3.21.0_alpha20240807,
	// isn't a release and doesn't match.
	apkDistroPattern = regexp.MustCompile(`^alpine-(\d+)\.(\d+)(?:\.\d+)?$`)
	apkArchPattern   = regexp.MustCompile(`^[a-z0-9_]+$`)
	// apkComponents are the repositories of a release a package is looked
	// up in, in order.
	apkComponents = []string{"main", "community"}
)

// apkIndexes keeps one registries client per repository directory, which
// holds the parsed APKINDEX.
type apkIndexes struct {
	mu   sync.Mutex
	regs map[string]apkIndex
}

type apkIndex struct {
	reg     registries.Registry
	fetched time.Time
}

// mirrorAPK mirrors an Alpine package under the path the handler caches it
// at, {repository}/v{release}/{component}/{arch}/{file}. The PURL namespace
// names the upstream.apk repository; the distro and arch qualifiers give
// the release and architecture, and the package is looked up in the main
// and community indexes of that release.
func (m *Mirror) mirrorAPK(ctx context.Context, pv PackageVersion, tracker *progressTracker) {
	fail := func(err error) { m.recordFailure(tracker, pv, err) }
	repository, name := pv.Namespace, pv.Name
	if repository == "" {
		repository = "alpine"
	}
	repositoryURL, ok := handler.APKRepositories(m.APKRepositories)[repository]
	if !ok {
		fail(fmt.Errorf("unknown APK repository %q: it isn't in upstream.apk", repository))
		return
	}
	match := apkDistroPattern.FindStringSubmatch(pv.Qualifiers["distro"])
	if match == nil {
		fail(fmt.Errorf("apk packages need a distro qualifier with the Alpine release, such as distro=alpine-3.20.3, not %q", pv.Qualifiers["distro"]))
		return
	}
	release := "v" + match[1] + "." + match[2]
	arch := pv.Qualifiers["arch"]
	if !apkArchPattern.MatchString(arch) {
		fail(fmt.Errorf("apk packages need an arch qualifier, such as arch=x86_64, not %q", arch))
		return
	}

	for _, component := range apkComponents {
		dir := release + "/" + component + "/" + arch
		artifact, found, err := m.apkArtifact(ctx, repositoryURL+"/"+dir, name, pv.Version)
		if err != nil {
			fail(fmt.Errorf("reading %s %s/APKINDEX.tar.gz: %w", repository, dir, err))
			return
		}
		if !found {
			continue
		}
		cacheFilename := handler.APKCacheFilename(repository, dir+"/"+artifact.Filename)
		result, err := m.proxy.GetOrFetchArtifactFromURL(ctx, pv.Ecosystem, name, pv.Version, cacheFilename, artifact.URL)
		if err != nil {
			fail(err)
			return
		}
		m.recordResult(tracker, pv, result)
		return
	}
	fail(fmt.Errorf("%s %s isn't in %s of %s %s for %s", name, pv.Version, strings.Join(apkComponents, " or "), repository, release, arch))
}

// apkArtifact returns the file of version of name that the index in dirURL
// lists, and whether it lists one.
func (m *Mirror) apkArtifact(ctx context.Context, dirURL, name, version string) (registries.Artifact, bool, error) {
	reg, err := m.apkRegistry(dirURL)
	if err != nil {
		return registries.Artifact{}, false, err
	}
	versions, err := reg.FetchVersions(ctx, name)
	if isNotFound(err) {
		return registries.Artifact{}, false, nil
	}
	if err != nil {
		m.apk.forget(dirURL)
		return registries.Artifact{}, false, err
	}
	for _, v := range versions {
		if v.Number == version && len(v.Artifacts) > 0 {
			return v.Artifacts[0], true, nil
		}
	}
	return registries.Artifact{}, false, nil
}

// apkRegistry returns the client for the repository directory at dirURL,
// reusing it, and the index it read, for apkIndexTTL.
func (m *Mirror) apkRegistry(dirURL string) (registries.Registry, error) { //nolint:ireturn // registries.New returns the interface
	m.apk.mu.Lock()
	defer m.apk.mu.Unlock()
	if idx, ok := m.apk.regs[dirURL]; ok && time.Since(idx.fetched) < apkIndexTTL {
		return idx.reg, nil
	}
	r, _ := m.Versions.(RegistryVersions)
	// Naming the v2 index reads it also where Packages.adb is served beside
	// it, which the client would try first and refuse.
	reg, err := registries.New("apk", dirURL+"/APKINDEX.tar.gz", r.client())
	if err != nil {
		return nil, err
	}
	if m.apk.regs == nil {
		m.apk.regs = map[string]apkIndex{}
	}
	m.apk.regs[dirURL] = apkIndex{reg: reg, fetched: time.Now()}
	return reg, nil
}

// forget drops the client for dirURL, so an index that failed to load is
// read again next time.
func (a *apkIndexes) forget(dirURL string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.regs, dirURL)
}

// isNotFound reports whether err means a package or index doesn't exist,
// such as a release without a community repository for an architecture.
func isNotFound(err error) bool {
	if errors.Is(err, registries.ErrNotFound) {
		return true
	}
	var httpErr *registries.HTTPError
	return errors.As(err, &httpErr) && httpErr.IsNotFound()
}
