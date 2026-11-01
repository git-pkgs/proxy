package mirror

import (
	"archive/tar"
	"bufio"
	"compress/bzip2"
	"compress/gzip"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"
)

// indexFile is a package file a repository index lists: its path below the
// repository base and, where the index gives one, its SHA-256 digest.
type indexFile struct {
	Path   string
	Digest string
}

// maxIndexLine bounds one line of a Debian or Alpine index.
const maxIndexLine = 1 << 20

// parseAPKIndex reads the files an APKINDEX.tar.gz lists. Its checksums
// cover only the control section of a package, so no digest is returned.
func parseAPKIndex(r io.Reader) ([]indexFile, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, err
	}
	defer func() { _ = gz.Close() }()
	// An APKINDEX.tar.gz may be several gzip streams: the signature, then the
	// index. Reading on across them finds the index whichever comes first.
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, errors.New("APKINDEX.tar.gz has no APKINDEX")
		}
		if err != nil {
			return nil, err
		}
		if hdr.Name == "APKINDEX" {
			return parseAPKIndexText(tr)
		}
	}
}

func parseAPKIndexText(r io.Reader) ([]indexFile, error) {
	var files []indexFile
	var name, version string
	flush := func() {
		if name != "" && version != "" {
			files = append(files, indexFile{Path: name + "-" + version + ".apk"})
		}
		name, version = "", ""
	}
	scanner := bufio.NewScanner(r)
	scanner.Buffer(nil, maxIndexLine)
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case line == "":
			flush()
		case strings.HasPrefix(line, "P:"):
			name = line[2:]
		case strings.HasPrefix(line, "V:"):
			version = line[2:]
		}
	}
	flush()
	return files, scanner.Err()
}

// debIndexPreference ranks the compressions of a Packages index, best first.
var debIndexPreference = []string{".xz", ".gz", ".bz2", ""}

// debPackagesIndexes returns, per component/binary-arch directory, the path
// of the Packages index a Release or InRelease file lists, in the best
// compression it offers.
func debPackagesIndexes(release []byte) map[string]string {
	listed := map[string]bool{}
	inSHA256 := false
	for _, line := range strings.Split(string(release), "\n") {
		line = strings.TrimRight(line, "\r")
		if !strings.HasPrefix(line, " ") {
			inSHA256 = line == "SHA256:"
			continue
		}
		if !inSHA256 {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 3 { //nolint:mnd // checksum, size, path
			listed[fields[2]] = true
		}
	}
	indexes := map[string]string{}
	for path := range listed {
		dir, file, ok := strings.Cut(path, "/Packages")
		if !ok || !strings.Contains(dir, "/binary-") || strings.Contains(file, "/") {
			continue
		}
		for _, ext := range debIndexPreference {
			if listed[dir+"/Packages"+ext] {
				indexes[dir] = dir + "/Packages" + ext
				break
			}
		}
	}
	return indexes
}

// parseDebPackages reads the files a Packages index lists.
func parseDebPackages(r io.Reader) ([]indexFile, error) {
	var files []indexFile
	var current indexFile
	flush := func() {
		if current.Path != "" {
			files = append(files, current)
		}
		current = indexFile{}
	}
	scanner := bufio.NewScanner(r)
	scanner.Buffer(nil, maxIndexLine)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			flush()
			continue
		}
		key, value, ok := strings.Cut(line, ": ")
		if !ok || strings.HasPrefix(line, " ") {
			continue
		}
		switch key {
		case "Filename":
			current.Path = value
		case "SHA256":
			current.Digest = "sha256:" + value
		}
	}
	flush()
	return files, scanner.Err()
}

// repomdPrimary returns the location of the primary metadata a repomd.xml
// points at.
func repomdPrimary(body []byte) (string, error) {
	var doc struct {
		Data []struct {
			Type     string `xml:"type,attr"`
			Location struct {
				Href string `xml:"href,attr"`
			} `xml:"location"`
		} `xml:"data"`
	}
	if err := xml.Unmarshal(body, &doc); err != nil {
		return "", err
	}
	for _, d := range doc.Data {
		if d.Type == "primary" && d.Location.Href != "" {
			return d.Location.Href, nil
		}
	}
	return "", errors.New("repomd.xml lists no primary metadata")
}

// parseRPMPrimary reads the binary packages a primary.xml lists, one at a
// time. Source packages are left out.
func parseRPMPrimary(r io.Reader) ([]indexFile, error) {
	dec := xml.NewDecoder(r)
	var files []indexFile
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return files, nil
		}
		if err != nil {
			return nil, err
		}
		start, ok := tok.(xml.StartElement)
		if !ok || start.Name.Local != "package" {
			continue
		}
		var pkg struct {
			Arch     string `xml:"arch"`
			Checksum struct {
				Type  string `xml:"type,attr"`
				Value string `xml:",chardata"`
			} `xml:"checksum"`
			Location struct {
				Href string `xml:"href,attr"`
			} `xml:"location"`
		}
		if err := dec.DecodeElement(&pkg, &start); err != nil {
			return nil, err
		}
		if pkg.Arch == "src" || pkg.Location.Href == "" {
			continue
		}
		file := indexFile{Path: pkg.Location.Href}
		if pkg.Checksum.Type == "sha256" {
			file.Digest = "sha256:" + strings.TrimSpace(pkg.Checksum.Value)
		}
		files = append(files, file)
	}
}

// decompressByName returns r decompressed as the extension of name says.
func decompressByName(name string, r io.Reader) (io.ReadCloser, error) {
	switch {
	case strings.HasSuffix(name, ".gz"):
		return gzip.NewReader(r)
	case strings.HasSuffix(name, ".xz"):
		x, err := xz.NewReader(r)
		if err != nil {
			return nil, err
		}
		return io.NopCloser(x), nil
	case strings.HasSuffix(name, ".zst"):
		z, err := zstd.NewReader(r)
		if err != nil {
			return nil, err
		}
		return z.IOReadCloser(), nil
	case strings.HasSuffix(name, ".bz2"):
		return io.NopCloser(bzip2.NewReader(r)), nil
	case strings.HasSuffix(name, ".xml"), !strings.Contains(name[strings.LastIndex(name, "/")+1:], "."):
		return io.NopCloser(r), nil
	default:
		return nil, fmt.Errorf("unsupported compression: %s", name)
	}
}
