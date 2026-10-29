// Package apktest builds small synthetic APKs and bundles for tests: real binary manifests
// with a generated certificate in a genuine v2 signing block.
package apktest

import (
	"archive/zip"
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/hex"
	"math/big"
	"sort"
	"strings"
	"time"
	"unicode/utf16"
)

func lp(b ...[]byte) []byte {
	var out []byte
	for _, e := range b {
		out = binary.LittleEndian.AppendUint32(out, uint32(len(e))) //nolint:gosec // test helper with tiny inputs
		out = append(out, e...)
	}
	return out
}

// Cert returns a fresh self-signed certificate (DER) and its SHA-256 hex.
func Cert(cn string) (der []byte, fingerprint string) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	t := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour)}
	var err error
	der, err = x509.CreateCertificate(rand.Reader, t, t, &key.PublicKey, key)
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(der)
	return der, hex.EncodeToString(sum[:])
}

// Zip builds a zip archive with the given entries (sorted by name).
func Zip(files map[string][]byte) []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		w, _ := zw.Create(n)
		_, _ = w.Write(files[n])
	}
	_ = zw.Close()
	return buf.Bytes()
}

// Entry is one archive member. ZipEntries keeps the given order and allows a repeated name, which
// the map form cannot express.
type Entry struct {
	Name string
	Data []byte
}

// ZipEntries builds a zip archive with the given entries, in order, duplicate names included.
func ZipEntries(entries ...Entry) []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		w, _ := zw.Create(e.Name)
		_, _ = w.Write(e.Data)
	}
	_ = zw.Close()
	return buf.Bytes()
}

// SignedAPK builds an APK from files and inserts a v2 signing block holding certDER.
func SignedAPK(files map[string][]byte, certDER []byte) []byte {
	return SignZip(Zip(files), certDER)
}

// SignZip inserts a v2 signing block holding certDER into an existing archive, for tests that
// build the archive themselves to control entry order, compression or timestamps.
func SignZip(z, certDER []byte) []byte {
	if certDER == nil {
		return z
	}
	signer := lp(lp(lp(), lp(certDER), lp()), lp(), lp())
	value := lp(lp(signer))
	pair := binary.LittleEndian.AppendUint64(nil, uint64(4+len(value)))
	pair = binary.LittleEndian.AppendUint32(pair, 0x7109871a)
	pair = append(pair, value...)
	block := binary.LittleEndian.AppendUint64(nil, uint64(len(pair)+24))
	block = append(block, pair...)
	block = binary.LittleEndian.AppendUint64(block, uint64(len(pair)+24))
	block = append(block, "APK Sig Block 42"...)

	eocd := bytes.LastIndex(z, []byte{0x50, 0x4b, 0x05, 0x06})
	cd := int(binary.LittleEndian.Uint32(z[eocd+16:]))
	out := append([]byte{}, z[:cd]...)
	out = append(out, block...)
	out = append(out, z[cd:]...)
	binary.LittleEndian.PutUint32(out[eocd+len(block)+16:], uint32(cd+len(block))) //nolint:gosec // test helper with tiny inputs
	return out
}

// Manifest builds a small binary AndroidManifest.xml: the package and version code, some
// <uses-permission> elements, and receiver/service/activity elements inside <application>.
// It is enough for the readers in package apk and not a general encoder.
func Manifest(pkg string, versionCode uint32, permissions, components []string) []byte {
	const androidNS = "http://schemas.android.com/apk/res/android"
	var strs []string
	idx := map[string]uint32{}
	id := func(s string) uint32 {
		if i, ok := idx[s]; ok {
			return i
		}
		idx[s] = uint32(len(strs)) //nolint:gosec // test helper with tiny inputs
		strs = append(strs, s)
		return idx[s]
	}
	const none = 0xFFFFFFFF
	u32 := func(v ...uint32) []byte {
		b := make([]byte, 0, 4*len(v))
		for _, x := range v {
			b = binary.LittleEndian.AppendUint32(b, x)
		}
		return b
	}
	attr := func(ns uint32, name string, str string, num uint32, isStr bool) []byte {
		b := u32(ns, id(name))
		if isStr {
			b = append(b, u32(id(str))...)
			b = append(b, 8, 0, 0, 3) // size, res0, type string
			return append(b, u32(id(str))...)
		}
		b = append(b, u32(none)...)
		b = append(b, 8, 0, 0, 0x10) // size, res0, type int dec
		return append(b, u32(num)...)
	}
	chunk := func(typ uint16, hdr uint16, body []byte) []byte {
		b := binary.LittleEndian.AppendUint16(nil, typ)
		b = binary.LittleEndian.AppendUint16(b, hdr)
		b = binary.LittleEndian.AppendUint32(b, uint32(len(body))+uint32(hdr)) //nolint:gosec // test helper with tiny inputs
		return b
	}
	var elems []byte
	start := func(name string, attrs ...[]byte) {
		body := u32(none, id(name))
		body = binary.LittleEndian.AppendUint16(body, 20)
		body = binary.LittleEndian.AppendUint16(body, 20)
		body = binary.LittleEndian.AppendUint16(body, uint16(len(attrs))) //nolint:gosec // test helper with tiny inputs
		body = append(body, 0, 0, 0, 0, 0, 0)
		for _, a := range attrs {
			body = append(body, a...)
		}
		h := chunk(0x0102, 16, body)
		h = append(h, u32(1, none)...) // line number, comment
		elems = append(elems, h...)
		elems = append(elems, body...)
	}
	end := func(name string) {
		body := u32(none, id(name))
		h := chunk(0x0103, 16, body)
		h = append(h, u32(1, none)...)
		elems = append(elems, h...)
		elems = append(elems, body...)
	}
	ans := id(androidNS)
	start("manifest", attr(none, "package", pkg, 0, true), attr(ans, "versionCode", "", versionCode, false))
	for _, p := range permissions {
		start("uses-permission", attr(ans, "name", p, 0, true))
		end("uses-permission")
	}
	start("application")
	for _, c := range components {
		kind := "activity"
		switch {
		case strings.HasSuffix(c, "Receiver"):
			kind = "receiver"
		case strings.HasSuffix(c, "Job"):
			kind = "service"
		}
		start(kind, attr(ans, "name", c, 0, true))
		end(kind)
	}
	end("application")
	end("manifest")

	// string pool (UTF-16)
	var pool, offs []byte
	for _, s := range strs {
		offs = binary.LittleEndian.AppendUint32(offs, uint32(len(pool))) //nolint:gosec // test helper with tiny inputs
		u := utf16.Encode([]rune(s))
		pool = binary.LittleEndian.AppendUint16(pool, uint16(len(u))) //nolint:gosec // test helper with tiny inputs
		for _, c := range u {
			pool = binary.LittleEndian.AppendUint16(pool, c)
		}
		pool = append(pool, 0, 0)
	}
	for len(pool)%4 != 0 {
		pool = append(pool, 0)
	}
	ph := chunk(0x0001, 28, append(offs, pool...))
	ph = append(ph, u32(uint32(len(strs)), 0, 0, uint32(28+len(offs)), 0)...) //nolint:gosec // test helper with tiny inputs
	sp := append(append(ph, offs...), pool...)

	out := binary.LittleEndian.AppendUint16(nil, 0x0003)
	out = binary.LittleEndian.AppendUint16(out, 8)
	out = binary.LittleEndian.AppendUint32(out, uint32(8+len(sp)+len(elems))) //nolint:gosec // test helper with tiny inputs
	return append(append(out, sp...), elems...)
}
