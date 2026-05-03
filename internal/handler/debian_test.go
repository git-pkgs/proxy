package handler

import (
	"testing"
	"github.com/git-pkgs/proxy/internal/config/debian"
)

func TestDebianHandler_parsePoolPath(t *testing.T) {
	h := &DebianHandler{}

	assertPathParser(t, "parsePoolPath", h.parsePoolPath, []pathParseCase{
		{"pool/main/n/nginx/nginx_1.18.0-6_amd64.deb", "nginx", "1.18.0-6", "amd64"},
		{"pool/main/libn/libncurses/libncurses6_6.2-1_amd64.deb", "libncurses6", "6.2-1", "amd64"},
		{"pool/contrib/v/virtualbox/virtualbox_6.1.38-1_amd64.deb", "virtualbox", "6.1.38-1", "amd64"},
		{"pool/main/g/git/git_2.39.2-1_arm64.deb", "git", "2.39.2-1", "arm64"},
		{"invalid/path", "", "", ""},
		{"pool/main/n/nginx/nginx.deb", "", "", ""},
	})
}

func TestDebianHandler_Routes(t *testing.T) {
	h := NewDebianHandler(nil, "http://localhost:8080", debian.RouteDefault)
	assertRoutesBasics(t, h.Routes(), "/dists/stable/Release", "/pool/../../../etc/passwd")
}
