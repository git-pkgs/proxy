package handler

import "testing"

func TestOSPackageIdentity(t *testing.T) {
	tests := []struct {
		ecosystem, path     string
		name, version, arch string
	}{
		{"deb", "pool/main/c/curl/curl_7.88.1-10+deb12u8_amd64.deb", "curl", "7.88.1-10+deb12u8", "amd64"},
		{"alpine", "v3.20/main/aarch64/curl-8.9.1-r0.apk", "curl", "8.9.1-r0", "aarch64"},
		{"rpm", "Packages/c/curl-8.6.0-10.fc40.x86_64.rpm", "curl", "8.6.0-10.fc40", "x86_64"},
		{"deb", "pool/main/c/curl/README", "", "", ""},
		{"npm", "lodash-4.17.21.tgz", "", "", ""},
	}
	for _, tt := range tests {
		name, version, arch := OSPackageIdentity(tt.ecosystem, tt.path)
		if name != tt.name || version != tt.version || arch != tt.arch {
			t.Errorf("OSPackageIdentity(%s, %s) = %q %q %q, want %q %q %q",
				tt.ecosystem, tt.path, name, version, arch, tt.name, tt.version, tt.arch)
		}
	}
}
