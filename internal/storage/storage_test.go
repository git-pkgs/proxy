package storage

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"strings"
	"testing"
)

func TestArtifactPath(t *testing.T) {
	tests := []struct {
		ecosystem string
		namespace string
		name      string
		version   string
		filename  string
		want      string
	}{
		{"npm", "", "lodash", "4.17.21", "lodash-4.17.21.tgz", "npm/lodash/4.17.21/lodash-4.17.21.tgz"},
		{"npm", "babel", "core", "7.0.0", "core-7.0.0.tgz", "npm/babel/core/7.0.0/core-7.0.0.tgz"},
		{"cargo", "", "serde", "1.0.0", "serde-1.0.0.crate", "cargo/serde/1.0.0/serde-1.0.0.crate"},
		{"pypi", "", "requests", "2.28.0", "requests-2.28.0.tar.gz", "pypi/requests/2.28.0/requests-2.28.0.tar.gz"},
		{"maven", "org.apache", "commons-lang3", "3.12.0", "commons-lang3-3.12.0.jar", "maven/org.apache/commons-lang3/3.12.0/commons-lang3-3.12.0.jar"},
	}

	for _, tt := range tests {
		got := ArtifactPath(tt.ecosystem, tt.namespace, tt.name, tt.version, tt.filename)
		if got != tt.want {
			t.Errorf("ArtifactPath(%q, %q, %q, %q, %q) = %q, want %q",
				tt.ecosystem, tt.namespace, tt.name, tt.version, tt.filename, got, tt.want)
		}
	}
}

func TestHashingReader(t *testing.T) {
	content := "hello world"
	r := NewHashingReader(strings.NewReader(content))

	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll failed: %v", err)
	}

	if string(data) != content {
		t.Errorf("got content %q, want %q", string(data), content)
	}

	if r.Size() != int64(len(content)) {
		t.Errorf("got size %d, want %d", r.Size(), len(content))
	}

	h := sha256.Sum256([]byte(content))
	wantHash := hex.EncodeToString(h[:])
	if r.Sum() != wantHash {
		t.Errorf("got hash %s, want %s", r.Sum(), wantHash)
	}
}
