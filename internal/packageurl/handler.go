package packageurl

import (
	"github.com/git-pkgs/purl"
)

// distroTypes are PURL types whose namespace names the distribution
// ("alpine", "debian"), not part of the package name the handlers store.
var distroTypes = map[string]bool{"alpm": true, "apk": true, "bsd": true, "deb": true, "rpm": true}

// HandlerEcosystem returns the ecosystem name the proxy's handlers store
// packages of a PURL type under. purl names gem and composer after their
// registries, rubygems and packagist; the handlers use the type.
func HandlerEcosystem(purlType string) string {
	switch purlType {
	case "gem", "composer":
		return purlType
	default:
		return purl.PURLTypeToEcosystem(purlType)
	}
}

// HandlerPackage returns the ecosystem and package name the proxy's handlers
// store p under, so a package named in a PURL, such as one from an SBOM,
// matches the package a client fetched through the proxy:
//
//   - apk, alpm, bsd, deb and rpm: the namespace names the distribution and
//     is dropped
//   - conda: the channel, from the namespace or the channel qualifier, is
//     part of the name ("conda-forge/numpy"); without either the name stays
//     bare
//   - pypi and nuget: the name in purl's comparison form, PEP 503 for PyPI
//     and lowercased for NuGet, as the handlers store them
//
// Other types use purl's full name, which keeps the namespace: "@babel/core",
// or "org.slf4j:slf4j-api" for Maven, as its handler stores it.
func HandlerPackage(p *purl.PURL) (ecosystem, name string) {
	ecosystem = HandlerEcosystem(p.Type)
	switch {
	case distroTypes[p.Type]:
		return ecosystem, p.Name
	case p.Type == "conda":
		return ecosystem, condaName(p.Namespace, p.Name, p.Qualifier("channel"))
	case p.Type == "pypi" || p.Type == "nuget":
		return ecosystem, p.ComparisonForm().FullName()
	}
	return ecosystem, p.FullName()
}

func condaName(namespace, name, channel string) string {
	switch {
	case namespace != "":
		return namespace + "/" + name
	case channel != "":
		return channel + "/" + name
	default:
		return name
	}
}
