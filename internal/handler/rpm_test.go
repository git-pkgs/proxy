package handler

import (
	"testing"
)

func TestRPMHandler_parseRPMPath(t *testing.T) {
	h := &RPMHandler{}

	assertPathParser(t, "parseRPMPath", h.parseRPMPath, []pathParseCase{
		{"releases/39/Everything/x86_64/os/Packages/n/nginx-1.24.0-1.fc39.x86_64.rpm", "nginx", "1.24.0-1.fc39", "x86_64"},
		{"Packages/kernel-core-6.5.5-200.fc38.x86_64.rpm", "kernel-core", "6.5.5-200.fc38", "x86_64"},
		{"updates/39/Everything/aarch64/Packages/g/git-2.42.0-1.fc39.aarch64.rpm", "git", "2.42.0-1.fc39", "aarch64"},
		{"vim-enhanced-9.0.1000-1.fc38.noarch.rpm", "vim-enhanced", "9.0.1000-1.fc38", "noarch"},
		{"invalid.rpm", "", "", ""},
		{"not-an-rpm-file", "", "", ""},
	})
}

func TestRPMHandler_Routes(t *testing.T) {
	h := NewRPMHandler(nil, "http://localhost:8080")
	assertRoutesBasics(t, h.Routes(), "/repodata/repomd.xml", "/releases/../../../etc/passwd")
}
