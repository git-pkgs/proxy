package handler

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/git-pkgs/proxy/internal/config"
)

const (
	terraformTestModule = "/modules/" + terraformTestHost + "/acme/vpc/aws/"
	// terraformTestModuleVersions is the module's version list, in the
	// shape registry.opentofu.org returns it.
	// terraformTestEmptyDownload is answered with a 204 without a location.
	terraformTestEmptyDownload  = "1.9.0"
	terraformTestModuleVersions = `{"modules":[{"versions":[{"version":"1.1.0","discovered":"2026-11-01T00:00:00Z"},{"version":"1.0.0","discovered":"2026-10-01T00:00:00Z"}]}]}`
)

// terraformModuleLocations are the module's download locations by version.
// 1.0.0 is given in the X-Terraform-Get header, as registry.terraform.io
// does, and the others in a JSON body, as registry.opentofu.org does.
var terraformModuleLocations = map[string]string{
	"1.0.0": "git::https://github.com/acme/terraform-aws-vpc?ref=v1.0.0",
	"1.1.0": "git::https://github.com/acme/terraform-aws-vpc.git//modules/subnets?ref=v1.1.0&depth=1",
	"1.2.0": "git::https://gitlab.example/acme/vpc?ref=v1.2.0",
	"1.3.0": "./archives/vpc-1.3.0.tar.gz",
	"1.4.0": "git::https://github.com/other/terraform-aws-vpc?ref=v1.4.0",
	"1.5.0": "git::https://token@github.com/acme/terraform-aws-vpc?ref=v1.5.0",
	"1.6.0": "git::ssh://git@github.com/acme/terraform-aws-vpc?ref=v1.6.0",
	"1.7.0": "/archives/vpc-1.7.0.tar.gz",
	"1.8.0": "../../archives/vpc-1.8.0.tar.gz",
}

// terraformModuleHeaderAndBody is answered with a 200 that gives one
// location in X-Terraform-Get and another in the body, and
// terraformModuleFailing with a 500 that has the header.
const (
	terraformModuleHeaderAndBody = "2.0.0"
	terraformModuleFailing       = "2.1.0"
	terraformModuleEmptyBody     = "2.2.0"
)

// serveModule serves the Module Registry Protocol for acme/vpc/aws.
func (s *fakeTerraformRegistry) serveModule(w http.ResponseWriter, r *http.Request) {
	// TestTerraformModuleDiscovery moves modules.v1 there.
	r.URL.Path = strings.Replace(r.URL.Path, "/api/modules/v1/", "/v1/modules/", 1)
	if r.URL.Path == "/v1/modules/acme/broken/aws/versions" {
		_, _ = w.Write([]byte(`{"modules":[`))
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/v1/modules/acme/vpc/aws/")
	if rest == r.URL.Path {
		http.NotFound(w, r)
		return
	}
	if rest == "versions" {
		_, _ = w.Write([]byte(terraformTestModuleVersions))
		return
	}
	version, ok := strings.CutSuffix(rest, "/download")
	switch {
	case ok && version == terraformTestEmptyDownload:
		w.WriteHeader(http.StatusNoContent)
		return
	case ok && version == terraformModuleEmptyBody:
		_, _ = w.Write([]byte(`{"location":""}`))
		return
	case ok && version == terraformModuleHeaderAndBody:
		w.Header().Set("X-Terraform-Get", terraformModuleLocations["1.2.0"])
		_ = json.NewEncoder(w).Encode(map[string]string{"location": terraformModuleLocations["1.6.0"]})
		return
	case ok && version == terraformModuleFailing:
		w.Header().Set("X-Terraform-Get", terraformModuleLocations["1.2.0"])
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	location := terraformModuleLocations[version]
	if !ok || location == "" {
		http.NotFound(w, r)
		return
	}
	if version == "1.0.0" {
		w.Header().Set("X-Terraform-Get", location)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]string{"location": location})
}

// withTestGit lets h hand out clones through a Git handler for github.com,
// limited to acme's repositories, at proxy.example.
func withTestGit(h *TerraformHandler) *TerraformHandler {
	return h.WithGit(NewGitHandler(nil, nil, config.UpstreamConfig{Git: map[string]config.GitUpstream{
		"github": {URL: "https://github.com", Repos: []string{"acme/*"}},
	}}).WithBaseURL("https://proxy.example/"))
}

func TestTerraformModuleVersions(t *testing.T) {
	s := newFakeTerraformRegistry(t)
	h, _ := newTerraformTestHandler(t, s)

	w := serveTerraformRequest(h, terraformTestModule+"versions")
	if w.Code != http.StatusOK || w.Body.String() != terraformTestModuleVersions {
		t.Fatalf("versions = %d %q, want the registry's list", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != contentTypeJSON {
		t.Errorf("Content-Type = %q, want %q", ct, contentTypeJSON)
	}
	if got := s.requestCount("/v1/modules/acme/vpc/aws/versions"); got != 1 {
		t.Errorf("versions requests = %d, want 1", got)
	}
}

func TestTerraformModuleDownloadLocation(t *testing.T) {
	s := newFakeTerraformRegistry(t)
	tests := []struct {
		version, want string
	}{
		// Repositories the Git handler serves are cloned through it.
		{"1.0.0", "git::https://proxy.example/git/github/acme/terraform-aws-vpc?ref=v1.0.0"},
		{"1.1.0", "git::https://proxy.example/git/github/acme/terraform-aws-vpc.git//modules/subnets?ref=v1.1.0&depth=1"},
		// Others are passed on as the registry gave them.
		{"1.2.0", terraformModuleLocations["1.2.0"]},
		{"1.4.0", terraformModuleLocations["1.4.0"]},
		{"1.5.0", terraformModuleLocations["1.5.0"]},
		{"1.6.0", terraformModuleLocations["1.6.0"]},
		// A relative location is resolved against the registry's download
		// URL, as Terraform would.
		{"1.3.0", s.api.URL + "/v1/modules/acme/vpc/aws/1.3.0/archives/vpc-1.3.0.tar.gz"},
		{"1.7.0", s.api.URL + "/archives/vpc-1.7.0.tar.gz"},
		{"1.8.0", s.api.URL + "/v1/modules/acme/vpc/archives/vpc-1.8.0.tar.gz"},
		// Terraform reads the header before the body.
		{terraformModuleHeaderAndBody, terraformModuleLocations["1.2.0"]},
	}
	h, _ := newTerraformTestHandler(t, s)
	h = withTestGit(h)
	for _, tt := range tests {
		t.Run(tt.version, func(t *testing.T) {
			w := serveTerraformRequest(h, terraformTestModule+tt.version+"/download")
			if w.Code != http.StatusNoContent {
				t.Fatalf("status = %d: %s", w.Code, w.Body.String())
			}
			if got := w.Header().Get("X-Terraform-Get"); got != tt.want {
				t.Errorf("X-Terraform-Get = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestTerraformModuleDownloadWithoutGit(t *testing.T) {
	s := newFakeTerraformRegistry(t)
	h, _ := newTerraformTestHandler(t, s)

	w := serveTerraformRequest(h, terraformTestModule+"1.0.0/download")
	if got := w.Header().Get("X-Terraform-Get"); w.Code != http.StatusNoContent || got != terraformModuleLocations["1.0.0"] {
		t.Errorf("download = %d %q, want the registry's location", w.Code, got)
	}
}

func TestTerraformModuleServedWhileRegistryIsDown(t *testing.T) {
	s := newFakeTerraformRegistry(t)
	h, proxy := newTerraformTestHandler(t, s)
	h = withTestGit(h)
	for _, file := range []string{"versions", "1.0.0/download"} {
		if w := serveTerraformRequest(h, terraformTestModule+file); w.Code/100 != 2 {
			t.Fatalf("%s status = %d: %s", file, w.Code, w.Body.String())
		}
	}

	s.available.Store(false)
	proxy.MetadataTTL = 0
	if w := serveTerraformRequest(h, terraformTestModule+"versions"); w.Code != http.StatusOK || w.Body.String() != terraformTestModuleVersions || w.Header().Get("Warning") == "" {
		t.Errorf("offline versions = %d %q, Warning %q; want the stored list marked stale", w.Code, w.Body.String(), w.Header().Get("Warning"))
	}
	w := serveTerraformRequest(h, terraformTestModule+"1.0.0/download")
	if got := w.Header().Get("X-Terraform-Get"); w.Code != http.StatusNoContent || got != "git::https://proxy.example/git/github/acme/terraform-aws-vpc?ref=v1.0.0" || w.Header().Get("Warning") == "" {
		t.Errorf("offline download = %d %q, Warning %q; want the stored location marked stale", w.Code, got, w.Header().Get("Warning"))
	}
}

func TestTerraformModuleDenylistIgnoresAddressCase(t *testing.T) {
	s := newFakeTerraformRegistry(t)
	h, proxy := newTerraformTestHandler(t, s)
	setTestDenylist(t, proxy, "pkg:terraform/acme/vpc@1.0.0")

	for _, target := range []string{
		terraformTestModule + "1.0.0/download",
		"/modules/" + terraformTestHost + "/Acme/VPC/AWS/1.0.0/download",
	} {
		if w := serveTerraformRequest(h, target); w.Code != http.StatusForbidden {
			t.Errorf("%s status = %d, want 403 for a denied version", target, w.Code)
		}
	}
	if got := s.totalRequests(); got != 0 {
		t.Errorf("upstream requests = %d, want 0 for a denied version", got)
	}
	if w := serveTerraformRequest(h, "/modules/"+terraformTestHost+"/Acme/VPC/AWS/1.1.0/download"); w.Code != http.StatusNoContent {
		t.Errorf("1.1.0 status = %d, want 204: %s", w.Code, w.Body.String())
	}
	if got := s.requestCount("/v1/modules/acme/vpc/aws/1.1.0/download"); got != 1 {
		t.Errorf("lowercased download requests = %d, want 1", got)
	}
}

func TestTerraformModuleNotFound(t *testing.T) {
	s := newFakeTerraformRegistry(t)
	h, _ := newTerraformTestHandler(t, s)

	for _, target := range []string{
		"/modules/" + terraformTestHost + "/acme/missing/aws/versions",
		terraformTestModule + "9.9.9/download",
	} {
		if w := serveTerraformRequest(h, target); w.Code != http.StatusNotFound {
			t.Errorf("%s status = %d, want 404", target, w.Code)
		}
	}
}

func TestTerraformModuleRejectsUnknownAndInvalidRequests(t *testing.T) {
	s := newFakeTerraformRegistry(t)
	h, _ := newTerraformTestHandler(t, s)

	for target, want := range map[string]int{
		"/modules/evil.example/acme/vpc/aws/versions":               http.StatusNotFound,
		"/modules/" + terraformTestHost + "/ac.me/vpc/aws/versions": http.StatusBadRequest,
		"/modules/" + terraformTestHost + "/acme/vpc/a.ws/versions": http.StatusBadRequest,
		terraformTestModule + "1.0/download":                        http.StatusBadRequest,
		terraformTestModule + "1.0.0/source":                        http.StatusNotFound,
	} {
		if w := serveTerraformRequest(h, target); w.Code != want {
			t.Errorf("%s status = %d, want %d", target, w.Code, want)
		}
	}
	if got := s.totalRequests(); got != 0 {
		t.Errorf("upstream requests = %d, want 0", got)
	}
}

// A download without a location, such as an empty 204, is a bad answer
// from the registry, and isn't stored.
func TestTerraformModuleDownloadWithoutLocation(t *testing.T) {
	s := newFakeTerraformRegistry(t)
	h, _ := newTerraformTestHandler(t, s)

	for _, version := range []string{terraformTestEmptyDownload, terraformModuleEmptyBody, terraformModuleFailing} {
		for i := range 2 {
			if w := serveTerraformRequest(h, terraformTestModule+version+"/download"); w.Code != http.StatusBadGateway {
				t.Errorf("%s request %d status = %d, want 502", version, i+1, w.Code)
			}
		}
		if got := s.requestCount("/v1/modules/acme/vpc/aws/" + version + "/download"); got != 2 {
			t.Errorf("%s download requests = %d, want 2", version, got)
		}
	}
}

// A version list that isn't JSON is a bad answer too, and isn't stored.
func TestTerraformModuleVersionsNotJSON(t *testing.T) {
	s := newFakeTerraformRegistry(t)
	h, _ := newTerraformTestHandler(t, s)

	for i := range 2 {
		if w := serveTerraformRequest(h, "/modules/"+terraformTestHost+"/acme/broken/aws/versions"); w.Code != http.StatusBadGateway {
			t.Errorf("request %d status = %d, want 502", i+1, w.Code)
		}
	}
	if got := s.requestCount("/v1/modules/acme/broken/aws/versions"); got != 2 {
		t.Errorf("versions requests = %d, want 2", got)
	}
}

func TestTerraformModuleDiscovery(t *testing.T) {
	for discovered, want := range map[string]int{
		`"/api/modules/v1/"`: http.StatusOK,
		`""`:                 http.StatusBadGateway,
		`{"url":"/v1/"}`:     http.StatusBadGateway,
		`null`:               http.StatusBadGateway,
	} {
		t.Run(discovered, func(t *testing.T) {
			s := newFakeTerraformRegistry(t)
			s.modulesPath = discovered
			h, _ := newTerraformTestHandler(t, s)
			if w := serveTerraformRequest(h, terraformTestModule+"versions"); w.Code != want {
				t.Errorf("status = %d, want %d: %s", w.Code, want, w.Body.String())
			}
		})
	}
}
