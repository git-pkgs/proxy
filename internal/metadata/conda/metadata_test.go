package conda

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/git-pkgs/cooldown"
	"github.com/vmihailenco/msgpack/v5"
)

var evaluatedAt = time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

// policy holds back what was published within age of the evaluation time.
type policy struct {
	disabled bool
	age      time.Duration
	// noAvailableAt leaves Decision.AvailableAt zero.
	noAvailableAt bool
	purls         []string
	at            []time.Time
}

func (p *policy) Enabled() bool { return !p.disabled }

func (p *policy) Evaluate(eco, packagePURL string, publishedAt, at time.Time) cooldown.Decision {
	if eco != "conda" {
		panic("ecosystem " + eco)
	}
	p.purls = append(p.purls, packagePURL)
	p.at = append(p.at, at)
	available := publishedAt.Add(p.age)
	d := cooldown.Decision{Allowed: !at.Before(available)}
	if !p.noAvailableAt {
		d.AvailableAt = available
	}
	return d
}

func TestTime(t *testing.T) {
	cases := []struct {
		in   float64
		want time.Time
	}{
		{1_700_000_000, time.Unix(1_700_000_000, 0)},
		{1_700_000_000_123, time.Unix(1_700_000_000, 0)},
		{MaxSecondsTimestamp, time.Unix(MaxSecondsTimestamp, 0)},
		{MaxSecondsTimestamp + 1, time.UnixMilli(MaxSecondsTimestamp + 1).Truncate(time.Second)},
	}
	for _, c := range cases {
		if got := Time(c.in); !got.Equal(c.want) {
			t.Errorf("Time(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestFilenameIdentity(t *testing.T) {
	cases := []struct{ in, name, version string }{
		{"numpy-1.24.0-py311h64a7726_0.conda", "numpy", "1.24.0"},
		{"python-dateutil-2.8.2-pyhd8ed1ab_0.tar.bz2", "python-dateutil", "2.8.2"},
		{"ca-certificates-2024.2.2-hbcca054_0.conda", "ca-certificates", "2024.2.2"},
		{"numpy-1.24.0.conda", "", ""},
		{"numpy-v1-0.conda", "", ""},
		{"numpy--0.conda", "", ""},
		{"numpy-1.0-0.conda.tar.bz2", "numpy", "1.0"},
		{"1.0-0.conda", "", ""},
	}
	for _, c := range cases {
		name, version := FilenameIdentity(c.in)
		if name != c.name || version != c.version {
			t.Errorf("FilenameIdentity(%q) = %q, %q, want %q, %q", c.in, name, version, c.name, c.version)
		}
	}
}

func repodata(t *testing.T) string {
	t.Helper()
	old := evaluatedAt.Add(-30 * 24 * time.Hour)
	recent := evaluatedAt.Add(-time.Hour)
	doc := `{"info":{"subdir":"noarch"},"packages":{` +
		`"old-1.0-0.tar.bz2":{"name":"old","version":"1.0","timestamp":` + ms(old) + `},` +
		`"new-2.0-0.tar.bz2":{"name":"new","version":"2.0","timestamp":` + ms(recent) + `},` +
		`"untimed-1.0-0.tar.bz2":{"name":"untimed","version":"1.0"},` +
		`"nameless-1.0-0.tar.bz2":{"version":"1.0","timestamp":` + ms(recent) + `}` +
		`},"packages.conda":{` +
		`"newer-3.0-0.conda":{"name":"newer","version":"3.0","timestamp":` + secs(evaluatedAt.Add(-2*time.Hour)) + `}` +
		`},"removed":["x-1-0.tar.bz2"]}`
	return doc
}

func ms(t time.Time) string   { b, _ := json.Marshal(t.UnixMilli()); return string(b) }
func secs(t time.Time) string { b, _ := json.Marshal(t.Unix()); return string(b) }

func TestFilterLeavesOutEntriesInCooldown(t *testing.T) {
	p := &policy{age: 24 * time.Hour}
	var filtered []string
	f := NewFilter(Options{Cooldown: p, EvaluatedAt: evaluatedAt, OnCooldownFilter: func(filename, name, version string) {
		filtered = append(filtered, filename+" "+name+" "+version)
	}})
	var out bytes.Buffer
	if err := f.Copy(strings.NewReader(repodata(t)), &out); err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Info          json.RawMessage            `json:"info"`
		Packages      map[string]json.RawMessage `json:"packages"`
		PackagesConda map[string]json.RawMessage `json:"packages.conda"`
		Removed       []string                   `json:"removed"`
	}
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out.String())
	}
	for _, kept := range []string{"old-1.0-0.tar.bz2", "untimed-1.0-0.tar.bz2", "nameless-1.0-0.tar.bz2"} {
		if _, ok := doc.Packages[kept]; !ok {
			t.Errorf("%s left out", kept)
		}
	}
	if _, ok := doc.Packages["new-2.0-0.tar.bz2"]; ok {
		t.Error("new-2.0-0.tar.bz2 kept")
	}
	if len(doc.PackagesConda) != 0 {
		t.Errorf("packages.conda = %v, want empty", doc.PackagesConda)
	}
	if string(doc.Info) != `{"subdir":"noarch"}` || len(doc.Removed) != 1 {
		t.Errorf("other members changed: info %s removed %v", doc.Info, doc.Removed)
	}
	want := []string{"new-2.0-0.tar.bz2 new 2.0", "newer-3.0-0.conda newer 3.0"}
	if strings.Join(filtered, "|") != strings.Join(want, "|") {
		t.Errorf("filtered %v, want %v", filtered, want)
	}
	for _, at := range p.at {
		if !at.Equal(evaluatedAt) {
			t.Errorf("evaluated at %v, want %v", at, evaluatedAt)
		}
	}
	if p.purls[0] != "pkg:conda/old" {
		t.Errorf("purl %q", p.purls[0])
	}
	until, known := f.Until()
	if !known || !until.Equal(evaluatedAt.Add(-2*time.Hour).Add(24*time.Hour)) {
		t.Errorf("Until = %v, %v, want the earliest entry's", until, known)
	}
}

func TestFilterKeepsUpstreamBytes(t *testing.T) {
	in := `{"packages":{"a-1-0.tar.bz2":{"name":"a", "timestamp":1, "depends":["b >=1"]}},"x": [1, 2]}`
	var out bytes.Buffer
	if err := NewFilter(Options{Cooldown: &policy{}, EvaluatedAt: evaluatedAt}).Copy(strings.NewReader(in), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `{"name":"a", "timestamp":1, "depends":["b >=1"]}`) || !strings.Contains(out.String(), `[1, 2]`) {
		t.Errorf("bytes changed: %s", out.String())
	}
}

func TestFilterWithheldDigest(t *testing.T) {
	run := func(age time.Duration) *Filter {
		f := NewFilter(Options{Cooldown: &policy{age: age}, EvaluatedAt: evaluatedAt})
		if err := f.Copy(strings.NewReader(repodata(t)), &bytes.Buffer{}); err != nil {
			t.Fatal(err)
		}
		return f
	}
	if !bytes.Equal(run(24*time.Hour).Withheld(), run(24*time.Hour).Withheld()) {
		t.Error("same entries left out give different digests")
	}
	if bytes.Equal(run(24*time.Hour).Withheld(), run(90*time.Minute).Withheld()) {
		t.Error("different entries left out give the same digest")
	}
	none := run(0)
	if until, known := none.Until(); !until.IsZero() || !known {
		t.Errorf("nothing left out: Until = %v, %v", until, known)
	}
}

func TestFilterKeepsEntriesWithoutTimestamp(t *testing.T) {
	// Everything published so far is held back, but entries without a
	// timestamp or a name have nothing to decide by.
	f := NewFilter(Options{Cooldown: &policy{age: 100 * 365 * 24 * time.Hour}, EvaluatedAt: evaluatedAt})
	var out bytes.Buffer
	if err := f.Copy(strings.NewReader(repodata(t)), &out); err != nil {
		t.Fatal(err)
	}
	for _, kept := range []string{"untimed-1.0-0.tar.bz2", "nameless-1.0-0.tar.bz2"} {
		if !strings.Contains(out.String(), kept) {
			t.Errorf("%s left out", kept)
		}
	}
	if strings.Contains(out.String(), "old-1.0-0.tar.bz2") {
		t.Error("old-1.0-0.tar.bz2 kept")
	}
}

func TestFilterUntilUnknown(t *testing.T) {
	f := NewFilter(Options{Cooldown: &policy{age: 24 * time.Hour, noAvailableAt: true}, EvaluatedAt: evaluatedAt})
	if err := f.Copy(strings.NewReader(repodata(t)), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if _, known := f.Until(); known {
		t.Error("Until known without an AvailableAt")
	}
}

func TestFilterWithoutPolicy(t *testing.T) {
	for name, p := range map[string]CooldownPolicy{"nil": nil, "disabled": &policy{disabled: true, age: 24 * time.Hour}} {
		var out bytes.Buffer
		if err := NewFilter(Options{Cooldown: p, EvaluatedAt: evaluatedAt}).Copy(strings.NewReader(repodata(t)), &out); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), "new-2.0-0.tar.bz2") {
			t.Errorf("%s policy left entries out", name)
		}
	}
}

func TestFilterRefusesInvalidDocument(t *testing.T) {
	if err := NewFilter(Options{}).Copy(strings.NewReader(`{"packages":`), &bytes.Buffer{}); err == nil {
		t.Error("truncated document copied")
	}
}

func TestReadIndex(t *testing.T) {
	index, err := ReadIndex(strings.NewReader(repodata(t) + ``))
	if err != nil {
		t.Fatal(err)
	}
	if got := index["old-1.0-0.tar.bz2"]; !got.Equal(evaluatedAt.Add(-30 * 24 * time.Hour).Truncate(time.Second)) {
		t.Errorf("old = %v", got)
	}
	if got := index["newer-3.0-0.conda"]; !got.Equal(evaluatedAt.Add(-2 * time.Hour)) {
		t.Errorf("newer = %v", got)
	}
	if got, ok := index["untimed-1.0-0.tar.bz2"]; !ok || !got.IsZero() {
		t.Errorf("untimed = %v, %v", got, ok)
	}
	if len(index) != 5 {
		t.Errorf("index has %d files, want 5", len(index))
	}
	index, err = ReadIndex(strings.NewReader(`{"packages":{"bad":[1],"good-1-0.conda":{"timestamp":1}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := index["bad"]; ok || len(index) != 1 {
		t.Errorf("index = %v, want only the object entry", index)
	}
	if _, err := ReadIndex(strings.NewReader(`[`)); err == nil {
		t.Error("invalid document read")
	}
}

func TestIsShardFile(t *testing.T) {
	hash := strings.Repeat("ab", ShardHashLen)
	cases := map[string]bool{
		hash + ShardSuffix:                     true,
		strings.ToUpper(hash) + ShardSuffix:    false,
		hash[2:] + ShardSuffix:                 false,
		hash + ".msgpack":                      false,
		strings.Repeat("zz", 32) + ShardSuffix: false,
	}
	for name, want := range cases {
		if got := IsShardFile(name); got != want {
			t.Errorf("IsShardFile(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestShardMask(t *testing.T) {
	a := ShardMask("conda-forge", "noarch", "1")
	if a != ShardMask("conda-forge", "noarch", "1") {
		t.Error("mask not stable")
	}
	for _, other := range [][ShardHashLen]byte{ShardMask("conda-forge", "noarch", "2"), ShardMask("conda-forge", "linux-64", "1"), ShardMask("bioconda", "noarch", "1")} {
		if a == other {
			t.Error("masks collide")
		}
	}
}

func mustMsgpack(t *testing.T, v any) []byte {
	t.Helper()
	b, err := msgpack.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func shardIndex(t *testing.T, info map[string]any, shards map[string]any) []byte {
	t.Helper()
	top := map[string]any{"version": 1, "info": info}
	if shards != nil {
		top["shards"] = shards
	}
	return mustMsgpack(t, top)
}

const indexURL = "https://proxy.example/conda/conda-forge/noarch/repodata_shards.msgpack.zst"

func TestDecodeShardIndexRefusesOtherDocuments(t *testing.T) {
	cases := map[string][]byte{
		"no version":     mustMsgpack(t, map[string]any{"info": map[string]any{}}),
		"version 2":      mustMsgpack(t, map[string]any{"version": 2, "info": map[string]any{}}),
		"no info":        mustMsgpack(t, map[string]any{"version": 1}),
		"info not map":   mustMsgpack(t, map[string]any{"version": 1, "info": "x"}),
		"not a map":      mustMsgpack(t, []int{1}),
		"non-string key": mustMsgpack(t, map[int]int{1: 1}),
	}
	for name, raw := range cases {
		if _, err := DecodeShardIndex(raw); err == nil {
			t.Errorf("%s: decoded", name)
		}
	}
}

func TestShardsLayout(t *testing.T) {
	cases := []struct {
		info map[string]any
		want string
		ok   bool
	}{
		{map[string]any{}, "", true},
		{map[string]any{"base_url": "./", "shards_base_url": "./shards/"}, ShardsSubdir, true},
		{map[string]any{"shards_base_url": "shards/"}, ShardsSubdir, true},
		{map[string]any{"base_url": "https://conda.anaconda.org/conda-forge/noarch/"}, "", false},
		{map[string]any{"base_url": "/conda-forge/noarch/"}, "", false},
		{map[string]any{"base_url": "/conda/conda-forge/noarch/"}, "", false},
		{map[string]any{"base_url": "../"}, "", false},
		{map[string]any{"shards_base_url": "other/"}, "", false},
		{map[string]any{"shards_base_url": "//cdn.example/x/"}, "", false},
		{map[string]any{"shards_base_url": "../"}, "", false},
		{map[string]any{"base_url": 1}, "", false},
		{map[string]any{"shards_base_url": 1}, "", false},
	}
	for _, c := range cases {
		idx, err := DecodeShardIndex(shardIndex(t, c.info, nil))
		if err != nil {
			t.Fatal(err)
		}
		got, err := idx.ShardsLayout(indexURL)
		if (err == nil) != c.ok || got != c.want {
			t.Errorf("info %v: layout %q, %v; want %q, ok %v", c.info, got, err, c.want, c.ok)
		}
		if err != nil && c.info["base_url"] != 1 && c.info["shards_base_url"] != 1 && !errors.Is(err, ErrShardLayout) {
			t.Errorf("info %v: error %v is not ErrShardLayout", c.info, err)
		}
	}
	idx, _ := DecodeShardIndex(shardIndex(t, map[string]any{}, nil))
	if _, err := idx.ShardsLayout("://bad"); err == nil {
		t.Error("bad index URL accepted")
	}
}

func TestRewriteShardIndex(t *testing.T) {
	hash := bytes.Repeat([]byte{0x11}, ShardHashLen)
	mask := ShardMask("conda-forge", "noarch", "7")
	for _, c := range []struct {
		info map[string]any
		base string
	}{
		{map[string]any{"base_url": "./", "subdir": "noarch"}, "./cooldown-shards/7/"},
		{map[string]any{"shards_base_url": "./shards/"}, "./cooldown-shards/7/shards/"},
	} {
		raw := shardIndex(t, c.info, map[string]any{"numpy": hash})
		out, err := RewriteShardIndex(raw, indexURL, mask, "7")
		if err != nil {
			t.Fatal(err)
		}
		var doc struct {
			Version int               `msgpack:"version"`
			Info    map[string]any    `msgpack:"info"`
			Shards  map[string][]byte `msgpack:"shards"`
		}
		if err := msgpack.Unmarshal(out, &doc); err != nil {
			t.Fatal(err)
		}
		if doc.Version != 1 || doc.Info["shards_base_url"] != c.base {
			t.Errorf("info %v: version %d, shards_base_url %v, want %q", c.info, doc.Version, doc.Info["shards_base_url"], c.base)
		}
		for k, v := range c.info {
			if k != "shards_base_url" && doc.Info[k] != v {
				t.Errorf("info %s = %v, want %v", k, doc.Info[k], v)
			}
		}
		want := make([]byte, ShardHashLen)
		for i := range want {
			want[i] = hash[i] ^ mask[i]
		}
		if !bytes.Equal(doc.Shards["numpy"], want) {
			t.Errorf("shard hash %x, want %x", doc.Shards["numpy"], want)
		}
	}
}

func TestRewriteShardIndexRefuses(t *testing.T) {
	mask := ShardMask("c", "a", "1")
	cases := map[string][]byte{
		"no shards":   shardIndex(t, map[string]any{}, nil),
		"short hash":  shardIndex(t, map[string]any{}, map[string]any{"x": []byte{1}}),
		"string hash": shardIndex(t, map[string]any{}, map[string]any{"x": "abc"}),
		"bad layout":  shardIndex(t, map[string]any{"base_url": "/x/"}, map[string]any{}),
		"not index":   mustMsgpack(t, []int{1}),
	}
	for name, raw := range cases {
		if _, err := RewriteShardIndex(raw, indexURL, mask, "1"); err == nil {
			t.Errorf("%s: rewritten", name)
		}
	}
}

func TestFilterShard(t *testing.T) {
	old := evaluatedAt.Add(-30 * 24 * time.Hour)
	recent := evaluatedAt.Add(-time.Hour)
	raw := mustMsgpack(t, map[string]any{
		"info": map[string]any{"subdir": "noarch"},
		"packages": map[string]any{
			"a-1.0-0.tar.bz2": map[string]any{"name": "a", "version": "1.0", "timestamp": old.UnixMilli()},
			"a-2.0-0.tar.bz2": map[string]any{"name": "a", "version": "2.0", "timestamp": recent.UnixMilli()},
			"a-3.0-0.tar.bz2": map[string]any{"name": "a", "version": "3.0", "timestamp": float64(recent.Unix())},
			"a-4.0-0.tar.bz2": map[string]any{"name": "a", "version": "4.0"},
		},
		"packages.conda": map[string]any{
			"a-5.0-0.conda": map[string]any{"name": "a", "version": "5.0", "timestamp": uint32(recent.Unix())},
		},
	})
	p := &policy{age: 24 * time.Hour}
	var filtered []string
	out, err := FilterShard(raw, Options{Cooldown: p, EvaluatedAt: evaluatedAt, OnCooldownFilter: func(filename, name, version string) {
		filtered = append(filtered, filename)
	}})
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Info          map[string]any            `msgpack:"info"`
		Packages      map[string]map[string]any `msgpack:"packages"`
		PackagesConda map[string]map[string]any `msgpack:"packages.conda"`
	}
	if err := msgpack.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Packages) != 2 || doc.Packages["a-1.0-0.tar.bz2"] == nil || doc.Packages["a-4.0-0.tar.bz2"] == nil {
		t.Errorf("packages = %v", doc.Packages)
	}
	if len(doc.PackagesConda) != 0 || doc.Info["subdir"] != "noarch" {
		t.Errorf("packages.conda = %v, info = %v", doc.PackagesConda, doc.Info)
	}
	if len(filtered) != 3 {
		t.Errorf("filtered %v, want 3 files", filtered)
	}
	for _, at := range p.at {
		if !at.Equal(evaluatedAt) {
			t.Errorf("evaluated at %v", at)
		}
	}
	same, err := FilterShard(raw, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := msgpack.Unmarshal(same, &doc); err != nil || len(doc.Packages) != 4 {
		t.Errorf("without a policy: %v, %v", doc.Packages, err)
	}
}

func TestFilterShardRefuses(t *testing.T) {
	cases := map[string][]byte{
		"not a map":        mustMsgpack(t, []int{1}),
		"packages not map": mustMsgpack(t, map[string]any{"packages": 1}),
		"record not map":   mustMsgpack(t, map[string]any{"packages": map[string]any{"a": 1}}),
	}
	for name, raw := range cases {
		if _, err := FilterShard(raw, Options{}); err == nil {
			t.Errorf("%s: filtered", name)
		}
	}
}

func TestMsgpackNumber(t *testing.T) {
	for _, v := range []any{int8(5), int16(5), int32(5), int64(5), uint8(5), uint16(5), uint32(5), uint64(5), float32(5), float64(5)} {
		if got := msgpackNumber(v); got != 5 {
			t.Errorf("msgpackNumber(%T) = %v", v, got)
		}
	}
	if got := msgpackNumber("5"); got != 0 {
		t.Errorf("msgpackNumber(string) = %v", got)
	}
}
