// Package npmbench builds npm packuments for benchmarks.
package npmbench

import (
	"fmt"
	"strings"
)

// readmeSentences makes each version's readme about 4KB.
const readmeSentences = 160

// Packument builds a full packument shaped like a large real one: versions
// 1.0.0 to 1.0.{versions-1}, each with dependencies, scripts and a readme,
// and a time entry for each.
func Packument(versions int) []byte {
	var b strings.Builder
	readme := strings.Repeat("Some documentation text. ", readmeSentences)
	b.WriteString(`{"_id":"big","name":"big","dist-tags":{"latest":"1.0.` + fmt.Sprint(versions-1) + `"},"versions":{`)
	for i := range versions {
		if i > 0 {
			b.WriteByte(',')
		}
		v := fmt.Sprintf("1.0.%d", i)
		fmt.Fprintf(&b, `%q:{"name":"big","version":%q,"description":"A big package","main":"index.js",`+
			`"scripts":{"test":"node test.js","build":"tsc -p ."},"dependencies":{`, v, v)
		for d := range 25 {
			if d > 0 {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, `"dep-%d":"^%d.0.0"`, d, d)
		}
		fmt.Fprintf(&b, `},"readme":%q,"dist":{"shasum":"0123456789abcdef0123456789abcdef01234567",`+
			`"integrity":"sha512-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA==",`+
			`"tarball":"https://registry.npmjs.org/big/-/big-%s.tgz"}}`, readme, v)
	}
	b.WriteString(`},"time":{"created":"2015-01-01T00:00:00.000Z"`)
	for i := range versions {
		fmt.Fprintf(&b, `,"1.0.%d":"2016-01-01T00:00:00.000Z"`, i)
	}
	b.WriteString(`}}`)
	return []byte(b.String())
}
