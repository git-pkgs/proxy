package mirror

import (
	"maps"
	"testing"
)

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
