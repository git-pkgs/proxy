package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestServeHelpListsUpstreamEnvironmentVariables(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestServeHelpProcess$")
	cmd.Env = append(os.Environ(), "PROXY_TEST_SERVE_HELP=1")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("serve help failed: %v\n%s", err, output)
	}

	variables := []string{
		"PROXY_UPSTREAM_ALLOW_PRIVATE_HOSTS",
		"PROXY_UPSTREAM_ALLOW_LOOPBACK",
		"PROXY_UPSTREAM_NPM",
		"PROXY_UPSTREAM_CARGO",
		"PROXY_UPSTREAM_CARGO_DOWNLOAD",
		"PROXY_UPSTREAM_GEM",
		"PROXY_UPSTREAM_GO",
		"PROXY_UPSTREAM_HEX",
		"PROXY_UPSTREAM_HEX_API",
		"PROXY_UPSTREAM_PUB",
		"PROXY_UPSTREAM_PYPI",
		"PROXY_UPSTREAM_PYPI_DOWNLOAD",
		"PROXY_UPSTREAM_MAVEN",
		"PROXY_UPSTREAM_GRADLE_PLUGIN_PORTAL",
		"PROXY_UPSTREAM_NUGET",
		"PROXY_UPSTREAM_NUGET_SEARCH",
		"PROXY_UPSTREAM_COMPOSER",
		"PROXY_UPSTREAM_COMPOSER_REPOSITORY",
		"PROXY_UPSTREAM_CONAN",
		"PROXY_UPSTREAM_CONDA",
		"PROXY_UPSTREAM_CRAN",
		"PROXY_UPSTREAM_JULIA",
		"PROXY_UPSTREAM_OCI_DEFAULT",
		"PROXY_UPSTREAM_DEBIAN",
		"PROXY_UPSTREAM_RPM",
	}
	for _, variable := range variables {
		if !strings.Contains(string(output), variable) {
			t.Errorf("serve help omitted %s", variable)
		}
	}
}

func TestServeHelpProcess(*testing.T) {
	if os.Getenv("PROXY_TEST_SERVE_HELP") != "1" {
		return
	}
	os.Args = []string{"proxy", "serve", "-help"}
	main()
}
