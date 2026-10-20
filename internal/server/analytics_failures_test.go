package server

import (
	"errors"
	"strings"
	"testing"

	"github.com/git-pkgs/proxy/internal/metrics"
	"github.com/prometheus/client_golang/prometheus"
)

func storageErrorSnapshot(t *testing.T, operation string) *metrics.Snapshot {
	t.Helper()
	reg := prometheus.NewRegistry()
	errs := prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "proxy_storage_errors_total", Help: "t"}, []string{"operation"})
	reg.MustRegister(errs)
	errs.WithLabelValues(operation).Inc()

	snap, err := metrics.GatherFrom(reg)
	if err != nil {
		t.Fatalf("GatherFrom: %v", err)
	}
	return snap
}

func TestRuntimeViewAttachesRecentFailuresWhenEnabled(t *testing.T) {
	const operation = "test-attach"
	metrics.RecordStorageError(operation, metrics.FailureDetail{
		PURL: "pkg:npm/left-pad@1.0.0", Filename: "left-pad-1.0.0.tgz", Path: "npm/left-pad", Err: errors.New("disk full")})

	v := runtimeView(storageErrorSnapshot(t, operation), true)

	if !v.FailureDetailsOn {
		t.Error("FailureDetailsOn = false, want true")
	}
	if len(v.StorageErrors) != 1 || len(v.StorageErrors[0].Recent) == 0 {
		t.Fatalf("StorageErrors = %+v, want one row with recent failures", v.StorageErrors)
	}
	got := v.StorageErrors[0].Recent[0]
	if got.PURL != "pkg:npm/left-pad@1.0.0" || got.Filename != "left-pad-1.0.0.tgz" ||
		got.Path != "npm/left-pad" || got.Error != "disk full" || got.Age == "" {
		t.Errorf("recent failure = %+v", got)
	}
}

func TestRuntimeViewOmitsRecentFailuresWhenDisabled(t *testing.T) {
	const operation = "test-omit"
	metrics.RecordStorageError(operation, metrics.FailureDetail{PURL: "pkg:npm/left-pad@1.0.0"})

	v := runtimeView(storageErrorSnapshot(t, operation), false)

	if v.FailureDetailsOn {
		t.Error("FailureDetailsOn = true, want false")
	}
	if len(v.StorageErrors) != 1 || v.StorageErrors[0].Recent != nil {
		t.Errorf("StorageErrors = %+v, want the count without recent failures", v.StorageErrors)
	}
}

// Package names and upstream URLs say what the proxy's users depend on, so like
// the caller table they are published only when asked for.
func TestAnalyticsPageHidesFailureDetailsByDefault(t *testing.T) {
	ts := newTestServer(t)
	defer ts.close()

	if ts.server.cfg.UIFailureDetails {
		t.Fatal("UIFailureDetails defaults to true; it must default off")
	}
	metrics.RecordUpstreamError("npm", "fetch_failed", metrics.FailureDetail{PURL: "pkg:npm/hidden-by-default@1.0.0"})

	body := ts.getOK(t, "/ui/analytics")
	if strings.Contains(body, "hidden-by-default") {
		t.Error("a failed package rendered with ui_failure_details off")
	}
	if !strings.Contains(body, "ui_failure_details") {
		t.Error("the off-state note naming the config key did not render")
	}
}

func TestAnalyticsPageShowsFailureDetailsWhenEnabled(t *testing.T) {
	ts := newTestServer(t)
	defer ts.close()

	ts.server.cfg.UIFailureDetails = true
	metrics.RecordUpstreamError("npm", "fetch_failed", metrics.FailureDetail{
		PURL: "pkg:npm/shown-when-enabled@1.0.0",
		URL:  "https://registry.example.com/shown-when-enabled.tgz?token=secret",
		Err:  errors.New("upstream returned 503"),
	})

	body := ts.getOK(t, "/ui/analytics")
	for _, want := range []string{
		"pkg:npm/shown-when-enabled@1.0.0",
		"https://registry.example.com/shown-when-enabled.tgz",
		"upstream returned 503",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("rendered page missing %q", want)
		}
	}
	if strings.Contains(body, "token=secret") {
		t.Error("rendered page leaks the URL's query string")
	}
}
