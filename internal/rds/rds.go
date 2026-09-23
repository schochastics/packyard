// Package rds writes R's serialization format ("RDS").
//
// It exists for exactly one file: src/contrib/Meta/archive.rds, the
// listing of archived package versions that remotes::install_version()
// and friends read with readRDS(). It covers the object types that
// file needs — lists, character/double/integer/logical vectors and
// attributes — and nothing else. There is no reader.
//
// Output is XDR (big-endian) serialization format version 2, which
// every R since 2.3.0 reads and which is what CRAN itself writes for
// archive.rds.
package rds

import (
	"bufio"
	"compress/gzip"
	"encoding/binary"
	"errors"
	"io"
	"math"
)

// errTooLong is returned for vectors R would need its long-vector
// encoding for (more than 2^31-1 elements). archive.rds never gets
// near that.
var errTooLong = errors.New("rds: vector too long for serialization format 2")

// SEXP type codes and flag bits from R's serialize.c / Rinternals.h.
const (
	symSXP   = 1
	listSXP  = 2
	charSXP  = 9
	lglSXP   = 10
	intSXP   = 13
	realSXP  = 14
	strSXP   = 16
	vecSXP   = 19
	refSXP   = 255
	nilValue = 254

	isObjectBit = 1 << 8
	hasAttrBit  = 1 << 9
	hasTagBit   = 1 << 10

	// CHARSXP encoding levels (shifted into bits 12+ of the flags).
	utf8Mask  = 1 << 3
	asciiMask = 1 << 6
)

// Version numbers written into the header, packed the way R's
// R_Version(v, p, s) macro packs them.
var (
	writerVersion     = rVersion(4, 4, 0)
	minReaderVersion  = rVersion(2, 3, 0)
	serializeFormatV2 = int32(2)
)

func rVersion(v, p, s int32) int32 { return v*65536 + p*256 + s }

// Object is a value that can be serialized. Build one with the
// constructors below.
type Object struct {
	kind    int32
	strs    []string
	doubles []float64
	ints    []int32
	elems   []Object
	attrs   []attr
}

type attr struct {
	name  string
	value Object
}

// Strings is a character vector.
func Strings(v ...string) Object { return Object{kind: strSXP, strs: v} }

// Doubles is a double vector.
func Doubles(v ...float64) Object { return Object{kind: realSXP, doubles: v} }

// Ints is an integer vector.
func Ints(v ...int32) Object { return Object{kind: intSXP, ints: v} }

// Logicals is a logical vector.
func Logicals(v ...bool) Object {
	ints := make([]int32, len(v))
	for i, b := range v {
		if b {
			ints[i] = 1
		}
	}
	return Object{kind: lglSXP, ints: ints}
}

// List is a generic vector (R list).
func List(elems ...Object) Object { return Object{kind: vecSXP, elems: elems} }

// WithAttr returns o with attribute name set to value, appended after
// any existing attributes. R preserves attribute order, so callers
// set them in the order R itself would.
func (o Object) WithAttr(name string, value Object) Object {
	o.attrs = append(append([]attr(nil), o.attrs...), attr{name, value})
	return o
}

func (o Object) isObject() bool {
	for _, a := range o.attrs {
		if a.name == "class" {
			return true
		}
	}
	return false
}

// WriteGzip writes obj as a gzip-compressed RDS file, the form
// saveRDS() produces by default.
func WriteGzip(w io.Writer, obj Object) error {
	zw := gzip.NewWriter(w)
	if err := Write(zw, obj); err != nil {
		_ = zw.Close()
		return err
	}
	return zw.Close()
}

// Write writes obj in uncompressed XDR serialization format 2.
func Write(w io.Writer, obj Object) error {
	bw := bufio.NewWriter(w)
	e := &encoder{w: bw, symbols: map[string]int32{}}
	e.bytes([]byte("X\n"))
	e.int(serializeFormatV2)
	e.int(writerVersion)
	e.int(minReaderVersion)
	e.item(obj)
	if e.err != nil {
		return e.err
	}
	return bw.Flush()
}

type encoder struct {
	w   *bufio.Writer
	err error
	// symbols maps a symbol name to its reference-table index. R
	// writes a symbol in full the first time and as a back-reference
	// afterwards; readers expect that.
	symbols map[string]int32
	nextRef int32
}

func (e *encoder) bytes(b []byte) {
	if e.err == nil {
		_, e.err = e.w.Write(b)
	}
}

func (e *encoder) int(v int32) {
	var b [4]byte
	// Two's-complement reinterpretation is the XDR encoding of a
	// signed 32-bit integer (NA_integer_ and REFSXP flags included).
	binary.BigEndian.PutUint32(b[:], uint32(v)) //nolint:gosec // G115: intended bit reinterpretation
	e.bytes(b[:])
}

// length writes a vector length, failing on lengths R would store as
// a long vector.
func (e *encoder) length(n int) {
	if n > math.MaxInt32 {
		if e.err == nil {
			e.err = errTooLong
		}
		return
	}
	e.int(int32(n)) //nolint:gosec // G115: bounded by the MaxInt32 check above
}

func (e *encoder) item(o Object) {
	flags := o.kind
	if len(o.attrs) > 0 {
		flags |= hasAttrBit
	}
	if o.isObject() {
		flags |= isObjectBit
	}
	e.int(flags)
	switch o.kind {
	case strSXP:
		e.length(len(o.strs))
		for _, s := range o.strs {
			e.charsxp(s)
		}
	case realSXP:
		e.length(len(o.doubles))
		var b [8]byte
		for _, d := range o.doubles {
			binary.BigEndian.PutUint64(b[:], math.Float64bits(d))
			e.bytes(b[:])
		}
	case intSXP, lglSXP:
		e.length(len(o.ints))
		for _, v := range o.ints {
			e.int(v)
		}
	case vecSXP:
		e.length(len(o.elems))
		for _, el := range o.elems {
			e.item(el)
		}
	}
	if len(o.attrs) > 0 {
		e.attributes(o.attrs)
	}
}

// attributes writes a tagged pairlist terminated by NILVALUE_SXP.
func (e *encoder) attributes(attrs []attr) {
	for _, a := range attrs {
		e.int(listSXP | hasTagBit)
		e.symbol(a.name)
		e.item(a.value)
	}
	e.int(nilValue)
}

func (e *encoder) symbol(name string) {
	if idx, ok := e.symbols[name]; ok {
		e.int(idx<<8 | refSXP)
		return
	}
	e.nextRef++
	e.symbols[name] = e.nextRef
	e.int(symSXP)
	e.charsxp(name)
}

func (e *encoder) charsxp(s string) {
	level := int32(utf8Mask)
	if isASCII(s) {
		level = asciiMask
	}
	e.int(charSXP | level<<12)
	e.length(len(s))
	e.bytes([]byte(s))
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}
