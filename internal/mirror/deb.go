package mirror

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/git-pkgs/proxy/internal/handler"
	"github.com/git-pkgs/registries"
)

// debIndexTTL is how long an archive's indexes are reused, so mirroring the
// packages of an image reads each index once.
const debIndexTTL = 5 * time.Minute

var (
	// debReleasePattern takes the release from the distro qualifier syft and
	// trivy write, such as debian-12, trivy's debian-12.4 or ubuntu-22.04.
	debReleasePattern = regexp.MustCompile(`^(debian-\d+|ubuntu-\d+\.\d+)(?:\.\d+)?$`)
	// debCodenamePattern matches a codename given as the distro qualifier.
	debCodenamePattern = regexp.MustCompile(`^[a-z]+$`)
	// debArchPattern matches an architecture, such as amd64 or the ports'
	// hurd-amd64.
	debArchPattern  = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)
	debEpochPattern = regexp.MustCompile(`^\d+$`)

	// debCodenames are the codenames of the Debian and Ubuntu releases,
	// which their suites are named after. Other releases can be given by
	// codename.
	debCodenames = map[string]string{
		"debian-8": "jessie", "debian-9": "stretch", "debian-10": "buster",
		"debian-11": "bullseye", "debian-12": "bookworm", "debian-13": "trixie",
		"debian-14":    "forky",
		"ubuntu-16.04": "xenial", "ubuntu-18.04": "bionic", "ubuntu-20.04": "focal",
		"ubuntu-22.04": "jammy", "ubuntu-24.04": "noble", "ubuntu-24.10": "oracular",
		"ubuntu-25.04": "plucky", "ubuntu-25.10": "questing",
	}

	// debSuffixes name the suites of a release that are read: the release,
	// its updates and its security updates. An archive serves the ones it
	// has; Debian keeps security updates in an archive of its own.
	debSuffixes = []string{"", "-updates", "-security"}

	// debComponents are read where a suite's Release lists them.
	debComponents = []string{"main", "contrib", "non-free", "non-free-firmware", "restricted", "universe", "multiverse"}
)

// debIndexes keeps one registries client per archive, release and
// architecture, which holds the parsed indexes.
type debIndexes struct {
	mu   sync.Mutex
	regs map[string]debIndex
	// allArchs are the architectures whose index is read for Architecture:
	// all packages, by archive and release.
	allArchs map[string]debAllArch
}

type debAllArch struct {
	arch    string
	fetched time.Time
}

type debIndex struct {
	reg registries.Registry
	// suites are the suites of the release the archive serves.
	suites  []string
	fetched time.Time
}

// mirrorDeb mirrors a Debian package under the name the Debian handler
// caches it at, its path in the archive. The PURL namespace names the
// archive: an upstream.debian_repositories one by that name, or else
// upstream.debian for debian or none. The distro qualifier gives the release
// and arch the architecture; the package is looked up in the release's
// suites the archive serves.
func (m *Mirror) mirrorDeb(ctx context.Context, pv PackageVersion, tracker *progressTracker) {
	fail := func(err error) { m.recordFailure(tracker, pv, err) }
	repository, archiveURL, err := m.debArchive(pv.Namespace)
	if err != nil {
		fail(err)
		return
	}
	codename, err := debCodename(pv.Qualifiers["distro"])
	if err != nil {
		fail(err)
		return
	}
	arch := pv.Qualifiers["arch"]
	if !debArchPattern.MatchString(arch) {
		fail(fmt.Errorf("deb packages need an arch qualifier, such as arch=amd64, not %q", arch))
		return
	}
	epoch, version := debSplitEpoch(pv.Version)
	if e := pv.Qualifiers["epoch"]; e != "" {
		epoch = e
	}
	if epoch != "" && !debEpochPattern.MatchString(epoch) {
		fail(fmt.Errorf("invalid epoch %q", epoch))
		return
	}

	artifact, err := m.debArtifact(ctx, archiveURL, codename, arch, pv.Name, version, epoch)
	if err != nil {
		fail(err)
		return
	}
	path := strings.TrimPrefix(artifact.URL, archiveURL+"/")
	name, fileVersion, _ := handler.OSPackageIdentity("deb", path)
	if name == "" {
		fail(fmt.Errorf("%s isn't a package file the Debian handler serves", path))
		return
	}
	result, err := m.proxy.GetOrFetchArtifactFromURLWithDigest(ctx, pv.Ecosystem, name, fileVersion,
		handler.DebianCacheFilename(repository, path), artifact.URL, fileDigest(artifact.Integrity))
	if err != nil {
		fail(err)
		return
	}
	m.recordResult(tracker, pv, result)
}

// debArchive returns the repository name and URL of the archive namespace
// names, with an empty name for upstream.debian.
func (m *Mirror) debArchive(namespace string) (string, string, error) {
	if archiveURL, ok := m.DebianRepositories[namespace]; ok && namespace != "" {
		return namespace, strings.TrimSuffix(archiveURL, "/"), nil
	}
	if namespace == "" || namespace == "debian" {
		return "", handler.DebianUpstreamURL(m.DebianUpstream), nil
	}
	return "", "", fmt.Errorf("unknown Debian archive %q: it isn't in upstream.debian_repositories", namespace)
}

// debCodename returns the codename of the release distro names.
func debCodename(distro string) (string, error) {
	if match := debReleasePattern.FindStringSubmatch(distro); match != nil {
		if codename, ok := debCodenames[match[1]]; ok {
			return codename, nil
		}
	} else if debCodenamePattern.MatchString(distro) {
		return distro, nil
	}
	return "", fmt.Errorf("deb packages need a distro qualifier with a known release, such as distro=debian-12, or its codename, such as distro=bookworm, not %q", distro)
}

// debSplitEpoch splits the epoch off a Debian version, returning "" for
// one without.
func debSplitEpoch(version string) (epoch, rest string) {
	if e, r, ok := strings.Cut(version, ":"); ok && debEpochPattern.MatchString(e) {
		return e, r
	}
	return "", version
}

// debArtifact returns the file of name for arch in the release codename of
// the archive whose version, without its epoch, is version. epoch, when
// given, picks the epoch; without it, versions that differ only in their
// epoch are refused rather than one picked.
func (m *Mirror) debArtifact(ctx context.Context, archiveURL, codename, arch, name, version, epoch string) (registries.Artifact, error) {
	// Architecture: all packages are in every binary-<arch> index.
	indexArch := arch
	if arch == "all" {
		var err error
		if indexArch, err = m.debAllArch(ctx, archiveURL, codename); err != nil {
			return registries.Artifact{}, err
		}
	}
	reg, suites, err := m.debRegistry(ctx, archiveURL, codename, indexArch)
	if err != nil {
		return registries.Artifact{}, err
	}
	notIn := fmt.Errorf("%s %s isn't in %s of %s for %s", name, version, strings.Join(suites, ", "), archiveURL, arch)
	versions, err := reg.FetchVersions(ctx, name)
	if isNotFound(err) {
		return registries.Artifact{}, notIn
	}
	if err != nil {
		m.deb.forget(debIndexKey(archiveURL, codename, indexArch))
		return registries.Artifact{}, fmt.Errorf("reading the indexes of %s: %w", archiveURL, err)
	}
	var found registries.Artifact
	var epochs []string
	for _, v := range versions {
		e, rest := debSplitEpoch(v.Number)
		if rest != version || (epoch != "" && debEpoch(e) != debEpoch(epoch)) {
			continue
		}
		// The indexes are those of codename alone.
		for _, a := range v.Artifacts {
			if a.Metadata["arch"] == arch || a.Metadata["arch"] == "all" {
				if !slices.Contains(epochs, debEpoch(e)) {
					epochs = append(epochs, debEpoch(e))
				}
				found = a
				break
			}
		}
	}
	switch {
	case len(epochs) == 0:
		return registries.Artifact{}, notIn
	case len(epochs) > 1:
		return registries.Artifact{}, fmt.Errorf("%s %s has epochs %s in %s; give one in the version or the epoch qualifier",
			name, version, strings.Join(epochs, " and "), archiveURL)
	}
	return found, nil
}

// debEpoch is epoch with the implied 0 of a version without one.
func debEpoch(epoch string) string {
	if epoch == "" {
		return "0"
	}
	return epoch
}

// debAllArch returns the architecture whose index is read for
// Architecture: all packages of release codename: amd64 where the archive's
// Release lists it or lists none, and otherwise the first it lists, as a
// ports archive has no amd64.
func (m *Mirror) debAllArch(ctx context.Context, archiveURL, codename string) (string, error) {
	key := archiveURL + "\x00" + codename
	m.deb.mu.Lock()
	defer m.deb.mu.Unlock()
	if a, ok := m.deb.allArchs[key]; ok && time.Since(a.fetched) < debIndexTTL {
		return a.arch, nil
	}
	r, _ := m.Versions.(RegistryVersions)
	client := r.client()
	arch := "amd64"
	for _, file := range []string{"InRelease", "Release"} {
		body, err := client.GetBody(ctx, archiveURL+"/dists/"+codename+"/"+file)
		if isNotFound(err) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("reading the %s of %s: %w", file, codename, err)
		}
		arch = debReleaseAllArch(string(body))
		break
	}
	if m.deb.allArchs == nil {
		m.deb.allArchs = map[string]debAllArch{}
	}
	m.deb.allArchs[key] = debAllArch{arch: arch, fetched: time.Now()}
	return arch, nil
}

// debReleaseAllArch picks the architecture for Architecture: all packages
// from a Release's Architectures field.
func debReleaseAllArch(release string) string {
	for line := range strings.Lines(release) {
		value, ok := strings.CutPrefix(line, "Architectures:")
		if !ok {
			continue
		}
		var listed []string
		for _, a := range strings.Fields(value) {
			if a != "all" && debArchPattern.MatchString(a) {
				listed = append(listed, a)
			}
		}
		if len(listed) > 0 && !slices.Contains(listed, "amd64") {
			return listed[0]
		}
	}
	return "amd64"
}

func debIndexKey(archiveURL, codename, arch string) string {
	return archiveURL + "\x00" + codename + "\x00" + arch
}

// debRegistry returns the client for the binary-arch indexes of the suites
// of release codename that the archive serves, reusing it, and the indexes it read, for
// debIndexTTL. It also returns the suites.
func (m *Mirror) debRegistry(ctx context.Context, archiveURL, codename, arch string) (registries.Registry, []string, error) { //nolint:ireturn // registries.New returns the interface
	key := debIndexKey(archiveURL, codename, arch)
	m.deb.mu.Lock()
	defer m.deb.mu.Unlock()
	if idx, ok := m.deb.regs[key]; ok && time.Since(idx.fetched) < debIndexTTL {
		return idx.reg, idx.suites, nil
	}
	r, _ := m.Versions.(RegistryVersions)
	client := r.client()
	var suites []string
	for _, suffix := range debSuffixes {
		suite := codename + suffix
		served, err := debSuiteServed(ctx, client, archiveURL, suite)
		if err != nil {
			return nil, nil, fmt.Errorf("checking %s for suite %s: %w", archiveURL, suite, err)
		}
		if served {
			suites = append(suites, suite)
		}
	}
	if len(suites) == 0 {
		return nil, nil, fmt.Errorf("%s serves no suite of %s", archiveURL, codename)
	}
	query := url.Values{
		"suites":        {strings.Join(suites, ",")},
		"components":    {strings.Join(debComponents, ",")},
		"architectures": {arch},
	}
	reg, err := registries.New("deb", archiveURL+"?"+query.Encode(), client)
	if err != nil {
		return nil, nil, err
	}
	if m.deb.regs == nil {
		m.deb.regs = map[string]debIndex{}
	}
	// Indexes no longer asked for are dropped, rather than kept for the
	// Mirror's life.
	maps.DeleteFunc(m.deb.regs, func(_ string, idx debIndex) bool { return time.Since(idx.fetched) >= debIndexTTL })
	m.deb.regs[key] = debIndex{reg: reg, suites: suites, fetched: time.Now()}
	return reg, suites, nil
}

// debSuiteServed reports whether the archive serves suite, by its InRelease
// or Release file. An answer other than the file or 404 is an error, so a
// failing archive doesn't leave a suite out for debIndexTTL.
func debSuiteServed(ctx context.Context, client *registries.Client, archiveURL, suite string) (bool, error) {
	for _, file := range []string{"InRelease", "Release"} {
		status, err := client.Head(ctx, archiveURL+"/dists/"+suite+"/"+file)
		switch {
		case err != nil:
			return false, err
		case status == http.StatusOK:
			return true, nil
		case status != http.StatusNotFound:
			return false, fmt.Errorf("%s/dists/%s/%s: status %d", archiveURL, suite, file, status)
		}
	}
	return false, nil
}

// forget drops the client for key, so indexes that failed to load are read
// again next time.
func (d *debIndexes) forget(key string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.regs, key)
}
