package metrics

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// isolateRecentFailures empties the process-wide failure history for the
// test and again when it ends, so neither earlier tests nor repeated runs
// reach its assertions.
func isolateRecentFailures(t *testing.T) {
	t.Helper()
	reset := func() {
		recentFailures.Lock()
		defer recentFailures.Unlock()
		recentFailures.series = make(map[string][]Failure)
	}
	reset()
	t.Cleanup(reset)
}

func TestRecentFailuresNewestFirst(t *testing.T) {
	isolateRecentFailures(t)

	RecordUpstreamError("npm", "fetch_failed", FailureDetail{PURL: "pkg:npm/left-pad@1.0.0"})
	RecordUpstreamError("npm", "fetch_failed", FailureDetail{PURL: "pkg:npm/right-pad@2.0.0"})

	got := RecentFailures("proxy_upstream_errors_total", map[string]string{"ecosystem": "npm", "error_type": "fetch_failed"})
	if len(got) != 2 {
		t.Fatalf("got %d failures, want 2", len(got))
	}
	if got[0].PURL != "pkg:npm/right-pad@2.0.0" || got[1].PURL != "pkg:npm/left-pad@1.0.0" {
		t.Errorf("failures not newest first: %q, %q", got[0].PURL, got[1].PURL)
	}
	if got[0].At.IsZero() {
		t.Error("failure has no timestamp")
	}
}

func TestRecentFailuresAreBoundedPerSeries(t *testing.T) {
	isolateRecentFailures(t)

	for i := range recentFailuresPerSeries + 5 {
		RecordStorageError("write", FailureDetail{Path: fmt.Sprintf("npm/pkg-%d.tgz", i)})
	}

	got := RecentFailures("proxy_storage_errors_total", map[string]string{"operation": "write"})
	if len(got) != recentFailuresPerSeries {
		t.Fatalf("got %d failures, want the cap of %d", len(got), recentFailuresPerSeries)
	}
	if want := fmt.Sprintf("npm/pkg-%d.tgz", recentFailuresPerSeries+4); got[0].Path != want {
		t.Errorf("newest failure = %q, want %q", got[0].Path, want)
	}
}

func TestRecentFailuresAreKeptPerSeries(t *testing.T) {
	isolateRecentFailures(t)

	RecordIntegrityFailure("npm", FailureDetail{PURL: "pkg:npm/a@1"})
	RecordIntegrityFailure("pypi", FailureDetail{PURL: "pkg:pypi/b@1"})

	npm := RecentFailures("proxy_integrity_failures_total", map[string]string{"ecosystem": "npm"})
	if len(npm) != 1 || npm[0].PURL != "pkg:npm/a@1" {
		t.Errorf("npm series = %+v, want only pkg:npm/a@1", npm)
	}
	if got := RecentFailures("proxy_integrity_failures_total", map[string]string{"ecosystem": "deb"}); got != nil {
		t.Errorf("unseen series = %+v, want nil", got)
	}
}

func TestRecordFailureStillCountsTheMetric(t *testing.T) {
	isolateRecentFailures(t)

	before := testutil.ToFloat64(HealthProbeFailures.WithLabelValues("read"))
	RecordHealthProbeFailure("read", FailureDetail{Err: errors.New("disk gone")})
	if diff := testutil.ToFloat64(HealthProbeFailures.WithLabelValues("read")) - before; diff != 1 {
		t.Errorf("counter delta = %.0f, want 1", diff)
	}

	got := RecentFailures("proxy_health_probe_failures_total", map[string]string{"step": "read"})
	if len(got) != 1 || got[0].Error != "disk gone" {
		t.Errorf("failures = %+v, want one carrying the error text", got)
	}
}

func TestScanErrorRecordsDetail(t *testing.T) {
	isolateRecentFailures(t)

	RecordScanError("NPM", "clamav", "timeout", FailureDetail{PURL: "pkg:npm/a@1", Filename: "a-1.tgz"})

	got := RecentFailures("proxy_scan_errors_total",
		map[string]string{"ecosystem": "npm", "scanner": "clamav", "error_type": "timeout"})
	if len(got) != 1 || got[0].Filename != "a-1.tgz" {
		t.Errorf("failures = %+v, want one keyed by the normalised ecosystem", got)
	}
}

// The analytics page is unauthenticated, and private registry URLs carry
// credentials in the userinfo or the query string. Neither may be kept.
func TestRecentFailuresStripCredentials(t *testing.T) {
	isolateRecentFailures(t)

	RecordUpstreamError("npm", "fetch_failed", FailureDetail{
		URL: "https://user:s3cret@registry.example.com/pkg/-/pkg-1.0.0.tgz?token=abc#frag",
		Err: errors.New(`Get "https://user:s3cret@registry.example.com/pkg?sig=xyz": connection refused`),
	})

	got := RecentFailures("proxy_upstream_errors_total", map[string]string{"ecosystem": "npm", "error_type": "fetch_failed"})
	if len(got) != 1 {
		t.Fatalf("got %d failures, want 1", len(got))
	}
	if want := "https://registry.example.com/pkg/-/pkg-1.0.0.tgz"; got[0].URL != want {
		t.Errorf("URL = %q, want %q", got[0].URL, want)
	}
	if want := `Get "https://registry.example.com/pkg": connection refused`; got[0].Error != want {
		t.Errorf("Error = %q, want %q", got[0].Error, want)
	}
	for _, secret := range []string{"s3cret", "token", "sig=", "user:"} {
		if strings.Contains(got[0].URL+got[0].Error, secret) {
			t.Errorf("recorded failure still contains %q", secret)
		}
	}
}

func TestRecentFailuresReturnsACopy(t *testing.T) {
	isolateRecentFailures(t)

	RecordStorageError("read", FailureDetail{Path: "a"})
	labels := map[string]string{"operation": "read"}
	RecentFailures("proxy_storage_errors_total", labels)[0].Path = "mutated"

	if got := RecentFailures("proxy_storage_errors_total", labels)[0].Path; got != "a" {
		t.Errorf("stored failure was mutated through the returned slice: %q", got)
	}
}
