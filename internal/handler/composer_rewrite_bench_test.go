package handler

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// benchComposerMetadata builds minified Composer v2 metadata shaped like a
// large real package: thousands of versions, where the first carries every
// field and the rest mostly change version, time, dist and source, with the
// requirements changing every so often.
func benchComposerMetadata(versions int) []byte {
	var b strings.Builder
	b.WriteString(`{"minified":"composer/2.0","packages":{"big/sdk":[`)
	for i := range versions {
		if i > 0 {
			b.WriteByte(',')
		}
		v := fmt.Sprintf("3.%d.0", versions-i)
		ref := fmt.Sprintf("%040x", i)
		b.WriteByte('{')
		if i == 0 {
			b.WriteString(`"name":"big/sdk","description":"A big SDK for a big cloud",` +
				`"keywords":["cloud","sdk","api","storage","queue"],"homepage":"https://example.com/sdk",` +
				`"license":["Apache-2.0"],"authors":[{"name":"Big Cloud","homepage":"https://example.com"}],` +
				`"type":"library","autoload":{"psr-4":{"Big\\Sdk\\":"src/"},"files":["src/functions.php"]},` +
				`"support":{"issues":"https://example.com/sdk/issues","source":"https://example.com/sdk/tree/main"},` +
				`"extra":{"branch-alias":{"dev-master":"3.0-dev"}},`)
		}
		if i%50 == 0 {
			b.WriteString(`"require":{"php":">=8.1",`)
			for d := range 12 {
				if d > 0 {
					b.WriteByte(',')
				}
				fmt.Fprintf(&b, `"vendor/dep-%d":"^%d.%d"`, d, d+1, i/50)
			}
			b.WriteString(`},`)
		}
		fmt.Fprintf(&b, `"version":%q,"version_normalized":"%s.0","time":"2024-01-01T00:00:00+00:00",`+
			`"source":{"url":"https://github.com/big/sdk.git","type":"git","reference":%q},`+
			`"dist":{"url":"https://api.github.com/repos/big/sdk/zipball/%s","type":"zip","shasum":"","reference":%q}}`,
			v, v, ref, ref, ref)
	}
	b.WriteString(`]}}`)
	return []byte(b.String())
}

func BenchmarkComposerRewriteMetadata(b *testing.B) {
	body := benchComposerMetadata(5000)
	h := &ComposerHandler{proxy: testProxy(), proxyURL: "http://proxy.example"}
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for b.Loop() {
		if _, err := h.rewriteMetadata(body); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkComposerFindDownloadURL(b *testing.B) {
	body := benchComposerMetadata(5000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	h := &ComposerHandler{proxy: testProxy(), proxyURL: "http://proxy.example"}
	h.proxy.HTTPClient = srv.Client()
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for b.Loop() {
		url, err := h.findDownloadURLFromMetadata(context.Background(), srv.URL, "big/sdk", "3.2500.0")
		if err != nil || url == "" {
			b.Fatalf("download URL not found: %q, %v", url, err)
		}
	}
}
