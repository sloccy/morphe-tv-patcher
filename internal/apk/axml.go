// Package apk reads what the patcher needs from Android packages: the manifest facts
// (package, version, Android TV launcher) and the signing certificate, using only the
// standard library.
package apk

import (
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"unicode/utf16"
)

// Manifest holds the facts read from a binary AndroidManifest.xml.
type Manifest struct {
	Package     string
	VersionName string
	VersionCode uint32
	// Leanback is true when some component declares the LEANBACK_LAUNCHER category,
	// i.e. the app has an Android TV launcher entry. A phone build does not.
	Leanback bool
	// Features lists <uses-feature android:name=...>.
	Features []string
}

const (
	chunkStringPool = 0x0001
	chunkXML        = 0x0003
	chunkStartElem  = 0x0102
	chunkEndElem    = 0x0103

	typeString = 0x03
	typeIntDec = 0x10
	typeIntHex = 0x11

	leanbackCategory = "android.intent.category.LEANBACK_LAUNCHER"
	androidNS        = "http://schemas.android.com/apk/res/android"
	noIndex          = 0xFFFFFFFF
)

var errTruncated = errors.New("apk: truncated binary xml")

// ParseManifest decodes a compiled (binary) AndroidManifest.xml.
func ParseManifest(data []byte) (*Manifest, error) {
	if len(data) < 8 || binary.LittleEndian.Uint16(data) != chunkXML {
		return nil, errors.New("apk: not a binary xml document")
	}
	total := min(int(binary.LittleEndian.Uint32(data[4:])), len(data))
	var (
		strs  []string
		m     = &Manifest{}
		stack []string
		pos   = int(binary.LittleEndian.Uint16(data[2:]))
		haveM bool
	)
	for pos+8 <= total {
		typ := binary.LittleEndian.Uint16(data[pos:])
		hdr := int(binary.LittleEndian.Uint16(data[pos+2:]))
		size := int(binary.LittleEndian.Uint32(data[pos+4:]))
		if size < 8 || pos+size > total {
			return nil, errTruncated
		}
		chunk := data[pos : pos+size]
		switch typ {
		case chunkStringPool:
			var err error
			if strs, err = parseStringPool(chunk); err != nil {
				return nil, err
			}
		case chunkStartElem:
			name, attrs, err := parseStartElement(chunk, hdr, strs)
			if err != nil {
				return nil, err
			}
			stack = append(stack, name)
			collect(m, name, stack, attrs)
			haveM = haveM || name == "manifest"
		case chunkEndElem:
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		}
		pos += size
	}
	if !haveM || m.Package == "" {
		return nil, errors.New("apk: no <manifest> package found")
	}
	return m, nil
}

type attr struct {
	ns, name string
	value    string
	isString bool
	num      uint32
}

func collect(m *Manifest, name string, stack []string, attrs []attr) {
	get := func(n string) (attr, bool) {
		for _, a := range attrs {
			if a.name == n {
				return a, true
			}
		}
		return attr{}, false
	}
	switch name {
	case "manifest":
		if a, ok := get("package"); ok {
			m.Package = a.value
		}
		if a, ok := get("versionName"); ok && a.ns == androidNS && a.isString {
			m.VersionName = a.value
		}
		if a, ok := get("versionCode"); ok && a.ns == androidNS {
			m.VersionCode = a.num
		}
	case "uses-feature":
		if a, ok := get("name"); ok && a.isString {
			m.Features = append(m.Features, a.value)
		}
	case "category":
		if a, ok := get("name"); ok && a.value == leanbackCategory && inIntentFilter(stack) {
			m.Leanback = true
		}
	}
}

func inIntentFilter(stack []string) bool {
	return slices.Contains(stack, "intent-filter")
}

func parseStringPool(c []byte) ([]string, error) {
	if len(c) < 28 {
		return nil, errTruncated
	}
	count := int(binary.LittleEndian.Uint32(c[8:]))
	flags := binary.LittleEndian.Uint32(c[16:])
	start := int(binary.LittleEndian.Uint32(c[20:]))
	utf8 := flags&0x100 != 0
	hdr := int(binary.LittleEndian.Uint16(c[2:]))
	if count < 0 || hdr+4*count > len(c) || start > len(c) {
		return nil, errTruncated
	}
	out := make([]string, count)
	for i := range count {
		off := start + int(binary.LittleEndian.Uint32(c[hdr+4*i:]))
		if off >= len(c) {
			return nil, errTruncated
		}
		s, err := readString(c[off:], utf8)
		if err != nil {
			return nil, err
		}
		out[i] = s
	}
	return out, nil
}

func readString(b []byte, utf8 bool) (string, error) {
	if utf8 {
		_, n1 := varLen8(b)
		l8, n2 := varLen8(b[n1:])
		s := n1 + n2
		if s+l8 > len(b) {
			return "", errTruncated
		}
		return string(b[s : s+l8]), nil
	}
	if len(b) < 2 {
		return "", errTruncated
	}
	l := int(binary.LittleEndian.Uint16(b))
	s := 2
	if l&0x8000 != 0 {
		if len(b) < 4 {
			return "", errTruncated
		}
		l = (l&0x7FFF)<<16 | int(binary.LittleEndian.Uint16(b[2:]))
		s = 4
	}
	if s+2*l > len(b) {
		return "", errTruncated
	}
	u := make([]uint16, l)
	for i := range u {
		u[i] = binary.LittleEndian.Uint16(b[s+2*i:])
	}
	return string(utf16.Decode(u)), nil
}

func varLen8(b []byte) (length, size int) {
	if len(b) == 0 {
		return 0, 1
	}
	if b[0]&0x80 != 0 && len(b) > 1 {
		return int(b[0]&0x7F)<<8 | int(b[1]), 2
	}
	return int(b[0]), 1
}

func str(strs []string, idx uint32) string {
	if idx == noIndex || int(idx) >= len(strs) {
		return ""
	}
	return strs[idx]
}

func parseStartElement(c []byte, hdr int, strs []string) (string, []attr, error) {
	if len(c) < hdr+20 {
		return "", nil, errTruncated
	}
	body := c[hdr:]
	name := str(strs, binary.LittleEndian.Uint32(body[4:]))
	attrStart := int(binary.LittleEndian.Uint16(body[8:]))
	attrSize := int(binary.LittleEndian.Uint16(body[10:]))
	attrCount := int(binary.LittleEndian.Uint16(body[12:]))
	if attrSize < 20 {
		return "", nil, fmt.Errorf("apk: bad attribute size %d", attrSize)
	}
	attrs := make([]attr, 0, attrCount)
	for i := range attrCount {
		o := attrStart + i*attrSize
		if o+20 > len(body) {
			return "", nil, errTruncated
		}
		a := body[o : o+20]
		at := attr{
			ns:   str(strs, binary.LittleEndian.Uint32(a[0:])),
			name: str(strs, binary.LittleEndian.Uint32(a[4:])),
		}
		raw := binary.LittleEndian.Uint32(a[8:])
		dt := a[15]
		data := binary.LittleEndian.Uint32(a[16:])
		switch {
		case raw != noIndex:
			at.value, at.isString = str(strs, raw), true
		case dt == typeString:
			at.value, at.isString = str(strs, data), true
		case dt == typeIntDec || dt == typeIntHex:
			at.num = data
			at.value = strconv.FormatUint(uint64(data), 10)
		default:
			at.num = data // references and other types are not literal values
		}
		attrs = append(attrs, at)
	}
	return name, attrs, nil
}
