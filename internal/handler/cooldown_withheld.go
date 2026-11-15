package handler

import (
	"time"

	"github.com/git-pkgs/proxy/internal/database"
)

// recordWithheld stores the publish time of a version a cooldown filter held
// back, so the package page can list it with when it becomes available,
// also for a package nobody has downloaded yet. name is the package name as
// the ecosystem's downloads store it, so both land on the same package row.
// A version without a package name, a version or a known time is skipped,
// and a failed write is only logged: the metadata is served either way.
func (p *Proxy) recordWithheld(ecosystem, name, version string, publishedAt time.Time) {
	p.recordWithheldWith(ecosystem, name, version, publishedAt, false)
}

// recordWithheldPyPI is recordWithheld for a PyPI release, whose time is its
// newest file's: a later time replaces the stored one.
func (p *Proxy) recordWithheldPyPI(name, version string, publishedAt time.Time) {
	p.recordWithheldWith("pypi", name, version, publishedAt, true)
}

func (p *Proxy) recordWithheldWith(ecosystem, name, version string, publishedAt time.Time, newest bool) {
	if p.DB == nil || name == "" || version == "" || publishedAt.IsZero() {
		return
	}
	packagePURL, versionPURL, err := packagePURLStrings(ecosystem, name, version)
	if err != nil {
		return
	}
	pkg := &database.Package{PURL: packagePURL, Ecosystem: ecosystem, Name: name}
	record := p.DB.RecordWithheldVersion
	if newest {
		record = p.DB.RecordWithheldVersionNewest
	}
	if err := record(pkg, versionPURL, publishedAt); err != nil {
		p.Logger.Warn("cooldown: failed to record withheld version",
			"ecosystem", ecosystem, "name", name, "version", version, "error", err)
	}
}
