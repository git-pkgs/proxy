package pypi

import (
	"regexp"
	"strconv"
	"strings"
)

// pep440Pattern is the PEP 440 version pattern, as packaging spells it.
var pep440Pattern = regexp.MustCompile(`(?i)^\s*v?` +
	`(?:(?P<epoch>[0-9]+)!)?` +
	`(?P<release>[0-9]+(?:\.[0-9]+)*)` +
	`(?P<pre>[-_.]?(?P<pre_l>alpha|a|beta|b|preview|pre|c|rc)[-_.]?(?P<pre_n>[0-9]+)?)?` +
	`(?P<post>-(?P<post_n1>[0-9]+)|[-_.]?(?P<post_l>post|rev|r)[-_.]?(?P<post_n2>[0-9]+)?)?` +
	`(?P<dev>[-_.]?(?P<dev_l>dev)[-_.]?(?P<dev_n>[0-9]+)?)?` +
	`(?:\+(?P<local>[a-z0-9]+(?:[-_.][a-z0-9]+)*))?\s*$`)

var localSeparators = strings.NewReplacer("-", ".", "_", ".")

// CanonicalVersion returns version in the form packaging's
// canonicalize_version gives it: PEP 440 normalized, with trailing zeros of
// the release left out, so versions PEP 440 counts as equal, such as 2.0,
// 2.0.0 and V2.0.0.0, have one form. PyPI's JSON API names a release by its
// normalized version, but accepts wheels whose filename spells it any way
// that compares equal. A string that isn't a PEP 440 version is returned as
// it is.
func CanonicalVersion(version string) string {
	m := pep440Pattern.FindStringSubmatch(strings.TrimSpace(version))
	if m == nil {
		return version
	}
	group := func(name string) string { return m[pep440Pattern.SubexpIndex(name)] }
	var b strings.Builder
	if epoch := number(group("epoch")); epoch != "0" {
		b.WriteString(epoch + "!")
	}
	release := strings.Split(group("release"), ".")
	for i := range release {
		release[i] = number(release[i])
	}
	for len(release) > 1 && release[len(release)-1] == "0" {
		release = release[:len(release)-1]
	}
	b.WriteString(strings.Join(release, "."))
	if group("pre") != "" {
		b.WriteString(preLabel(strings.ToLower(group("pre_l"))) + number(group("pre_n")))
	}
	if group("post") != "" {
		b.WriteString(".post" + number(group("post_n1")+group("post_n2")))
	}
	if group("dev") != "" {
		b.WriteString(".dev" + number(group("dev_n")))
	}
	if local := group("local"); local != "" {
		parts := strings.Split(localSeparators.Replace(strings.ToLower(local)), ".")
		for i, part := range parts {
			if strings.Trim(part, "0123456789") == "" {
				parts[i] = number(part)
			}
		}
		b.WriteString("+" + strings.Join(parts, "."))
	}
	return b.String()
}

// canonicalVersions returns versions in CanonicalVersion's form, for
// comparing the versions files are named with.
func canonicalVersions(versions map[string]bool) map[string]bool {
	canonical := make(map[string]bool, len(versions))
	for version, ok := range versions {
		if ok {
			canonical[CanonicalVersion(version)] = true
		}
	}
	return canonical
}

func preLabel(label string) string {
	switch label {
	case "alpha", "a":
		return "a"
	case "beta", "b":
		return "b"
	default:
		return "rc"
	}
}

// number returns digits without leading zeros, "0" for none.
func number(digits string) string {
	n, err := strconv.ParseUint(digits, 10, 64)
	if err != nil {
		trimmed := strings.TrimLeft(digits, "0")
		if trimmed == "" {
			return "0"
		}
		return trimmed
	}
	return strconv.FormatUint(n, 10)
}
