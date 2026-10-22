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
	"time"
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

// SignedAPK builds an APK from files and inserts a v2 signing block holding certDER.
func SignedAPK(files map[string][]byte, certDER []byte) []byte {
	z := Zip(files)
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
