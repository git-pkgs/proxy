package mirror

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"reflect"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// apkIndexArchive packs an APKINDEX into the tar.gz Alpine serves.
func apkIndexArchive(t *testing.T, index string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range map[string]string{"DESCRIPTION": "v3.20.3-1-g0000000", "APKINDEX": index} {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestParseAPKIndex(t *testing.T) {
	index := "C:Q1abc=\nP:curl\nV:8.9.1-r0\nA:aarch64\nS:100\n\nC:Q1def=\nP:busybox\nV:1.36.1-r29\nA:aarch64\n\n"
	got, err := parseAPKIndex(bytes.NewReader(apkIndexArchive(t, index)))
	if err != nil {
		t.Fatal(err)
	}
	want := []indexFile{{Path: "curl-8.9.1-r0.apk"}, {Path: "busybox-1.36.1-r29.apk"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseAPKIndex = %+v, want %+v", got, want)
	}
}

const debRelease = `-----BEGIN PGP SIGNED MESSAGE-----
Hash: SHA256

Origin: Debian
Suite: stable
Codename: bookworm
Architectures: all amd64 arm64
Components: main contrib
MD5Sum:
 0123 100 main/binary-amd64/Packages.xz
SHA256:
 aaaa 1000 main/binary-amd64/Packages
 bbbb 200 main/binary-amd64/Packages.xz
 cccc 300 main/binary-amd64/Packages.gz
 dddd 400 main/binary-arm64/Packages.gz
 eeee 500 contrib/binary-amd64/Packages.xz
 ffff 600 main/i18n/Translation-en.bz2
-----BEGIN PGP SIGNATURE-----
xyz
-----END PGP SIGNATURE-----
`

func TestDebPackagesIndexes(t *testing.T) {
	got := debPackagesIndexes([]byte(debRelease))
	want := map[string]string{
		"main/binary-amd64":    "main/binary-amd64/Packages.xz",
		"main/binary-arm64":    "main/binary-arm64/Packages.gz",
		"contrib/binary-amd64": "contrib/binary-amd64/Packages.xz",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("debPackagesIndexes = %v, want %v", got, want)
	}
}

func TestParseDebPackages(t *testing.T) {
	packages := `Package: curl
Version: 7.88.1-10+deb12u8
Architecture: amd64
Description: a tool
 with a long description
Filename: pool/main/c/curl/curl_7.88.1-10+deb12u8_amd64.deb
SHA256: 1111

Package: zlib1g
Version: 1:1.2.13.dfsg-1
Architecture: amd64
Filename: pool/main/z/zlib/zlib1g_1.2.13.dfsg-1_amd64.deb
SHA256: 2222
`
	got, err := parseDebPackages(strings.NewReader(packages))
	if err != nil {
		t.Fatal(err)
	}
	want := []indexFile{
		{Path: "pool/main/c/curl/curl_7.88.1-10+deb12u8_amd64.deb", Digest: "sha256:1111"},
		{Path: "pool/main/z/zlib/zlib1g_1.2.13.dfsg-1_amd64.deb", Digest: "sha256:2222"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseDebPackages = %+v, want %+v", got, want)
	}
}

const repomd = `<?xml version="1.0" encoding="UTF-8"?>
<repomd xmlns="http://linux.duke.edu/metadata/repo" xmlns:rpm="http://linux.duke.edu/metadata/rpm">
  <revision>1700000000</revision>
  <data type="filelists"><location href="repodata/aaaa-filelists.xml.zst"/></data>
  <data type="primary"><location href="repodata/bbbb-primary.xml.zst"/></data>
</repomd>
`

func TestRepomdPrimary(t *testing.T) {
	got, err := repomdPrimary([]byte(repomd))
	if err != nil || got != "repodata/bbbb-primary.xml.zst" {
		t.Errorf("repomdPrimary = %q, %v", got, err)
	}
	if _, err := repomdPrimary([]byte(`<repomd></repomd>`)); err == nil {
		t.Error("repomdPrimary accepted a repomd without primary")
	}
}

const rpmPrimary = `<?xml version="1.0" encoding="UTF-8"?>
<metadata xmlns="http://linux.duke.edu/metadata/common" xmlns:rpm="http://linux.duke.edu/metadata/rpm" packages="3">
<package type="rpm">
  <name>curl</name>
  <arch>x86_64</arch>
  <version epoch="0" ver="8.6.0" rel="10.fc40"/>
  <checksum type="sha256" pkgid="YES">3333</checksum>
  <location href="Packages/c/curl-8.6.0-10.fc40.x86_64.rpm"/>
</package>
<package type="rpm">
  <name>curl</name>
  <arch>src</arch>
  <version epoch="0" ver="8.6.0" rel="10.fc40"/>
  <checksum type="sha256" pkgid="YES">4444</checksum>
  <location href="Packages/c/curl-8.6.0-10.fc40.src.rpm"/>
</package>
<package type="rpm">
  <name>tzdata</name>
  <arch>noarch</arch>
  <version epoch="0" ver="2024a" rel="5.fc40"/>
  <checksum type="sha1" pkgid="YES">5555</checksum>
  <location href="Packages/t/tzdata-2024a-5.fc40.noarch.rpm"/>
</package>
</metadata>
`

func TestParseRPMPrimary(t *testing.T) {
	got, err := parseRPMPrimary(strings.NewReader(rpmPrimary))
	if err != nil {
		t.Fatal(err)
	}
	want := []indexFile{
		{Path: "Packages/c/curl-8.6.0-10.fc40.x86_64.rpm", Digest: "sha256:3333"},
		{Path: "Packages/t/tzdata-2024a-5.fc40.noarch.rpm"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseRPMPrimary = %+v, want %+v", got, want)
	}
}

func TestDecompressByName(t *testing.T) {
	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	_, _ = w.Write([]byte("hello"))
	_ = w.Close()
	enc, _ := zstd.NewWriter(nil)
	zst := enc.EncodeAll([]byte("hello"), nil)

	for name, data := range map[string][]byte{"Packages": []byte("hello"), "Packages.gz": gz.Bytes(), "primary.xml.zst": zst} {
		r, err := decompressByName(name, bytes.NewReader(data))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var out bytes.Buffer
		if _, err := out.ReadFrom(r); err != nil || out.String() != "hello" {
			t.Errorf("%s: got %q, %v", name, out.String(), err)
		}
		_ = r.Close()
	}
}
