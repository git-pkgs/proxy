// Package hex holds the Hex registry's metadata transformations, without
// the proxy's fetching, caching or storage, so it builds on its own,
// including for targets such as WASI.
//
// A Hex package file (/packages/{name}) is a gzipped Signed message whose
// payload is a Package message:
//
//	Signed:  1 = payload (bytes), 2 = signature (bytes)
//	Package: 1 = releases (repeated Release), 2 = name, 3 = repository
//	Release: 1 = version (string), ...
package hex

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/git-pkgs/cooldown"
	"github.com/git-pkgs/proxy/internal/packageurl"
	"google.golang.org/protobuf/encoding/protowire"
)

const ecosystem = "hex"

// CooldownPolicy evaluates version age at the caller's supplied time.
type CooldownPolicy interface {
	Enabled() bool
	Evaluate(ecosystem, packagePURL string, publishedAt, evaluatedAt time.Time) cooldown.Decision
}

// Options supplies the policy and the publish times for a filter. The
// registry's package file carries no times, so the caller reads them from
// the Hex API (see ReleaseTimes). EvaluatedAt is used as given; the
// transformation never reads the clock.
type Options struct {
	Cooldown    CooldownPolicy
	EvaluatedAt time.Time
	// Published maps each release's version to its publish time. A release
	// with the zero time is kept.
	Published map[string]time.Time
	// WithholdUnknown holds back the releases Published doesn't have, for
	// a caller whose times may be older than the package file. They are
	// kept otherwise.
	WithholdUnknown bool
	// OnWithheld is called synchronously for each release cooldown holds
	// back, with its publish time: the zero time for a release held back
	// because Published doesn't have it.
	OnWithheld func(name, version string, publishedAt time.Time)
}

// FilterPackage leaves the releases cooldown holds back out of the
// decompressed Signed message of the package name, and returns it gzipped
// with the versions it kept. The payload has changed, so the result
// carries no signature. When no release is held back out is nil: the
// caller serves upstream's bytes, whose signature still holds. Without an
// enabled policy every release is kept.
func FilterPackage(name string, signed []byte, o Options) (out []byte, kept []string, err error) {
	payload, err := Payload(signed)
	if err != nil {
		return nil, nil, err
	}
	versions, err := ReleaseVersions(payload)
	if err != nil {
		return nil, nil, fmt.Errorf("reading releases: %w", err)
	}

	packagePURL := packageurl.MakeString(ecosystem, name, "")
	withheld := make(map[string]bool)
	for _, version := range versions {
		if o.withheld(packagePURL, name, version) {
			withheld[version] = true
			continue
		}
		kept = append(kept, version)
	}
	if len(withheld) == 0 {
		return nil, kept, nil
	}

	filtered, err := FilterReleases(payload, withheld, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("filtering releases: %w", err)
	}
	out, err = UnsignedPackage(filtered)
	return out, kept, err
}

// withheld reports whether cooldown holds a release back, telling
// o.OnWithheld when it does.
func (o Options) withheld(packagePURL, name, version string) bool {
	if o.Cooldown == nil || !o.Cooldown.Enabled() {
		return false
	}
	publishedAt, known := o.Published[version]
	switch {
	case !known:
		if !o.WithholdUnknown {
			return false
		}
	case publishedAt.IsZero():
		return false
	case o.Cooldown.Evaluate("hex", packagePURL, publishedAt, o.EvaluatedAt).Allowed:
		return false
	}
	if o.OnWithheld != nil {
		o.OnWithheld(name, version, publishedAt)
	}
	return true
}

// Payload returns the Package payload of a decompressed Signed message.
func Payload(signed []byte) ([]byte, error) {
	payload, err := BytesField(signed, 1)
	if err != nil {
		return nil, fmt.Errorf("extracting payload: %w", err)
	}
	return payload, nil
}

// ReleaseVersions lists the versions of the releases in a Package payload,
// in their order there.
func ReleaseVersions(payload []byte) ([]string, error) {
	var versions []string
	_, err := FilterReleases(payload, nil, func(version string) {
		versions = append(versions, version)
	})
	return versions, err
}

// FilterSigned leaves the releases whose versions withheld holds out of a
// decompressed Signed message, and returns it gzipped. The payload has
// changed, so the result carries no signature.
func FilterSigned(signed []byte, withheld map[string]bool) ([]byte, error) {
	payload, err := Payload(signed)
	if err != nil {
		return nil, err
	}
	filtered, err := FilterReleases(payload, withheld, nil)
	if err != nil {
		return nil, fmt.Errorf("filtering releases: %w", err)
	}
	return UnsignedPackage(filtered)
}

// UnsignedPackage wraps a Package payload in a Signed message without a
// signature, gzipped as the registry serves it.
func UnsignedPackage(payload []byte) ([]byte, error) {
	var signed []byte
	signed = protowire.AppendTag(signed, 1, protowire.BytesType)
	signed = protowire.AppendBytes(signed, payload)

	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write(signed); err != nil {
		return nil, err
	}
	if err := gw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// FilterReleases returns a Package payload without the releases whose
// versions withheld holds, calling seen, when not nil, with the version of
// each release. Other fields are kept as they are.
func FilterReleases(payload []byte, withheld map[string]bool, seen func(string)) ([]byte, error) {
	var result []byte
	data := payload

	for len(data) > 0 {
		num, wtype, n := protowire.ConsumeTag(data)
		if n < 0 {
			return nil, errors.New("invalid protobuf tag")
		}

		tagBytes := data[:n]
		data = data[n:]

		var fieldBytes []byte
		switch wtype {
		case protowire.BytesType:
			v, vn := protowire.ConsumeBytes(data)
			if vn < 0 {
				return nil, errors.New("invalid protobuf bytes field")
			}
			fieldBytes = data[:vn]
			data = data[vn:]

			if num == 1 { // releases field
				version := ReleaseVersion(v)
				if seen != nil {
					seen(version)
				}
				if withheld[version] {
					continue
				}
			}
		case protowire.VarintType:
			_, vn := protowire.ConsumeVarint(data)
			if vn < 0 {
				return nil, errors.New("invalid protobuf varint")
			}
			fieldBytes = data[:vn]
			data = data[vn:]
		default:
			return nil, fmt.Errorf("unexpected wire type %d", wtype)
		}

		result = append(result, tagBytes...)
		result = append(result, fieldBytes...)
	}

	return result, nil
}

// ReleaseVersion returns the version of a Release message, empty when it
// has none or doesn't parse.
func ReleaseVersion(release []byte) string {
	data := release
	for len(data) > 0 {
		num, wtype, n := protowire.ConsumeTag(data)
		if n < 0 {
			return ""
		}
		data = data[n:]

		switch wtype {
		case protowire.BytesType:
			v, vn := protowire.ConsumeBytes(data)
			if vn < 0 {
				return ""
			}
			if num == 1 {
				return string(v)
			}
			data = data[vn:]
		case protowire.VarintType:
			_, vn := protowire.ConsumeVarint(data)
			if vn < 0 {
				return ""
			}
			data = data[vn:]
		default:
			return ""
		}
	}
	return ""
}

// BytesField returns the first bytes field numbered field in a protobuf
// message.
func BytesField(data []byte, field protowire.Number) ([]byte, error) {
	for len(data) > 0 {
		num, wtype, n := protowire.ConsumeTag(data)
		if n < 0 {
			return nil, errors.New("invalid protobuf tag")
		}
		data = data[n:]

		switch wtype {
		case protowire.BytesType:
			v, vn := protowire.ConsumeBytes(data)
			if vn < 0 {
				return nil, errors.New("invalid protobuf bytes")
			}
			if num == field {
				return v, nil
			}
			data = data[vn:]
		case protowire.VarintType:
			_, vn := protowire.ConsumeVarint(data)
			if vn < 0 {
				return nil, errors.New("invalid protobuf varint")
			}
			data = data[vn:]
		default:
			return nil, fmt.Errorf("unexpected wire type %d", wtype)
		}
	}
	return nil, fmt.Errorf("field %d not found", field)
}

// APIPackage is the part of a Hex API package response
// (/api/packages/{name}) the proxy reads.
type APIPackage struct {
	Releases []APIRelease `json:"releases"`
}

// APIRelease is a release in an APIPackage.
type APIRelease struct {
	Version    string `json:"version"`
	InsertedAt string `json:"inserted_at"`
}

// ReleaseTimes reads each release's publish time from a Hex API package
// response, zero for a release whose inserted_at doesn't parse.
func ReleaseTimes(body []byte) (map[string]time.Time, error) {
	var pkg APIPackage
	if err := json.Unmarshal(body, &pkg); err != nil {
		return nil, err
	}
	times := make(map[string]time.Time, len(pkg.Releases))
	for _, release := range pkg.Releases {
		insertedAt, err := time.Parse(time.RFC3339Nano, release.InsertedAt)
		if err != nil {
			insertedAt = time.Time{}
		}
		times[release.Version] = insertedAt
	}
	return times, nil
}

// TarballIdentity reads the name and version from a tarball filename, such
// as phoenix-1.7.10.tar or amqp_client-4.1.5-1.tar. Hex package names
// can't contain a hyphen, so the first one separates the name from a
// version that may carry hyphens of its own. Both are empty for a filename
// without one.
func TarballIdentity(filename string) (name, version string) {
	name, version, ok := strings.Cut(strings.TrimSuffix(filename, ".tar"), "-")
	if !ok || name == "" || version == "" {
		return "", ""
	}
	return name, version
}
