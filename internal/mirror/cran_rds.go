package mirror

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// R serialization types and flags, from R's serialize.c. Only those a
// character matrix and its dimnames are written with are read.
const (
	rdsSymSXP   = 1
	rdsListSXP  = 2
	rdsCharSXP  = 9
	rdsLglSXP   = 10
	rdsIntSXP   = 13
	rdsStrSXP   = 16
	rdsVecSXP   = 19
	rdsNilValue = 254
	rdsRefSXP   = 255

	rdsTypeMask = 0xff
	rdsRefShift = 8
	rdsHasAttr  = 1 << 9
	rdsHasTag   = 1 << 10

	rdsMagic    = "X\n"
	rdsMatrix   = 2 // dimensions of a matrix
	rdsFormatV2 = 2
	rdsFormatV3 = 3

	// rdsMaxDepth limits nesting; a matrix needs four levels.
	rdsMaxDepth = 16
)

// rdsSymbol is a symbol, the tag of an attribute.
type rdsSymbol string

// rdsAttr is one attribute of a vector.
type rdsAttr struct {
	tag   rdsSymbol
	value any
}

// rdsVector is an R vector: []*string (nil for NA) for a character vector,
// []int32 for an integer or logical one, []any for a list.
type rdsVector struct {
	values any
	attrs  []rdsAttr
}

// rdsReader reads R's XDR serialization format, as saveRDS writes it
// uncompressed.
type rdsReader struct {
	r    *bytes.Reader
	refs []any
}

// readRDSStringMatrix reads an R character matrix saved by saveRDS,
// uncompressed, returning its values in column-major order, nil for NA, its
// number of rows and its column names.
func readRDSStringMatrix(data []byte) (values []*string, nrow int, columns []string, err error) {
	rd := &rdsReader{r: bytes.NewReader(data)}
	if err := rd.header(); err != nil {
		return nil, 0, nil, err
	}
	item, err := rd.item(0)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("reading rds: %w", err)
	}
	vec, ok := item.(*rdsVector)
	if !ok {
		return nil, 0, nil, errors.New("rds is not a vector")
	}
	values, ok = vec.values.([]*string)
	if !ok {
		return nil, 0, nil, errors.New("rds is not a character vector")
	}
	var dim []int32
	var dimnames []any
	for _, attr := range vec.attrs {
		v, _ := attr.value.(*rdsVector)
		if v == nil {
			continue
		}
		switch attr.tag {
		case "dim":
			dim, _ = v.values.([]int32)
		case "dimnames":
			dimnames, _ = v.values.([]any)
		}
	}
	if len(dim) != rdsMatrix || dim[0] < 0 || dim[1] < 0 || int(dim[0])*int(dim[1]) != len(values) {
		return nil, 0, nil, errors.New("rds is not a matrix")
	}
	if len(dimnames) != rdsMatrix {
		return nil, 0, nil, errors.New("rds matrix has no column names")
	}
	names, _ := dimnames[1].(*rdsVector)
	if names == nil {
		return nil, 0, nil, errors.New("rds matrix has no column names")
	}
	colnames, _ := names.values.([]*string)
	if len(colnames) != int(dim[1]) {
		return nil, 0, nil, errors.New("rds matrix has no column names")
	}
	columns = make([]string, len(colnames))
	for i, name := range colnames {
		if name != nil {
			columns[i] = *name
		}
	}
	return values, int(dim[0]), columns, nil
}

func (rd *rdsReader) header() error {
	magic := make([]byte, len(rdsMagic))
	if _, err := io.ReadFull(rd.r, magic); err != nil || string(magic) != rdsMagic {
		return errors.New("rds is not in R's XDR format")
	}
	version, err := rd.int()
	if err != nil {
		return err
	}
	// The R version that wrote it and the oldest that reads it.
	for range 2 {
		if _, err := rd.int(); err != nil {
			return err
		}
	}
	switch version {
	case rdsFormatV2:
	case rdsFormatV3:
		// The native encoding, unused: CHARSXPs are UTF-8 or ASCII.
		n, err := rd.int()
		if err != nil {
			return err
		}
		if n < 0 || int64(n) > int64(rd.r.Len()) {
			return errors.New("rds header is truncated")
		}
		if _, err := rd.r.Seek(int64(n), io.SeekCurrent); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unsupported rds format version %d", version)
	}
	return nil
}

func (rd *rdsReader) int() (int32, error) {
	var v int32
	if err := binary.Read(rd.r, binary.BigEndian, &v); err != nil {
		return 0, fmt.Errorf("rds is truncated: %w", err)
	}
	return v, nil
}

// length reads a vector length, which needs no more items than are left.
func (rd *rdsReader) length() (int, error) {
	n, err := rd.int()
	if err != nil {
		return 0, err
	}
	if n < 0 {
		return 0, errors.New("rds has a long vector")
	}
	// Each element takes at least four bytes.
	if int64(n)*4 > int64(rd.r.Len()) {
		return 0, errors.New("rds is truncated")
	}
	return int(n), nil
}

func (rd *rdsReader) item(depth int) (any, error) {
	if depth > rdsMaxDepth {
		return nil, errors.New("rds is nested too deeply")
	}
	flags, err := rd.int()
	if err != nil {
		return nil, err
	}
	kind := flags & rdsTypeMask
	hasAttr := flags&rdsHasAttr != 0
	switch kind {
	case rdsNilValue:
		return nil, nil
	case rdsRefSXP:
		index := int(flags >> rdsRefShift)
		if index == 0 {
			i, err := rd.int()
			if err != nil {
				return nil, err
			}
			index = int(i)
		}
		if index < 1 || index > len(rd.refs) {
			return nil, fmt.Errorf("rds refers to unknown item %d", index)
		}
		return rd.refs[index-1], nil
	case rdsSymSXP:
		name, err := rd.item(depth + 1)
		if err != nil {
			return nil, err
		}
		s, ok := name.(*string)
		if !ok || s == nil {
			return nil, errors.New("rds symbol has no name")
		}
		sym := rdsSymbol(*s)
		rd.refs = append(rd.refs, sym)
		return sym, nil
	case rdsCharSXP:
		return rd.charsxp()
	case rdsListSXP:
		return rd.pairlist(flags, depth)
	case rdsStrSXP, rdsIntSXP, rdsLglSXP, rdsVecSXP:
		vec := &rdsVector{}
		if vec.values, err = rd.vector(kind, depth); err != nil {
			return nil, err
		}
		if hasAttr {
			attrs, err := rd.item(depth + 1)
			if err != nil {
				return nil, err
			}
			vec.attrs, _ = attrs.([]rdsAttr)
		}
		return vec, nil
	default:
		return nil, fmt.Errorf("unsupported rds item type %d", kind)
	}
}

func (rd *rdsReader) charsxp() (*string, error) {
	n, err := rd.int()
	if err != nil {
		return nil, err
	}
	if n == -1 {
		return nil, nil // NA_character_
	}
	if n < 0 || int64(n) > int64(rd.r.Len()) {
		return nil, errors.New("rds is truncated")
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(rd.r, b); err != nil {
		return nil, err
	}
	s := string(b)
	return &s, nil
}

func (rd *rdsReader) vector(kind int32, depth int) (any, error) {
	n, err := rd.length()
	if err != nil {
		return nil, err
	}
	switch kind {
	case rdsStrSXP:
		values := make([]*string, n)
		for i := range values {
			flags, err := rd.int()
			if err != nil {
				return nil, err
			}
			if flags&rdsTypeMask != rdsCharSXP {
				return nil, errors.New("rds character vector holds a non-string")
			}
			if values[i], err = rd.charsxp(); err != nil {
				return nil, err
			}
		}
		return values, nil
	case rdsIntSXP, rdsLglSXP:
		values := make([]int32, n)
		for i := range values {
			if values[i], err = rd.int(); err != nil {
				return nil, err
			}
		}
		return values, nil
	default:
		values := make([]any, n)
		for i := range values {
			if values[i], err = rd.item(depth + 1); err != nil {
				return nil, err
			}
		}
		return values, nil
	}
}

// pairlist reads an attribute pairlist, its first cell's flags already
// read, as its tags and values.
func (rd *rdsReader) pairlist(flags int32, depth int) ([]rdsAttr, error) {
	var attrs []rdsAttr
	for {
		if flags&rdsHasAttr != 0 {
			if _, err := rd.item(depth + 1); err != nil {
				return nil, err
			}
		}
		var attr rdsAttr
		if flags&rdsHasTag != 0 {
			tag, err := rd.item(depth + 1)
			if err != nil {
				return nil, err
			}
			attr.tag, _ = tag.(rdsSymbol)
		}
		value, err := rd.item(depth + 1)
		if err != nil {
			return nil, err
		}
		attr.value = value
		attrs = append(attrs, attr)
		// The rest of the list follows as the next cell, or ends.
		if flags, err = rd.int(); err != nil {
			return nil, err
		}
		switch flags & rdsTypeMask {
		case rdsNilValue:
			return attrs, nil
		case rdsListSXP:
		default:
			return nil, errors.New("rds pairlist is malformed")
		}
	}
}
