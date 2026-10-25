package mirror

import (
	"maps"
	"os"
	"testing"
)

// testdata/cran-archive-PACKAGES.rds was written by R 4.4.1 with saveRDS,
// shaped like the PACKAGES.rds in CRAN's src/contrib/Archive/{name}: a
// character matrix of fields, NA where a version has none.
func TestParseCRANArchiveRDS(t *testing.T) {
	data, err := os.ReadFile("testdata/cran-archive-PACKAGES.rds")
	if err != nil {
		t.Fatal(err)
	}
	data, err = gunzipLimited(data)
	if err != nil {
		t.Fatal(err)
	}

	sums, err := parseCRANArchiveRDS(data)
	if err != nil {
		t.Fatal(err)
	}

	want := map[string]string{
		"0.9": "b7f01da919c10e36d6777fc337f48dfa",
		"1.0": "e51e3cbb209131a84f3ff5938dd93602",
	}
	if !maps.Equal(sums, want) {
		t.Errorf("sums = %v, want %v", sums, want)
	}

	for i := range data {
		if _, err := parseCRANArchiveRDS(data[:i]); err == nil {
			t.Fatalf("truncated to %d bytes: no error", i)
		}
	}
}

func TestParseCRANPackages(t *testing.T) {
	data := []byte(`Package: A3
Version: 1.0.0
Depends: R (>= 2.15.0), xtable,
        pbapply
MD5sum: 027ebdd8affce8f0effaecfcd5f5ade2
NeedsCompilation: no

Package: abc
Version: 2.2.1
NeedsCompilation: no
`)
	want := map[string]string{
		"A3_1.0.0.tar.gz":  "027ebdd8affce8f0effaecfcd5f5ade2",
		"abc_2.2.1.tar.gz": "",
	}
	if got, err := parseCRANPackages(data); err != nil || !maps.Equal(got, want) {
		t.Errorf("parseCRANPackages = %v, %v, want %v", got, err, want)
	}
}

// FuzzReadRDSStringMatrix feeds the decoder arbitrary bytes, seeded with a
// real PACKAGES.rds: it reads files from the network, so it must reject
// anything malformed with an error rather than panic or allocate without
// bound.
func FuzzReadRDSStringMatrix(f *testing.F) {
	data, err := os.ReadFile("testdata/cran-archive-PACKAGES.rds")
	if err != nil {
		f.Fatal(err)
	}
	if data, err = gunzipLimited(data); err != nil {
		f.Fatal(err)
	}
	f.Add(data)
	f.Add(data[:len(data)/2])
	f.Fuzz(func(t *testing.T, b []byte) {
		values, nrow, columns, err := readRDSStringMatrix(b)
		if err == nil && nrow > 0 && len(values) != nrow*len(columns) {
			t.Errorf("%d values for %d rows and %d columns", len(values), nrow, len(columns))
		}
	})
}
