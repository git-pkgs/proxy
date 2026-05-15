package handler

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestJuliaParseRegistryLine(t *testing.T) {
	tests := []struct {
		line     string
		wantUUID string
		wantHash string
		wantOK   bool
	}{
		{
			"/registry/23338594-aafe-5451-b93e-139f81909106/342327538ed6c1ec54c69fa145e7b6bf5934201e",
			"23338594-aafe-5451-b93e-139f81909106",
			"342327538ed6c1ec54c69fa145e7b6bf5934201e",
			true,
		},
		{
			" /registry/23338594-aafe-5451-b93e-139f81909106/342327538ed6c1ec54c69fa145e7b6bf5934201e\n",
			"23338594-aafe-5451-b93e-139f81909106",
			"342327538ed6c1ec54c69fa145e7b6bf5934201e",
			true,
		},
		{"/registry/not-a-uuid/0000", "", "", false},
		{"junk", "", "", false},
		{"", "", "", false},
	}

	for _, tt := range tests {
		uuid, hash, ok := parseRegistryLine(tt.line)
		if uuid != tt.wantUUID || hash != tt.wantHash || ok != tt.wantOK {
			t.Errorf("parseRegistryLine(%q) = (%q, %q, %v), want (%q, %q, %v)",
				tt.line, uuid, hash, ok, tt.wantUUID, tt.wantHash, tt.wantOK)
		}
	}
}

func TestJuliaValidUUID(t *testing.T) {
	tests := []struct {
		s    string
		want bool
	}{
		{"23338594-aafe-5451-b93e-139f81909106", true},
		{"295af30f-e4ad-537b-8983-00126c2a3abe", true},
		{"23338594-AAFE-5451-b93e-139f81909106", false},
		{"23338594aafe5451b93e139f81909106", false},
		{"23338594-aafe-5451-b93e-139f8190910", false},
		{"23338594-aafe-5451-b93e-139f81909106-", false},
		{"23338594-gafe-5451-b93e-139f81909106", false},
		{"", false},
	}

	for _, tt := range tests {
		if got := validJuliaUUID(tt.s); got != tt.want {
			t.Errorf("validJuliaUUID(%q) = %v, want %v", tt.s, got, tt.want)
		}
	}
}

func TestJuliaParseRegistryToml(t *testing.T) {
	data := []byte(`name = "General"
uuid = "23338594-aafe-5451-b93e-139f81909106"

[packages]
295af30f-e4ad-537b-8983-00126c2a3abe = { name = "Revise", path = "R/Revise" }
91a5bcdd-55d7-5caf-9e0b-520d859cae80 = { name = "Plots", path = "P/Plots" }
`)

	names, err := parseRegistryToml(data)
	if err != nil {
		t.Fatalf("parseRegistryToml: %v", err)
	}
	if got := names["295af30f-e4ad-537b-8983-00126c2a3abe"]; got != "Revise" {
		t.Errorf("names[Revise uuid] = %q, want Revise", got)
	}
	if got := names["91a5bcdd-55d7-5caf-9e0b-520d859cae80"]; got != "Plots" {
		t.Errorf("names[Plots uuid] = %q, want Plots", got)
	}
	if len(names) != 2 {
		t.Errorf("len(names) = %d, want 2", len(names))
	}
}

func TestJuliaExtractRegistryNames(t *testing.T) {
	registryToml := `name = "General"
[packages]
295af30f-e4ad-537b-8983-00126c2a3abe = { name = "Revise", path = "R/Revise" }
`
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)

	for _, f := range []struct{ name, body string }{
		{"R/Revise/Package.toml", "name = \"Revise\"\n"},
		{"Registry.toml", registryToml},
	} {
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Mode: 0o644, Size: int64(len(f.body))}); err != nil {
			t.Fatalf("WriteHeader: %v", err)
		}
		if _, err := tw.Write([]byte(f.body)); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar Close: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("gzip Close: %v", err)
	}

	names, err := extractRegistryNames(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("extractRegistryNames: %v", err)
	}
	if got := names["295af30f-e4ad-537b-8983-00126c2a3abe"]; got != "Revise" {
		t.Errorf("names[Revise uuid] = %q, want Revise", got)
	}
}

func TestJuliaResolveName(t *testing.T) {
	h := &JuliaHandler{
		proxy: &Proxy{Logger: slog.Default()},
		names: map[string]string{
			"295af30f-e4ad-537b-8983-00126c2a3abe": "Revise",
		},
	}

	if got := h.resolveName("295af30f-e4ad-537b-8983-00126c2a3abe"); got != "Revise" {
		t.Errorf("resolveName(known) = %q, want Revise", got)
	}
	if got := h.resolveName("00000000-0000-0000-0000-000000000000"); got != "00000000-0000-0000-0000-000000000000" {
		t.Errorf("resolveName(unknown) = %q, want uuid fallback", got)
	}
}

func TestJuliaRoutesValidation(t *testing.T) {
	h := NewJuliaHandler(&Proxy{Logger: slog.Default()}, "")
	routes := h.Routes()

	tests := []struct {
		path string
		want int
	}{
		{"/package/not-a-uuid/342327538ed6c1ec54c69fa145e7b6bf5934201e", http.StatusBadRequest},
		{"/package/295af30f-e4ad-537b-8983-00126c2a3abe/short", http.StatusBadRequest},
		{"/registry/295af30f-e4ad-537b-8983-00126c2a3abe/zzzz", http.StatusBadRequest},
		{"/artifact/nothex", http.StatusBadRequest},
		{"/nope", http.StatusNotFound},
	}

	for _, tt := range tests {
		req := httptest.NewRequest(http.MethodGet, tt.path, nil)
		rr := httptest.NewRecorder()
		routes.ServeHTTP(rr, req)
		if rr.Code != tt.want {
			t.Errorf("GET %s = %d, want %d", tt.path, rr.Code, tt.want)
		}
	}
}
