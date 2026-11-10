package mirror

import (
	"io"
	"strings"
	"testing"
)

func TestIndexLimitReaderAllowsExactlyTheLimit(t *testing.T) {
	limit := maxIndexBytes
	maxIndexBytes = 8
	t.Cleanup(func() { maxIndexBytes = limit })

	if _, err := io.ReadAll(&indexLimitReader{r: strings.NewReader("12345678"), remaining: maxIndexBytes}); err != nil {
		t.Errorf("index of exactly the limit: %v", err)
	}
	if _, err := io.ReadAll(&indexLimitReader{r: strings.NewReader("123456789"), remaining: maxIndexBytes}); err == nil {
		t.Error("index one byte over the limit read without an error")
	}
}
