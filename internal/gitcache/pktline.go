package gitcache

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// pkt-line framing (gitprotocol-common): four hex digits giving the packet
// length including themselves, then the payload. The lengths 0000, 0001 and
// 0002 are the flush, delimiter and response-end packets.
const (
	pktFlush       = "0000"
	pktDelim       = "0001"
	pktHeaderLen   = 4
	pktMaxLen      = 65520
	pktSpecialLast = 2
)

type pktKind int

const (
	pktData pktKind = iota
	pktFlushKind
	pktDelimKind
	pktEndKind
)

func pktLine(payload string) string {
	return fmt.Sprintf("%04x%s", len(payload)+pktHeaderLen, payload)
}

type pktReader struct {
	r *bufio.Reader
}

func newPktReader(r io.Reader) *pktReader {
	return &pktReader{r: bufio.NewReader(r)}
}

// next returns the next packet. A data packet's payload has its trailing
// newline removed.
func (p *pktReader) next() (pktKind, string, error) {
	var header [pktHeaderLen]byte
	if _, err := io.ReadFull(p.r, header[:]); err != nil {
		return 0, "", err
	}
	n, err := strconv.ParseUint(string(header[:]), 16, 16)
	if err != nil {
		return 0, "", fmt.Errorf("invalid pkt-line length %q", header)
	}
	if n <= pktSpecialLast {
		return pktKind(n + 1), "", nil
	}
	if n < pktHeaderLen || n > pktMaxLen {
		return 0, "", fmt.Errorf("invalid pkt-line length %d", n)
	}
	payload := make([]byte, n-pktHeaderLen)
	if _, err := io.ReadFull(p.r, payload); err != nil {
		return 0, "", err
	}
	return pktData, strings.TrimSuffix(string(payload), "\n"), nil
}
