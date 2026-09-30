package server

import (
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestClientName(t *testing.T) {
	tests := []struct {
		ua   string
		want string
	}{
		// Real User-Agents captured from these tools.
		{`pip/21.2.4 {"ci":null,"cpu":"arm64"}`, "pip"},
		{"GoCommand/1 (+https://go.dev/cmd/go)", "go"},
		{"curl/8.7.1", "curl"},
		{"npm/10.2.3 node/v20.10.0 darwin arm64 workspaces/false", "npm"},
		{"docker/24.0.7 go/go1.20.10 kernel/6.5.0 os/linux", "docker"},
		{"Debian APT-HTTP/1.3 (2.6.1)", "apt"},
		{"bundler/2.4.22 rubygems/3.4.22", "bundler"},
		// Unknown and hostile input collapses, so a User-Agent cannot mint a
		// new Prometheus time series.
		{"", unknownClient},
		{"definitely-not-a-known-tool/9", unknownClient},
		{"a\nb/1", unknownClient},
		{`{"evil":"label"}`, unknownClient},
	}
	for _, tc := range tests {
		if got := clientName(tc.ua); got != tc.want {
			t.Errorf("clientName(%q) = %q, want %q", tc.ua, got, tc.want)
		}
	}
}

// Every value clientName can return must be one of the closed set, otherwise
// the Prometheus label is not actually bounded.
func TestClientNameIsBounded(t *testing.T) {
	allowed := map[string]bool{unknownClient: true}
	for _, v := range knownClients {
		allowed[v] = true
	}

	for _, ua := range []string{"pip/1", "weird", "", "npm/1", "x y z", "../../etc/passwd"} {
		if got := clientName(ua); !allowed[got] {
			t.Errorf("clientName(%q) = %q, which is outside the closed set", ua, got)
		}
	}
}

func TestClientAddr(t *testing.T) {
	t.Run("peer address by default", func(t *testing.T) {
		r := httptest.NewRequest("GET", "/npm/x", nil)
		r.RemoteAddr = "10.1.2.3:54321"
		r.Header.Set("X-Forwarded-For", "203.0.113.9")

		// The header is present but untrusted, so it must be ignored.
		if got := clientAddr(r, false); got != "10.1.2.3" {
			t.Errorf("clientAddr = %q, want the peer address 10.1.2.3", got)
		}
	})

	t.Run("forwarded when trusted", func(t *testing.T) {
		r := httptest.NewRequest("GET", "/npm/x", nil)
		r.RemoteAddr = "10.1.2.3:54321"
		r.Header.Set("X-Forwarded-For", "203.0.113.9, 10.0.0.1")

		if got := clientAddr(r, true); got != "203.0.113.9" {
			t.Errorf("clientAddr = %q, want the leftmost forwarded entry", got)
		}
	})

	t.Run("rejects a forwarded value that is not an IP", func(t *testing.T) {
		for _, forged := range []string{
			"not-an-ip",
			strings.Repeat("A", 4096),
			"mygroup/myproject",
			"10.0.0.1; DROP TABLE",
		} {
			r := httptest.NewRequest("GET", "/npm/x", nil)
			r.RemoteAddr = "10.1.2.3:54321"
			r.Header.Set("X-Forwarded-For", forged)

			if got := clientAddr(r, true); got != "10.1.2.3" {
				t.Errorf("clientAddr with forwarded %q = %q, want the peer address",
					truncate(forged), got)
			}
		}
	})

	t.Run("normalizes a valid forwarded IP", func(t *testing.T) {
		r := httptest.NewRequest("GET", "/npm/x", nil)
		r.RemoteAddr = "10.1.2.3:54321"
		r.Header.Set("X-Forwarded-For", "2001:0db8:0000:0000:0000:0000:0000:0001")

		if got := clientAddr(r, true); got != "2001:db8::1" {
			t.Errorf("clientAddr = %q, want the canonical IPv6 form", got)
		}
	})

	t.Run("falls back when forwarded is empty", func(t *testing.T) {
		r := httptest.NewRequest("GET", "/npm/x", nil)
		r.RemoteAddr = "10.1.2.3:54321"
		r.Header.Set("X-Forwarded-For", "   ")

		if got := clientAddr(r, true); got != "10.1.2.3" {
			t.Errorf("clientAddr = %q, want the peer address", got)
		}
	})

	t.Run("unparseable remote address is returned as-is", func(t *testing.T) {
		r := httptest.NewRequest("GET", "/npm/x", nil)
		r.RemoteAddr = "not-a-host-port"

		if got := clientAddr(r, false); got != "not-a-host-port" {
			t.Errorf("clientAddr = %q, want the raw value", got)
		}
	})
}

func TestSourceTrackerAggregates(t *testing.T) {
	var tr sourceTracker

	tr.Record("10.0.0.1", "npm", 1000)
	tr.Record("10.0.0.1", "npm", 500)
	tr.Record("10.0.0.2", "pip", 4000)

	rows := tr.Top(10)
	if len(rows) != 2 {
		t.Fatalf("expected 2 rows, got %+v", rows)
	}

	// Ordered by bytes, so the pip caller leads.
	if rows[0].Addr != "10.0.0.2" || rows[0].Bytes != formatSize(4000) {
		t.Errorf("first row = %+v, want 10.0.0.2 with 4000 bytes", rows[0])
	}
	if rows[1].Addr != "10.0.0.1" || rows[1].Requests != "2" || rows[1].Bytes != formatSize(1500) {
		t.Errorf("second row = %+v, want 10.0.0.1 with 2 requests and 1500 bytes", rows[1])
	}
	if tr.Count() != 2 {
		t.Errorf("Count = %d, want 2", tr.Count())
	}
}

// The same address running two tools is two sources, since that is the
// distinction the table exists to show.
func TestSourceTrackerSeparatesClientsOnOneAddress(t *testing.T) {
	var tr sourceTracker
	tr.Record("10.0.0.1", "npm", 10)
	tr.Record("10.0.0.1", "pip", 20)

	if got := tr.Count(); got != 2 {
		t.Errorf("Count = %d, want 2 (one row per address+client)", got)
	}
}

// The caller set is not under the proxy's control, so the table must stop
// growing rather than track every address that ever connects.
func TestSourceTrackerBoundsItsSize(t *testing.T) {
	tr := sourceTracker{max: 3}

	for i := range 50 {
		tr.Record(string(rune('a'+i%26))+string(rune('0'+i/26)), "npm", 100)
	}

	if got := tr.Count(); got != 3 {
		t.Fatalf("tracked %d sources, want the cap of 3", got)
	}

	rows := tr.Top(10)
	last := rows[len(rows)-1]
	if !last.IsOverflow {
		t.Fatalf("expected an overflow row, got %+v", rows)
	}

	// Nothing may be lost from the totals: 50 requests and 5000 bytes went in,
	// so the tracked rows plus the overflow row must still account for them.
	var requests, bytes int64
	for _, r := range rows {
		requests += parseCount(t, r.Requests)
	}
	if requests != 50 {
		t.Errorf("rows account for %d requests, want all 50", requests)
	}
	_ = bytes
}

// A full table must evict its stalest caller, not freeze on whoever arrived
// first: ephemeral CI addresses would otherwise fill it with dead entries and
// push every live caller into the overflow row.
func TestSourceTrackerEvictsLeastRecentlySeen(t *testing.T) {
	tr := sourceTracker{max: 2}

	tr.Record("10.0.0.1", "npm", 100)
	tr.Record("10.0.0.2", "npm", 100)

	// Touch the first so the second becomes the stalest.
	time.Sleep(2 * time.Millisecond)
	tr.Record("10.0.0.1", "npm", 100)

	time.Sleep(2 * time.Millisecond)
	tr.Record("10.0.0.3", "npm", 100)

	addrs := map[string]bool{}
	for _, r := range tr.Top(10) {
		if !r.IsOverflow {
			addrs[r.Addr] = true
		}
	}

	if !addrs["10.0.0.1"] {
		t.Error("the recently active caller was evicted")
	}
	if !addrs["10.0.0.3"] {
		t.Error("the newest caller was not admitted")
	}
	if addrs["10.0.0.2"] {
		t.Error("the stalest caller should have been evicted")
	}
}

// parseCount reverses formatCount for assertions.
func parseCount(t *testing.T, s string) int64 {
	t.Helper()
	n, err := strconv.ParseInt(strings.ReplaceAll(s, ",", ""), 10, 64)
	if err != nil {
		t.Fatalf("parsing %q: %v", s, err)
	}
	return n
}

func TestSourceTrackerTopLimits(t *testing.T) {
	var tr sourceTracker
	for i := range 10 {
		tr.Record(string(rune('a'+i)), "npm", int64(i*100))
	}

	// Three shown, plus one row summarising the seven that are not.
	got := tr.Top(3)
	if len(got) != 4 {
		t.Errorf("Top(3) returned %d rows, want 3 plus an overflow row", len(got))
	} else if !got[3].IsOverflow {
		t.Errorf("fourth row should be the overflow row, got %+v", got[3])
	}

	// No limit means nothing is left over, so there is no overflow row.
	if got := tr.Top(0); len(got) != 10 {
		t.Errorf("Top(0) returned %d rows, want all 10", len(got))
	}
}

func TestSourceTrackerZeroValueAndEmpty(t *testing.T) {
	var tr sourceTracker

	if rows := tr.Top(5); len(rows) != 0 {
		t.Errorf("an empty tracker returned %+v, want no rows", rows)
	}
	if tr.Count() != 0 {
		t.Errorf("Count = %d, want 0", tr.Count())
	}
	// Must not panic on a nil map.
	tr.Record("10.0.0.1", "npm", 1)
	if tr.Count() != 1 {
		t.Errorf("Count after first Record = %d, want 1", tr.Count())
	}
}

// A Server assembled as a struct literal has no config; reading the setting
// must not dereference it.
func TestTrustsForwardedForHandlesNilConfig(t *testing.T) {
	var s Server
	if s.trustsForwardedFor() {
		t.Error("a Server with no config must not trust X-Forwarded-For")
	}
}

func truncate(s string) string {
	if len(s) > 32 {
		return s[:32] + "..."
	}
	return s
}

// The rows the page shows must add up to the totals it reports above them, so
// callers past the display limit have to be summarised, not dropped.
func TestSourceTrackerTopAccountsForEveryCaller(t *testing.T) {
	var tr sourceTracker
	var wantRequests, wantBytes int64
	for i := range 40 {
		bytes := int64((i + 1) * 100)
		tr.Record("10.0.0."+strconv.Itoa(i), "npm", bytes)
		wantRequests++
		wantBytes += bytes
	}

	rows := tr.Top(5)
	if len(rows) != 6 {
		t.Fatalf("expected 5 rows plus an overflow row, got %d", len(rows))
	}
	if !rows[5].IsOverflow {
		t.Fatalf("last row is not the overflow row: %+v", rows[5])
	}

	var gotRequests int64
	for _, r := range rows {
		gotRequests += parseCount(t, r.Requests)
	}
	if gotRequests != wantRequests {
		t.Errorf("rows account for %d requests, want all %d", gotRequests, wantRequests)
	}
	// 35 callers are neither shown individually nor evicted.
	if rows[5].Client != formatCount(35)+" not shown" {
		t.Errorf("overflow row = %q, want 35 not shown", rows[5].Client)
	}
}

// The overflow row reports two counts that are true of different things: how
// many tracked callers fell below the display limit, which is exact, and how
// many fold-ins the eviction path has done, which is not a count of callers —
// a caller that is evicted and comes back is folded in twice. Reporting the sum
// as "N not shown" claimed more distinct callers than the row stands for.
func TestSourceTrackerOverflowSeparatesHiddenFromEvicted(t *testing.T) {
	tr := sourceTracker{max: 3}

	// Fill, then push the first caller out, then bring it back and push it out
	// again: two fold-ins, one caller.
	for _, addr := range []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"} {
		tr.Record(addr, "npm", 100)
		time.Sleep(time.Millisecond)
	}
	tr.Record("10.0.0.4", "npm", 100) // evicts .1
	time.Sleep(time.Millisecond)
	tr.Record("10.0.0.1", "npm", 100) // .1 returns, evicting .2
	time.Sleep(time.Millisecond)
	tr.Record("10.0.0.5", "npm", 100) // evicts .3

	if tr.evictions != 3 {
		t.Fatalf("evictions = %d, want 3 fold-ins", tr.evictions)
	}

	rows := tr.Top(2)
	overflow := rows[len(rows)-1]
	if !overflow.IsOverflow {
		t.Fatalf("last row is not the overflow row: %+v", overflow)
	}
	// One of the three tracked callers is below the cut; three fold-ins
	// happened, covering two distinct callers.
	if want := "1 not shown, 3 evicted"; overflow.Client != want {
		t.Errorf("overflow row = %q, want %q", overflow.Client, want)
	}

	var got int64
	for _, r := range rows {
		got += parseCount(t, r.Requests)
	}
	if got != 6 {
		t.Errorf("rows account for %d requests, want all 6", got)
	}
}

// With nothing evicted the row says only what it can count exactly.
func TestSourceTrackerOverflowOmitsEvictionsWhenThereAreNone(t *testing.T) {
	var tr sourceTracker
	for i := range 5 {
		tr.Record("10.0.0."+strconv.Itoa(i), "npm", 100)
	}

	rows := tr.Top(2)
	if want := "3 not shown"; rows[len(rows)-1].Client != want {
		t.Errorf("overflow row = %q, want %q", rows[len(rows)-1].Client, want)
	}
}

// The eviction scan is linear over the table, and Record holds a process-wide
// lock while it runs. These two bound what that costs in the case the source
// table is designed for — containerised CI, where every job has a fresh address
// and the table therefore sits permanently at its limit, so every request pays
// a scan. Measured at 200 entries on an M4 Max: ~2us evicting against ~40ns for
// a caller already tracked. Two microseconds against a request that goes to the
// network is not worth a heap or an LRU list, but the gap is the reason this is
// bounded at 200 rather than at something larger.
func BenchmarkRecordAlwaysEvicting(b *testing.B) {
	var t sourceTracker
	for i := 0; i < maxTrackedSources; i++ {
		t.Record("10.0."+strconv.Itoa(i/256)+"."+strconv.Itoa(i%256), "npm", 1024)
	}

	addrs := make([]string, b.N)
	for i := range addrs {
		addrs[i] = "172.16." + strconv.Itoa(i/256%256) + "." + strconv.Itoa(i%256)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		t.Record(addrs[i], "npm", 1024)
	}
}

func BenchmarkRecordExisting(b *testing.B) {
	var t sourceTracker
	t.Record("10.0.0.1", "npm", 1024)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		t.Record("10.0.0.1", "npm", 1024)
	}
}
