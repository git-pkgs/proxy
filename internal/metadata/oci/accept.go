package oci

import (
	"mime"
	"sort"
	"strconv"
	"strings"
)

// Manifest media types the Accept negotiation names.
const (
	ImageManifestMediaType      = "application/vnd.oci.image.manifest.v1+json"
	ImageIndexMediaType         = "application/vnd.oci.image.index.v1+json"
	DockerManifestMediaType     = "application/vnd.docker.distribution.manifest.v2+json"
	DockerManifestListMediaType = "application/vnd.docker.distribution.manifest.list.v2+json"
	DockerSignedManifestType    = "application/vnd.docker.distribution.manifest.v1+prettyjws"
)

// DefaultManifestAccept is the Accept value used for a manifest request
// that sends none: every manifest type a registry may answer with.
var DefaultManifestAccept = strings.Join([]string{
	ImageManifestMediaType,
	ImageIndexMediaType,
	DockerManifestMediaType,
	DockerManifestListMediaType,
	DockerSignedManifestType,
}, ", ")

const (
	acceptWildcardSpecificity = iota
	acceptTypeWildcardSpecificity
	acceptExactSpecificity
)

// NormalizeAccept returns accept with its media ranges lowercased, their
// parameters sorted, q=1 dropped and duplicates removed, in a fixed order,
// so Accept values that mean the same share a cache key.
func NormalizeAccept(accept string) string {
	mediaTypes := make(map[string]struct{})
	for _, value := range strings.Split(accept, ",") {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		mediaType, params, err := mime.ParseMediaType(value)
		if err != nil {
			mediaTypes[strings.ToLower(value)] = struct{}{}
			continue
		}
		paramKeys := make([]string, 0, len(params))
		for key := range params {
			paramKeys = append(paramKeys, key)
		}
		sort.Strings(paramKeys)
		canonical := strings.ToLower(mediaType)
		for _, key := range paramKeys {
			value := params[key]
			if strings.EqualFold(key, "q") {
				if quality, err := strconv.ParseFloat(value, 64); err == nil {
					if quality == 1 {
						continue
					}
					value = strconv.FormatFloat(quality, 'g', -1, 64)
				}
			}
			canonical += ";" + strings.ToLower(key) + "=" + value
		}
		mediaTypes[canonical] = struct{}{}
	}
	canonicalMediaTypes := make([]string, 0, len(mediaTypes))
	for mediaType := range mediaTypes {
		canonicalMediaTypes = append(canonicalMediaTypes, mediaType)
	}
	sort.Strings(canonicalMediaTypes)
	return strings.Join(canonicalMediaTypes, ",")
}

// Accepts reports whether accept allows contentType: the most specific
// media range that matches it has a quality above zero.
func Accepts(accept, contentType string) bool {
	contentType, contentParams, err := mime.ParseMediaType(contentType)
	if err != nil {
		return false
	}
	contentType = strings.ToLower(contentType)
	contentMajor, contentMinor, found := strings.Cut(contentType, "/")
	if !found {
		return false
	}

	bestMediaTypeSpecificity := -1
	bestParameterSpecificity := 0
	bestQuality := 0.0
	for _, value := range strings.Split(accept, ",") {
		mediaType, params, err := mime.ParseMediaType(strings.TrimSpace(value))
		if err != nil {
			continue
		}
		mediaType = strings.ToLower(mediaType)
		major, minor, found := strings.Cut(mediaType, "/")
		if found && acceptRangeMatches(major, minor, params, contentMajor, contentMinor, contentParams) {
			mediaTypeSpecificity, parameterSpecificity := acceptSpecificity(major, minor, params)
			if mediaTypeSpecificity > bestMediaTypeSpecificity ||
				(mediaTypeSpecificity == bestMediaTypeSpecificity && parameterSpecificity > bestParameterSpecificity) {
				bestMediaTypeSpecificity = mediaTypeSpecificity
				bestParameterSpecificity = parameterSpecificity
				bestQuality = acceptQuality(params)
			}
		}
	}
	return bestQuality > 0
}

// AcceptsIndex reports whether accept allows an OCI image index or a Docker
// manifest list.
func AcceptsIndex(accept string) bool {
	return Accepts(accept, ImageIndexMediaType) || Accepts(accept, DockerManifestListMediaType)
}

// Names reports whether accept lists mediaType itself and doesn't refuse it
// with q=0. Registries compare Accept values literally, so a wildcard gets a
// client the converted manifest, not the index.
func Names(accept, mediaType string) bool {
	for _, value := range strings.Split(accept, ",") {
		if named, _, err := mime.ParseMediaType(value); err == nil && named == mediaType {
			return Accepts(accept, mediaType)
		}
	}
	return false
}

// IsIndexMediaType reports whether contentType is an OCI image index or a
// Docker manifest list.
func IsIndexMediaType(contentType string) bool {
	mediaType, _, err := mime.ParseMediaType(contentType)
	return err == nil && (mediaType == ImageIndexMediaType || mediaType == DockerManifestListMediaType)
}

func acceptRangeMatches(major, minor string, params map[string]string, contentMajor, contentMinor string, contentParams map[string]string) bool {
	if (major != "*" && major != contentMajor) || (minor != "*" && minor != contentMinor) {
		return false
	}
	for key, value := range params {
		if strings.EqualFold(key, "q") {
			continue
		}
		if contentParams[key] != value {
			return false
		}
	}
	return true
}

func acceptSpecificity(major, minor string, params map[string]string) (int, int) {
	parameterSpecificity := 0
	for key := range params {
		if !strings.EqualFold(key, "q") {
			parameterSpecificity++
		}
	}

	switch {
	case major == "*" && minor == "*":
		return acceptWildcardSpecificity, parameterSpecificity
	case major == "*" || minor == "*":
		return acceptTypeWildcardSpecificity, parameterSpecificity
	default:
		return acceptExactSpecificity, parameterSpecificity
	}
}

func acceptQuality(params map[string]string) float64 {
	value, ok := params["q"]
	if !ok {
		return 1
	}
	quality, err := strconv.ParseFloat(value, 64)
	if err != nil || quality < 0 || quality > 1 {
		return 0
	}
	return quality
}
