package handler

import (
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestPyPIParseReleaseTimes(t *testing.T) {
	h := &PyPIHandler{}
	body := `{
		"info": {"name": "pkg", "version": "3.0.0"},
		"releases": {
			"1.0.0": [
				{"filename": "pkg-1.0.0.tar.gz", "upload_time_iso_8601": "2024-01-01T10:00:00Z"},
				{"filename": "pkg-1.0.0-py3-none-any.whl", "upload_time_iso_8601": "2024-01-02T10:00:00.123456Z"}
			],
			"2.0.0": [],
			"3.0.0": [
				42,
				{"upload_time_iso_8601": 5},
				{"upload_time_iso_8601": null},
				{"upload_time_iso_8601": "not a time"},
				{"upload_time": "2024-06-01T00:00:00"},
				{"upload_time_iso_8601": "2024-03-01T00:00:00Z"}
			],
			"4.0.0": "not a list"
		},
		"urls": [{"upload_time_iso_8601": "2030-01-01T00:00:00Z"}]
	}`

	times, err := h.parseReleaseTimes([]byte(body))
	if err != nil {
		t.Fatalf("parseReleaseTimes failed: %v", err)
	}
	want := map[string]time.Time{
		"1.0.0": time.Date(2024, 1, 2, 10, 0, 0, 123456000, time.UTC),
		"2.0.0": {},
		"3.0.0": time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC),
		"4.0.0": {},
	}
	for version, wantTime := range want {
		if got := times[version]; !got.Equal(wantTime) {
			t.Errorf("times[%q] = %v, want %v", version, got, wantTime)
		}
	}
	for version := range times {
		if _, ok := want[version]; !ok {
			t.Errorf("unexpected release %q", version)
		}
	}
}

func TestPyPIParseReleaseTimesRejectsDocumentWithoutReleases(t *testing.T) {
	h := &PyPIHandler{}
	for _, body := range []string{
		`{"info": {"name": "pkg"}}`,
		`{"releases": null}`,
		`{"releases": []}`,
		`[]`,
		`{"releases": {"1.0.0": [`,
		`not json`,
	} {
		if times, err := h.parseReleaseTimes([]byte(body)); err == nil {
			t.Errorf("parseReleaseTimes(%s) = %v, want an error", body, times)
		}
	}

	times, err := h.parseReleaseTimes([]byte(`{"releases": {}}`))
	if err != nil || len(times) != 0 {
		t.Errorf("parseReleaseTimes with no releases = %v, %v; want an empty result", times, err)
	}
}

// pypiJSONDocument builds a JSON API document shaped like PyPI's, with the
// file fields that make up most of a real one.
func pypiJSONDocument(releases, filesPerRelease int) []byte {
	var b strings.Builder
	b.WriteString(`{"info":{"name":"pkg","summary":"A package"},"releases":{`)
	for r := range releases {
		if r > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `"1.%d.0":[`, r)
		for f := range filesPerRelease {
			if f > 0 {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, `{"comment_text":"","digests":{"blake2b_256":"%064x","md5":"%032x","sha256":"%064x"},`+
				`"downloads":-1,"filename":"pkg-1.%d.0-cp3%d-cp3%d-manylinux_2_17_x86_64.whl","has_sig":false,`+
				`"md5_digest":"%032x","packagetype":"bdist_wheel","python_version":"cp3%d","requires_python":">=3.8",`+
				`"size":123456,"upload_time":"2024-01-02T03:04:05","upload_time_iso_8601":"2024-01-02T03:04:05.123456Z",`+
				`"url":"https://files.pythonhosted.org/packages/ab/cd/%060x/pkg-1.%d.0-cp3%d-cp3%d-manylinux_2_17_x86_64.whl",`+
				`"yanked":false,"yanked_reason":null}`, r, f, r, r, f, f, f, f, r, r, f, f)
		}
		b.WriteByte(']')
	}
	b.WriteString(`},"urls":[],"vulnerabilities":[]}`)
	return []byte(b.String())
}

// Reading the release times must not build the whole document in memory: it
// runs on every simple index request for a package under cooldown.
func TestPyPIParseReleaseTimesAllocatesLessThanDocument(t *testing.T) {
	h := &PyPIHandler{}
	body := pypiJSONDocument(300, 12)

	const runs = 5
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for range runs {
		times, err := h.parseReleaseTimes(body)
		if err != nil || len(times) != 300 {
			t.Fatalf("parseReleaseTimes = %d releases, %v", len(times), err)
		}
	}
	runtime.ReadMemStats(&after)

	perRun := (after.TotalAlloc - before.TotalAlloc) / runs
	if perRun > uint64(len(body)) {
		t.Errorf("parseReleaseTimes allocated %d bytes for a %d byte document", perRun, len(body))
	}
}
