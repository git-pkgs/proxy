package handler

import (
	"net/http"
	"strings"
	"time"
)

type filenameDownload struct {
	ecosystem   string
	upstreamURL string
	suffix      string
	parseErr    string
	fetchErr    string
	parse       func(string) (name, version string)
	// publishTime reads a version's publish time from upstream metadata for
	// the download-time cooldown check.
	publishTime func(r *http.Request, name, version string) (time.Time, bool, error)
}

func (p *Proxy) handleFilenameDownload(w http.ResponseWriter, r *http.Request, d filenameDownload) {
	filename := r.PathValue("filename")
	if filename == "" || !strings.HasSuffix(filename, d.suffix) {
		http.Error(w, "invalid filename", http.StatusBadRequest)
		return
	}

	name, version := d.parse(filename)
	if name == "" || version == "" {
		http.Error(w, d.parseErr, http.StatusBadRequest)
		return
	}

	p.Logger.Info(d.ecosystem+" download request",
		"name", name, "version", version, "filename", filename)

	if p.withholdInCooldown(w, downloadVersion{
		ecosystem: d.ecosystem, policyName: name, storedName: name, version: version,
		lookup: func() (time.Time, bool, error) { return d.publishTime(r, name, version) },
	}) {
		return
	}

	downloadURL := d.upstreamURL + r.URL.Path
	result, err := p.GetOrFetchArtifactFromURL(
		r.Context(), d.ecosystem, name, version, filename, downloadURL,
	)
	if err != nil {
		p.serveArtifactError(w, err, d.fetchErr)
		return
	}

	ServeArtifactRequest(w, r, result)
}
