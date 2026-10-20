package metrics

import (
	"net/url"
	"regexp"
	"sync"
	"time"
)

// recentFailuresPerSeries bounds how many failures are kept for each series of
// a failure counter. The series set is already bounded by the counter's
// labels, so this caps the whole store.
const recentFailuresPerSeries = 20

// FailureDetail is what a failure counter's call site knows beyond the
// counter's labels: which artifact failed, where it came from or was going,
// and why. Every field is optional.
//
// None of it becomes a metric label. Package names and URLs are unbounded, so
// they are kept in a small in-memory store for /ui/analytics instead.
type FailureDetail struct {
	PURL     string
	Filename string
	URL      string
	Path     string
	Err      error
}

// Failure is one recorded failure, with credentials already stripped from the
// URL and the error text.
type Failure struct {
	At       time.Time
	PURL     string
	Filename string
	URL      string
	Path     string
	Error    string
}

// recentFailures holds the latest failures per counter series. It lives in
// process memory only, so like the counters themselves it starts empty when
// the proxy starts.
var recentFailures = struct {
	sync.Mutex
	series map[string][]Failure
}{series: make(map[string][]Failure)}

// RecentFailures returns the latest failures recorded for one series of a
// failure counter, newest first, or nil when the series has none.
func RecentFailures(name string, labels map[string]string) []Failure {
	recentFailures.Lock()
	defer recentFailures.Unlock()

	stored := recentFailures.series[failureKey(name, labels)]
	if len(stored) == 0 {
		return nil
	}
	return append([]Failure(nil), stored...)
}

func recordFailure(name string, labels map[string]string, detail FailureDetail) {
	failure := Failure{
		At:       time.Now(),
		PURL:     detail.PURL,
		Filename: detail.Filename,
		URL:      redactURL(detail.URL),
		Path:     detail.Path,
	}
	if detail.Err != nil {
		failure.Error = redactURLsIn(detail.Err.Error())
	}

	key := failureKey(name, labels)

	recentFailures.Lock()
	defer recentFailures.Unlock()

	stored := append([]Failure{failure}, recentFailures.series[key]...)
	if len(stored) > recentFailuresPerSeries {
		stored = stored[:recentFailuresPerSeries]
	}
	recentFailures.series[key] = stored
}

func failureKey(name string, labels map[string]string) string {
	return name + "{" + labelKey(labels) + "}"
}

var urlInText = regexp.MustCompile(`[a-zA-Z][a-zA-Z0-9+.-]*://[^\s"'<>]+`)

// redactURLsIn strips credentials from every URL embedded in text, such as the
// one net/http quotes in a failed request's error.
func redactURLsIn(text string) string {
	return urlInText.ReplaceAllStringFunc(text, redactURL)
}

// redactURL drops the userinfo, query and fragment, which is where registry
// tokens and signed-URL signatures live.
func redactURL(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "(unparseable URL)"
	}
	u.User = nil
	u.RawQuery = ""
	u.ForceQuery = false
	u.Fragment = ""
	u.RawFragment = ""
	return u.String()
}
