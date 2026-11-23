package hex

import (
	"bytes"
	"compress/gzip"
	"io"
	"slices"
	"testing"
	"time"

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
