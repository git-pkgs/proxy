package mirror

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/git-pkgs/proxy/internal/handler"
	"github.com/git-pkgs/registries"
)

// rpmIndexTTL is how long a repository's primary metadata is reused, so
// mirroring the packages of an image reads it once.
const rpmIndexTTL = 5 * time.Minute

var (
	// rpmDistroPattern takes the release from the distro qualifier syft
	// writes, such as fedora-40 or rocky-9.4.
	rpmDistroPattern = regexp.MustCompile(`^[a-z][a-z0-9]*-(\d+(?:\.\d+)*)$`)
	rpmArchPattern   = regexp.MustCompile(`^[a-z0-9_]+$`)
	rpmEpochPattern  = regexp.MustCompile(`^\d+$`)

	// rpmDefaultRepositoryPaths are Fedora's repositories of a release on
	// dl.fedoraproject.org, the default upstream.rpm: its updates, then the
	// release.
	rpmDefaultRepositoryPaths = []string{
		"updates/{release}/Everything/{arch}",
		"releases/{release}/Everything/{arch}/os",
	}
)

// rpmIndexes keeps one registries client per repository, which holds the
// parsed primary metadata.
type rpmIndexes struct {
	mu   sync.Mutex
	regs map[string]rpmIndex
}

type rpmIndex struct {
	// reg is nil for a repository upstream answered 404 for.
	reg     registries.Registry
	fetched time.Time
}

// mirrorRPM mirrors an RPM package under the name the RPM handler caches it
// at, its path below upstream.rpm. The distro qualifier gives the release
// and arch the architecture, and the package is looked up in the
// repositories upstream.rpm_repository_paths names for them, Fedora's by
// default.
func (m *Mirror) mirrorRPM(ctx context.Context, pv PackageVersion, tracker *progressTracker) {
	fail := func(err error) { m.recordFailure(tracker, pv, err) }
	match := rpmDistroPattern.FindStringSubmatch(pv.Qualifiers["distro"])
	if match == nil {
		fail(fmt.Errorf("rpm packages need a distro qualifier with the release, such as distro=fedora-40, not %q", pv.Qualifiers["distro"]))
		return
	}
	release := match[1]
	arch := pv.Qualifiers["arch"]
	if !rpmArchPattern.MatchString(arch) {
		fail(fmt.Errorf("rpm packages need an arch qualifier, such as arch=x86_64, not %q", arch))
		return
	}
	epoch := pv.Qualifiers["epoch"]
	if epoch != "" && !rpmEpochPattern.MatchString(epoch) {
		fail(fmt.Errorf("invalid epoch %q", epoch))
		return
	}
	// noarch packages are in every architecture's repository.
	repoArch := arch
	if arch == "noarch" {
		repoArch = "x86_64"
	}
	major, _, _ := strings.Cut(release, ".")
	replacer := strings.NewReplacer("{release}", release, "{major}", major, "{arch}", repoArch)
	paths := m.RPMRepositoryPaths
	if len(paths) == 0 {
		paths = rpmDefaultRepositoryPaths
	}
	upstreamURL := handler.RPMUpstreamURL(m.RPMUpstream)

	dirs := make([]string, len(paths))
	for i, p := range paths {
		dirs[i] = path.Clean(replacer.Replace(p))
	}
	artifact, err := m.rpmArtifact(ctx, upstreamURL, dirs, pv.Qualifiers["distro"], arch, pv.Name, pv.Version, epoch)
	if err != nil {
		fail(err)
		return
	}
	filePath := path.Clean(strings.TrimPrefix(artifact.URL, upstreamURL+"/"))
	name, version, _ := handler.OSPackageIdentity("rpm", filePath)
	if name == "" {
		fail(fmt.Errorf("%s isn't a package file the RPM handler serves", filePath))
		return
	}
	result, err := m.proxy.GetOrFetchArtifactFromURLWithDigest(ctx, pv.Ecosystem, name, version, filePath, artifact.URL, fileDigest(artifact.Integrity))
	if err != nil {
		fail(err)
		return
	}
	m.recordResult(tracker, pv, result)
}

// rpmArtifact returns the file of name for arch whose version-release is
// version, from the first of the repositories dirs below upstreamURL that
// lists it. epoch, when given, picks the epoch;
// without it, builds that differ only in their epoch are refused rather
// than one picked.
func (m *Mirror) rpmArtifact(ctx context.Context, upstreamURL string, dirs []string, distro, arch, name, version, epoch string) (registries.Artifact, error) {
	for _, dir := range dirs {
		repoURL := upstreamURL + "/" + dir
		reg, err := m.rpmRegistry(repoURL, distro)
		if err != nil {
			return registries.Artifact{}, err
		}
		if reg == nil {
			continue
		}
		versions, err := reg.FetchVersions(ctx, name)
		var httpErr *registries.HTTPError
		if errors.As(err, &httpErr) && httpErr.IsNotFound() {
			// The repository isn't there, which is kept for rpmIndexTTL
			// rather than asked again for each package.
			m.rpm.missing(repoURL)
			continue
		}
		if isNotFound(err) {
			continue
		}
		if err != nil {
			m.rpm.forget(repoURL)
			return registries.Artifact{}, fmt.Errorf("reading %s/repodata: %w", repoURL, err)
		}
		found, epochs := rpmMatch(versions, arch, version, epoch)
		switch {
		case len(epochs) > 1:
			return registries.Artifact{}, fmt.Errorf("%s %s has epochs %s in %s; give one in the epoch qualifier",
				name, version, strings.Join(epochs, " and "), repoURL)
		case len(epochs) == 1:
			return found, nil
		}
	}
	return registries.Artifact{}, fmt.Errorf("%s %s isn't in %s below %s for %s", name, version, strings.Join(dirs, " or "), upstreamURL, arch)
}

// rpmMatch returns a file of versions for arch whose version-release is
// version, and the epochs of those files, of epoch alone when it is given.
func rpmMatch(versions []registries.Version, arch, version, epoch string) (registries.Artifact, []string) {
	var found registries.Artifact
	var epochs []string
	for _, v := range versions {
		if v.Number != version {
			continue
		}
		for _, a := range v.Artifacts {
			e := rpmEpoch(a)
			if (a.Metadata["arch"] == arch || a.Metadata["arch"] == "noarch") && (epoch == "" || e == epoch) {
				if !slices.Contains(epochs, e) {
					epochs = append(epochs, e)
				}
				found = a
			}
		}
	}
	return found, epochs
}

// rpmEpoch returns the epoch of a, 0 when it records none.
func rpmEpoch(a registries.Artifact) string {
	if e, _ := a.Metadata["epoch"].(string); e != "" {
		return e
	}
	return "0"
}

// rpmRegistry returns the client for the repository at repoURL, reusing it,
// and the metadata it read, for rpmIndexTTL. It returns nil for a
// repository recorded as missing.
func (m *Mirror) rpmRegistry(repoURL, distro string) (registries.Registry, error) { //nolint:ireturn // registries.New returns the interface
	m.rpm.mu.Lock()
	defer m.rpm.mu.Unlock()
	if idx, ok := m.rpm.regs[repoURL]; ok && time.Since(idx.fetched) < rpmIndexTTL {
		return idx.reg, nil
	}
	r, _ := m.Versions.(RegistryVersions)
	reg, err := registries.New("rpm", repoURL+"?"+url.Values{"distro": {distro}}.Encode(), r.client())
	if err != nil {
		return nil, err
	}
	m.rpm.store(repoURL, reg)
	return reg, nil
}

// store keeps reg for repoURL, dropping the clients past rpmIndexTTL, so
// indexes no longer asked for aren't kept for the Mirror's life. The
// caller holds r.mu.
func (r *rpmIndexes) store(repoURL string, reg registries.Registry) {
	if r.regs == nil {
		r.regs = map[string]rpmIndex{}
	}
	maps.DeleteFunc(r.regs, func(_ string, idx rpmIndex) bool { return time.Since(idx.fetched) >= rpmIndexTTL })
	r.regs[repoURL] = rpmIndex{reg: reg, fetched: time.Now()}
}

// missing records that the repository at repoURL isn't there.
func (r *rpmIndexes) missing(repoURL string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.store(repoURL, nil)
}

// forget drops the client for repoURL, so metadata that failed to load is
// read again next time.
func (r *rpmIndexes) forget(repoURL string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.regs, repoURL)
}
