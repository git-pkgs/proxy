//go:build linux

package gitcache

import (
	"os"
	"syscall"
	"unsafe"
)

// fadvDontneed is POSIX_FADV_DONTNEED, the advice that drops a file's
// clean pages from the page cache; with no length, from its whole rest.
const fadvDontneed = 4

// evictPageCache drops the pages of the files behind the connections from
// the page cache, so a test can check that the cache reads them back. It
// needs no administrator's help: only clean pages go, and the writing
// program has closed the files by then. A page can outstay the advice,
// so what is left is the caller's to see to.
func evictPageCache(conns []*os.File) error {
	for _, conn := range conns {
		if _, _, errno := syscall.Syscall6(syscall.SYS_FADVISE64,
			conn.Fd(), 0, 0, fadvDontneed, 0, 0); errno != 0 {
			return errno
		}
	}
	return nil
}

// pageCount is how many pages the files behind the connections would take
// up in the page cache, read or not. An empty file takes none.
func pageCount(conns []*os.File) int {
	page := os.Getpagesize()
	pages := 0
	for _, conn := range conns {
		st, err := conn.Stat()
		if err != nil || !st.Mode().IsRegular() {
			continue
		}
		pages += int((uintptr(st.Size()) + uintptr(page) - 1) / uintptr(page))
	}
	return pages
}

// residentCount is how many pages of the files behind the connections sit
// in the page cache right now. The pages are mapped for the counting, but
// the kernel counts without faulting them in, so only ones already cached
// add up; a page read back brings its own.
func residentCount(conns []*os.File) int {
	if len(conns) == 0 {
		return -1
	}
	page := uintptr(os.Getpagesize())
	resident := 0
	for _, conn := range conns {
		st, err := conn.Stat()
		if err != nil || st.Size() == 0 || !st.Mode().IsRegular() {
			continue
		}
		length := (uintptr(st.Size()) + page - 1) &^ (page - 1)
		data, err := syscall.Mmap(int(conn.Fd()), 0, int(length), syscall.PROT_READ, syscall.MAP_SHARED)
		if err != nil {
			continue
		}
		vec := make([]byte, int(length/page))
		_, _, errno := syscall.Syscall(syscall.SYS_MINCORE,
			uintptr(unsafe.Pointer(&data[0])), length, uintptr(unsafe.Pointer(&vec[0])))
		_ = syscall.Munmap(data)
		if errno != 0 {
			continue
		}
		for _, res := range vec {
			if res&1 == 1 {
				resident++
			}
		}
	}
	return resident
}
