package handler

import (
	"fmt"
	"strings"
	"testing"
)

// benchPackument builds a full packument shaped like a large real one:
// thousands of versions, each with dependencies, scripts and a readme.
func benchPackument(versions int) []byte {
	var b strings.Builder
	readme := strings.Repeat("Some documentation text. ", 160)
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

func BenchmarkNPMRewriteMetadata(b *testing.B) {
	body := benchPackument(5000)
	h := &NPMHandler{proxy: testProxy(), proxyURL: "http://proxy.example"}
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for b.Loop() {
		if _, err := h.rewriteMetadata("big", body); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkNPMVersionTarball(b *testing.B) {
	body := benchPackument(5000)
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for b.Loop() {
		if npmVersionTarball(body, "1.0.2500") == "" {
			b.Fatal("tarball not found")
		}
	}
}
