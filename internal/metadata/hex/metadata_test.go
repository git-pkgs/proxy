package hex

import (
	"bytes"
	"compress/gzip"
	"io"
	"slices"
	"testing"
	"time"

	"github.com/git-pkgs/cooldown"
	"google.golang.org/protobuf/encoding/protowire"
)

func release(version string, extra ...byte) []byte {
	var r []byte
	r = protowire.AppendTag(r, 1, protowire.BytesType)
	r = protowire.AppendString(r, version)
	return append(r, extra...)
}

// pkg builds a Package message for phoenix: the releases, then name and repository,
// with a varint field 4 to check other fields pass through.
func pkg(versions ...string) []byte {
	var p []byte
	for _, v := range versions {
		// A Release with a checksum (field 2) after its version.
		var checksum []byte
		checksum = protowire.AppendTag(checksum, 2, protowire.BytesType)
		checksum = protowire.AppendBytes(checksum, []byte{0xde, 0xad})
		p = protowire.AppendTag(p, 1, protowire.BytesType)
		p = protowire.AppendBytes(p, release(v, checksum...))
	}
	p = protowire.AppendTag(p, 2, protowire.BytesType)
	p = protowire.AppendString(p, "phoenix")
	p = protowire.AppendTag(p, 3, protowire.BytesType)
	p = protowire.AppendString(p, "hexpm")
	p = protowire.AppendTag(p, 4, protowire.VarintType)
	p = protowire.AppendVarint(p, 7)
	return p
}

func signed(payload, signature []byte) []byte {
	var s []byte
	s = protowire.AppendTag(s, 1, protowire.BytesType)
	s = protowire.AppendBytes(s, payload)
	if signature != nil {
		s = protowire.AppendTag(s, 2, protowire.BytesType)
		s = protowire.AppendBytes(s, signature)
	}
	return s
}

func gunzip(t *testing.T, data []byte) []byte {
	t.Helper()
	gr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	out, err := io.ReadAll(gr)
	if err != nil {
		t.Fatalf("gunzip: %v", err)
	}
	return out
}

func TestReleaseVersions(t *testing.T) {
	got, err := ReleaseVersions(pkg("1.0.0", "2.0.0-rc.1", "2.0.0"))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"1.0.0", "2.0.0-rc.1", "2.0.0"}; !slices.Equal(got, want) {
		t.Errorf("ReleaseVersions = %v, want %v", got, want)
	}
}

func TestFilterReleasesKeepsOtherFields(t *testing.T) {
	var seen []string
	got, err := FilterReleases(pkg("1.0.0", "2.0.0", "3.0.0"),
		map[string]bool{"2.0.0": true}, func(v string) { seen = append(seen, v) })
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, pkg("1.0.0", "3.0.0")) {
		t.Errorf("FilterReleases left %x, want the package without 2.0.0", got)
	}
	if want := []string{"1.0.0", "2.0.0", "3.0.0"}; !slices.Equal(seen, want) {
		t.Errorf("seen = %v, want %v", seen, want)
	}
}

func TestFilterReleasesWithNothingWithheld(t *testing.T) {
	in := pkg("1.0.0", "2.0.0")
	got, err := FilterReleases(in, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, in) {
		t.Errorf("FilterReleases changed a package with nothing withheld")
	}
}

func TestFilterReleasesRejectsMalformedInput(t *testing.T) {
	var fixed32 []byte
	fixed32 = protowire.AppendTag(fixed32, 5, protowire.Fixed32Type)
	fixed32 = protowire.AppendFixed32(fixed32, 1)
	var truncatedBytes []byte
	truncatedBytes = protowire.AppendTag(truncatedBytes, 1, protowire.BytesType)
	truncatedBytes = append(truncatedBytes, 10, 'x')
	var truncatedVarint []byte
	truncatedVarint = protowire.AppendTag(truncatedVarint, 4, protowire.VarintType)
	truncatedVarint = append(truncatedVarint, 0x80)

	for name, in := range map[string][]byte{
		"bad tag":          {0x80},
		"fixed32 field":    fixed32,
		"truncated bytes":  truncatedBytes,
		"truncated varint": truncatedVarint,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := FilterReleases(in, nil, nil); err == nil {
				t.Error("FilterReleases accepted malformed input")
			}
		})
	}
}

func TestReleaseVersion(t *testing.T) {
	var varintFirst []byte
	varintFirst = protowire.AppendTag(varintFirst, 3, protowire.VarintType)
	varintFirst = protowire.AppendVarint(varintFirst, 1)
	varintFirst = append(varintFirst, release("1.2.3")...)

	var otherBytesFirst []byte
	otherBytesFirst = protowire.AppendTag(otherBytesFirst, 2, protowire.BytesType)
	otherBytesFirst = protowire.AppendBytes(otherBytesFirst, []byte("sum"))
	otherBytesFirst = append(otherBytesFirst, release("4.5.6")...)

	var fixed64 []byte
	fixed64 = protowire.AppendTag(fixed64, 3, protowire.Fixed64Type)
	fixed64 = protowire.AppendFixed64(fixed64, 1)
	fixed64 = append(fixed64, release("7.8.9")...)

	var truncatedVarint []byte
	truncatedVarint = protowire.AppendTag(truncatedVarint, 3, protowire.VarintType)
	truncatedVarint = append(truncatedVarint, 0x80)

	var noVersion []byte
	noVersion = protowire.AppendTag(noVersion, 2, protowire.BytesType)
	noVersion = protowire.AppendBytes(noVersion, []byte("sum"))

	for name, tt := range map[string]struct {
		in   []byte
		want string
	}{
		"version":             {release("1.0.0"), "1.0.0"},
		"after a varint":      {varintFirst, "1.2.3"},
		"after another field": {otherBytesFirst, "4.5.6"},
		"unknown wire type":   {fixed64, ""},
		"bad tag":             {[]byte{0x80}, ""},
		"truncated bytes":     {[]byte{0x0a, 5, 'x'}, ""},
		"truncated varint":    {truncatedVarint, ""},
		"no version":          {noVersion, ""},
		"empty":               {nil, ""},
	} {
		t.Run(name, func(t *testing.T) {
			if got := ReleaseVersion(tt.in); got != tt.want {
				t.Errorf("ReleaseVersion = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBytesField(t *testing.T) {
	var msg []byte
	msg = protowire.AppendTag(msg, 3, protowire.VarintType)
	msg = protowire.AppendVarint(msg, 9)
	msg = protowire.AppendTag(msg, 1, protowire.BytesType)
	msg = protowire.AppendBytes(msg, []byte("one"))
	msg = protowire.AppendTag(msg, 2, protowire.BytesType)
	msg = protowire.AppendBytes(msg, []byte("two"))

	for field, want := range map[protowire.Number]string{1: "one", 2: "two"} {
		got, err := BytesField(msg, field)
		if err != nil || string(got) != want {
			t.Errorf("BytesField(%d) = %q, %v; want %q", field, got, err, want)
		}
	}
	if _, err := BytesField(msg, 5); err == nil {
		t.Error("BytesField found a field the message doesn't have")
	}

	var fixed32 []byte
	fixed32 = protowire.AppendTag(fixed32, 4, protowire.Fixed32Type)
	fixed32 = protowire.AppendFixed32(fixed32, 1)
	var truncatedVarint []byte
	truncatedVarint = protowire.AppendTag(truncatedVarint, 3, protowire.VarintType)
	truncatedVarint = append(truncatedVarint, 0x80)
	for name, in := range map[string][]byte{
		"bad tag":          {0x80},
		"truncated bytes":  {0x0a, 5, 'x'},
		"truncated varint": truncatedVarint,
		"fixed32 field":    fixed32,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := BytesField(in, 1); err == nil {
				t.Error("BytesField accepted malformed input")
			}
		})
	}
}

func TestPayload(t *testing.T) {
	payload := pkg("1.0.0")
	got, err := Payload(signed(payload, []byte("sig")))
	if err != nil || !bytes.Equal(got, payload) {
		t.Errorf("Payload = %x, %v; want the payload", got, err)
	}
	var onlySignature []byte
	onlySignature = protowire.AppendTag(onlySignature, 2, protowire.BytesType)
	onlySignature = protowire.AppendBytes(onlySignature, []byte("sig"))
	if _, err := Payload(onlySignature); err == nil {
		t.Error("Payload accepted a Signed message without one")
	}
}

func TestFilterSignedDropsReleasesAndSignature(t *testing.T) {
	out, err := FilterSigned(signed(pkg("1.0.0", "2.0.0"), []byte("sig")), map[string]bool{"2.0.0": true})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := gunzip(t, out), signed(pkg("1.0.0"), nil); !bytes.Equal(got, want) {
		t.Errorf("FilterSigned = %x, want %x", got, want)
	}
}

// windowPolicy holds a version back until window has passed since it was
// published, as judged at the evaluation time it is given.
type windowPolicy struct {
	disabled  bool
	window    time.Duration
	evaluated []time.Time
	purls     []string
}

func (p *windowPolicy) Enabled() bool { return !p.disabled }

func (p *windowPolicy) Evaluate(ecosystem, packagePURL string, publishedAt, evaluatedAt time.Time) cooldown.Decision {
	p.evaluated = append(p.evaluated, evaluatedAt)
	p.purls = append(p.purls, ecosystem+" "+packagePURL)
	availableAt := publishedAt.Add(p.window)
	return cooldown.Decision{Allowed: !evaluatedAt.Before(availableAt), AvailableAt: availableAt}
}

var now = time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

// published dates 1.0.0 and 3.0.0 a month ago and 2.0.0 an hour ago.
func published() map[string]time.Time {
	old := now.AddDate(0, -1, 0)
	return map[string]time.Time{"1.0.0": old, "2.0.0": now.Add(-time.Hour), "3.0.0": old}
}

type withheldVersion struct {
	name, version string
	publishedAt   time.Time
}

func TestFilterPackage(t *testing.T) {
	policy := &windowPolicy{window: 72 * time.Hour}
	var reported []withheldVersion
	out, kept, err := FilterPackage("phoenix", signed(pkg("1.0.0", "2.0.0", "3.0.0"), []byte("sig")), Options{
		Cooldown:    policy,
		EvaluatedAt: now,
		Published:   published(),
		OnWithheld: func(name, version string, publishedAt time.Time) {
			reported = append(reported, withheldVersion{name, version, publishedAt})
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := gunzip(t, out), signed(pkg("1.0.0", "3.0.0"), nil); !bytes.Equal(got, want) {
		t.Errorf("FilterPackage = %x, want %x: without 2.0.0 and the signature", got, want)
	}
	if want := []string{"1.0.0", "3.0.0"}; !slices.Equal(kept, want) {
		t.Errorf("kept = %q, want %q", kept, want)
	}
	if want := []withheldVersion{{"phoenix", "2.0.0", now.Add(-time.Hour)}}; !slices.Equal(reported, want) {
		t.Errorf("OnWithheld got %v, want %v", reported, want)
	}
	if len(policy.purls) != 3 || slices.ContainsFunc(policy.purls, func(p string) bool { return p != "hex pkg:hex/phoenix" }) {
		t.Errorf("policy asked about %q, want hex pkg:hex/phoenix for each of the 3 releases", policy.purls)
	}
	if slices.ContainsFunc(policy.evaluated, func(at time.Time) bool { return !at.Equal(now) }) {
		t.Errorf("policy evaluated at %v, want only %v", policy.evaluated, now)
	}
}

// With nothing held back there is no output: upstream's bytes are the ones
// to serve, as their signature still holds.
func TestFilterPackageWithholdsNothing(t *testing.T) {
	tests := map[string]Options{
		"no policy":       {Published: published(), WithholdUnknown: true},
		"policy disabled": {Cooldown: &windowPolicy{disabled: true, window: 72 * time.Hour}, EvaluatedAt: now, WithholdUnknown: true},
		"all old enough":  {Cooldown: &windowPolicy{window: 30 * time.Minute}, EvaluatedAt: now, Published: published()},
	}
	for name, o := range tests {
		t.Run(name, func(t *testing.T) {
			o.OnWithheld = func(_, version string, _ time.Time) { t.Errorf("withheld %s", version) }
			out, kept, err := FilterPackage("phoenix", signed(pkg("1.0.0", "2.0.0", "3.0.0"), []byte("sig")), o)
			if err != nil {
				t.Fatal(err)
			}
			if out != nil {
				t.Errorf("FilterPackage = %x, want no output", out)
			}
			if want := []string{"1.0.0", "2.0.0", "3.0.0"}; !slices.Equal(kept, want) {
				t.Errorf("kept = %q, want %q", kept, want)
			}
		})
	}
}

// A release the times don't have is kept, unless the caller says its times
// are too old to trust: then it is held back, with no publish time to
// report.
func TestFilterPackageReleaseWithoutTime(t *testing.T) {
	times := published()
	delete(times, "3.0.0")
	in := signed(pkg("1.0.0", "2.0.0", "3.0.0"), []byte("sig"))
	o := Options{Cooldown: &windowPolicy{window: 30 * time.Minute}, EvaluatedAt: now, Published: times}

	if out, kept, err := FilterPackage("phoenix", in, o); err != nil || out != nil || len(kept) != 3 {
		t.Errorf("FilterPackage = %x, %q, %v; want all 3 releases kept", out, kept, err)
	}

	var reported []withheldVersion
	o.WithholdUnknown = true
	o.OnWithheld = func(name, version string, publishedAt time.Time) {
		reported = append(reported, withheldVersion{name, version, publishedAt})
	}
	out, kept, err := FilterPackage("phoenix", in, o)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := gunzip(t, out), signed(pkg("1.0.0", "2.0.0"), nil); !bytes.Equal(got, want) {
		t.Errorf("FilterPackage = %x, want %x: without 3.0.0", got, want)
	}
	if want := []string{"1.0.0", "2.0.0"}; !slices.Equal(kept, want) {
		t.Errorf("kept = %q, want %q", kept, want)
	}
	if want := []withheldVersion{{"phoenix", "3.0.0", time.Time{}}}; !slices.Equal(reported, want) {
		t.Errorf("OnWithheld got %v, want %v", reported, want)
	}
}

// A release whose time didn't parse is kept, whatever WithholdUnknown says.
func TestFilterPackageKeepsReleaseWithZeroTime(t *testing.T) {
	times := published()
	times["2.0.0"] = time.Time{}
	policy := &windowPolicy{window: 72 * time.Hour}
	out, kept, err := FilterPackage("phoenix", signed(pkg("1.0.0", "2.0.0", "3.0.0"), nil), Options{
		Cooldown: policy, EvaluatedAt: now, Published: times, WithholdUnknown: true,
	})
	if err != nil || out != nil || len(kept) != 3 {
		t.Errorf("FilterPackage = %x, %q, %v; want all 3 releases kept", out, kept, err)
	}
	if len(policy.evaluated) != 2 {
		t.Errorf("policy evaluated %d releases, want 2: not the one without a time", len(policy.evaluated))
	}
}

func TestFilterPackageRejectsMalformedInput(t *testing.T) {
	o := Options{Cooldown: &windowPolicy{window: 72 * time.Hour}, EvaluatedAt: now, Published: published()}
	if _, _, err := FilterPackage("phoenix", []byte{0x80}, o); err == nil {
		t.Error("FilterPackage accepted a malformed Signed message")
	}
	var fixed32 []byte
	fixed32 = protowire.AppendTag(fixed32, 5, protowire.Fixed32Type)
	fixed32 = protowire.AppendFixed32(fixed32, 1)
	if _, _, err := FilterPackage("phoenix", signed(fixed32, nil), o); err == nil {
		t.Error("FilterPackage accepted a malformed payload")
	}
}

func TestFilterSignedRejectsMalformedInput(t *testing.T) {
	if _, err := FilterSigned([]byte{0x80}, nil); err == nil {
		t.Error("FilterSigned accepted a malformed Signed message")
	}
	var fixed32 []byte
	fixed32 = protowire.AppendTag(fixed32, 5, protowire.Fixed32Type)
	fixed32 = protowire.AppendFixed32(fixed32, 1)
	if _, err := FilterSigned(signed(fixed32, nil), nil); err == nil {
		t.Error("FilterSigned accepted a malformed payload")
	}
}

func TestUnsignedPackage(t *testing.T) {
	payload := pkg("1.0.0")
	out, err := UnsignedPackage(payload)
	if err != nil {
		t.Fatal(err)
	}
	if got := gunzip(t, out); !bytes.Equal(got, signed(payload, nil)) {
		t.Errorf("UnsignedPackage = %x, want the payload in a Signed message", got)
	}
}

func TestReleaseTimes(t *testing.T) {
	times, err := ReleaseTimes([]byte(`{"releases":[
		{"version":"1.0.0","inserted_at":"2024-01-02T03:04:05.123456Z"},
		{"version":"2.0.0","inserted_at":"not a time"},
		{"version":"3.0.0"}
	]}`))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]time.Time{
		"1.0.0": time.Date(2024, 1, 2, 3, 4, 5, 123456000, time.UTC),
		"2.0.0": {},
		"3.0.0": {},
	}
	if len(times) != len(want) {
		t.Fatalf("ReleaseTimes = %v, want %v", times, want)
	}
	for version, at := range want {
		if got, ok := times[version]; !ok || !got.Equal(at) {
			t.Errorf("ReleaseTimes[%s] = %v, %v; want %v", version, got, ok, at)
		}
	}
	if _, err := ReleaseTimes([]byte("{")); err == nil {
		t.Error("ReleaseTimes accepted invalid JSON")
	}
}

func TestTarballIdentity(t *testing.T) {
	for filename, want := range map[string][2]string{
		"phoenix-1.7.10.tar":      {"phoenix", "1.7.10"},
		"amqp_client-4.1.5-1.tar": {"amqp_client", "4.1.5-1"},
		"phoenix.tar":             {"", ""},
		"-1.0.0.tar":              {"", ""},
		"phoenix-.tar":            {"", ""},
	} {
		name, version := TarballIdentity(filename)
		if name != want[0] || version != want[1] {
			t.Errorf("TarballIdentity(%q) = (%q, %q), want (%q, %q)", filename, name, version, want[0], want[1])
		}
	}
}
