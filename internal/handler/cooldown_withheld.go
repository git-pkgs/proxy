package handler

import (
	"time"

	"github.com/git-pkgs/proxy/internal/database"
)

// recordWithheld stores the publish time of a version a cooldown filter held
// back, so the package page can list it with when it becomes available,
// also for a package nobody has downloaded yet. name is the package name as
// the ecosystem's downloads store it, so both land on the same package row.
// A version without a name or a known time is skipped, and a failed write is
// only logged: the metadata is served either way.
func (p *Proxy) recordWithheld(ecosystem, name, version string, publishedAt time.Time) {
	if p.DB == nil || version == "" || publishedAt.IsZero() {
		return
	}
	packagePURL, versionPURL, err := packagePURLStrings(ecosystem, name, version)
	if err != nil {
		return
	}
	pkg := &database.Package{PURL: packagePURL, Ecosystem: ecosystem, Name: name}
	if err := p.DB.RecordWithheldVersion(pkg, versionPURL, publishedAt); err != nil {
		p.Logger.Warn("cooldown: failed to record withheld version",
			"ecosystem", ecosystem, "name", name, "version", version, "error", err)
	}
}
