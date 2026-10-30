package mirror

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/git-pkgs/registries"
)

const (
	cranDefaultURL = "https://cloud.r-project.org"
	// cranIndexTTL is how long a fetched src/contrib/PACKAGES is reused, so
	// mirroring many packages, or every version of one, reads it once.
	cranIndexTTL = 5 * time.Minute
	// cranMaxIndexSize limits the decompressed PACKAGES.
	// CRAN's PACKAGES is about 7 MB.
	cranMaxIndexSize = 64 << 20
	// cranMaxLine limits a line of PACKAGES.
	cranMaxLine = 1 << 20
)

var (
	cranNamePattern    = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9.]*$`)
	cranVersionPattern = regexp.MustCompile(`^[0-9]+[0-9.-]*$`)
)

// cranIndex caches the MD5 sums src/contrib/PACKAGES lists for the current
// source packages of a CRAN repository, by file name.
type cranIndex struct {
	mu      sync.Mutex
	baseURL string
	fetched time.Time
	md5s    map[string]string
}

// mirrorCRAN mirrors a CRAN source package under ("cran", name, version,
// name_version.tar.gz), the one entry the handler serves for both
// src/contrib and src/contrib/Archive.
func (m *Mirror) mirrorCRAN(ctx context.Context, pv PackageVersion, tracker *progressTracker) {
	fail := func(err error) { m.recordFailure(tracker, pv, err) }
	if !cranNamePattern.MatchString(pv.Name) {
		fail(fmt.Errorf("invalid CRAN package name %q", pv.Name))
		return
	}
	if !cranVersionPattern.MatchString(pv.Version) {
		fail(fmt.Errorf("invalid CRAN version %q", pv.Version))
		return
	}
	baseURL, client, err := m.cranRepository(pv.Name)
	if err != nil {
		fail(err)
		return
	}
	filename := pv.Name + "_" + pv.Version + ".tar.gz"

	current, err := m.cranCurrentMD5s(ctx, baseURL, client)
	if err != nil {
		fail(fmt.Errorf("reading src/contrib/PACKAGES: %w", err))
		return
	}
	downloadURL := baseURL + "/src/contrib/" + filename
	sum, ok := current[filename]
	if !ok {
		downloadURL = baseURL + "/src/contrib/Archive/" + url.PathEscape(pv.Name) + "/" + filename
	}
	var declared string
	if sum != "" {
		declared = "md5-" + sum
	}
	result, err := m.proxy.GetOrFetchArtifactFromURLWithIntegrity(ctx, pv.Ecosystem, pv.Name, pv.Version, filename, downloadURL, declared)
	if err != nil {
		fail(err)
		return
	}
	m.recordResult(tracker, pv, result)
}

// cranRepository returns the repository to mirror a CRAN package from and
// the client to read its indexes with: those Versions lists versions with,
// which come from upstream.cran, or the public CRAN.
func (m *Mirror) cranRepository(name string) (string, *registries.Client, error) {
	r, _ := m.Versions.(RegistryVersions)
	baseURL := cranDefaultURL
	if r.URL != nil {
		configured, err := r.URL("cran", name)
		if err != nil {
			return "", nil, err
		}
		if configured != "" {
			baseURL = strings.TrimSuffix(configured, "/")
		}
	}
	client := r.Client
	if client == nil {
		client = registries.DefaultClient()
	}
	return baseURL, client, nil
}

// cranCurrentMD5s returns the MD5 sums of the current source packages of the
// repository at baseURL, reading src/contrib/PACKAGES.gz unless it was read
// within cranIndexTTL.
func (m *Mirror) cranCurrentMD5s(ctx context.Context, baseURL string, client *registries.Client) (map[string]string, error) {
	m.cran.mu.Lock()
	defer m.cran.mu.Unlock()
	if m.cran.md5s != nil && m.cran.baseURL == baseURL && time.Since(m.cran.fetched) < cranIndexTTL {
		return m.cran.md5s, nil
	}
	body, err := client.GetBody(ctx, baseURL+"/src/contrib/PACKAGES.gz")
	if err != nil {
		return nil, err
	}
	data, err := gunzipLimited(body)
	if err != nil {
		return nil, err
	}
	sums, err := parseCRANPackages(data)
	if err != nil {
		return nil, err
	}
	m.cran.md5s = sums
	m.cran.baseURL = baseURL
	m.cran.fetched = time.Now()
	return m.cran.md5s, nil
}

// gunzipLimited decompresses data, or returns it as is when it isn't
// gzipped.
func gunzipLimited(data []byte) ([]byte, error) {
	if !bytes.HasPrefix(data, []byte{0x1f, 0x8b}) {
		return data, nil
	}
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer func() { _ = zr.Close() }()
	out, err := io.ReadAll(io.LimitReader(zr, cranMaxIndexSize+1))
	if err != nil {
		return nil, err
	}
	if len(out) > cranMaxIndexSize {
		return nil, fmt.Errorf("index is larger than %d bytes", cranMaxIndexSize)
	}
	return out, nil
}

// parseCRANPackages returns the MD5sum of each source package a PACKAGES
// file lists, by file name. Entries without one are listed with "".
func parseCRANPackages(data []byte) (map[string]string, error) {
	sums := map[string]string{}
	var name, version, sum string
	flush := func() {
		if name != "" && version != "" {
			sums[name+"_"+version+".tar.gz"] = sum
		}
		name, version, sum = "", "", ""
	}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(nil, cranMaxLine)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "" {
			flush()
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok || strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
			continue
		}
		switch value = strings.TrimSpace(value); key {
		case "Package":
			name = value
		case "Version":
			version = value
		case "MD5sum":
			sum = value
		}
	}
	flush()
	return sums, scanner.Err()
}
