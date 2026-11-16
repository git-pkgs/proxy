//go:build !linux

package gitcache

import (
	"errors"
	"os"
)

// evictPageCache reports that only Linux lets a test drop pages from the
// page cache without the administrator's help, so the page cache test
// skips everywhere else. Nothing of it runs there, so the two counts need
// no more than a shape.
func evictPageCache(_ []*os.File) error {
	return errors.New("no way to drop the page cache here")
}

func pageCount(_ []*os.File) int {
	return 0
}

func residentCount(_ []*os.File) int {
	return 0
}
