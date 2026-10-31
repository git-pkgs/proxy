package handler

// OSPackageIdentity returns the name, version and architecture the deb,
// alpine and rpm download handlers cache a package file under, read from its
// path below the repository the way they read it. It returns empty strings
// for other ecosystems and for paths they would not cache.
func OSPackageIdentity(ecosystem, path string) (name, version, arch string) {
	switch ecosystem {
	case "deb":
		return (*DebianHandler)(nil).parsePoolPath("/" + path)
	case apkEcosystem:
		return (*APKHandler)(nil).parseAPKPath(path)
	case "rpm":
		return (*RPMHandler)(nil).parseRPMPath(path)
	}
	return "", "", ""
}
