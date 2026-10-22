package apk

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
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Values recorded from the real APKs (cross-checked with androguard and apksigner).
var real = []struct {
	file, pkg, version, signer string
}{
	{"prime", "com.amazon.amazonvideo.livingroom", "6.23.23+v15.5.0.70-armv7a",
		"2f19adeb284eb36f7f07786152b9a1d14b21653203ad0b04ebbf9c73ab6d7625"},
	{"netflix", "com.netflix.ninja", "13.0.1 build 25028",
		"363863596ea99241eb71b1a985553aa604de3ea3c5f0c546742390e682164e6b"},
	{"disney", "com.disney.disneyplus", "26.16.0+rc2-2026.09.08",
		"b4d251e979ea974d8f070feb5ab69fc4c2873f4e8add421ff63ad5f2789d63d4"},
}

func TestParseManifestRealApps(t *testing.T) {
	for _, tc := range real {
		data, err := os.ReadFile(filepath.Join("testdata", tc.file+".AndroidManifest.xml"))
		if err != nil {
			t.Fatal(err)
		}
		m, err := ParseManifest(data)
		if err != nil {
			t.Fatalf("%s: %v", tc.file, err)
		}
		if m.Package != tc.pkg || m.VersionName != tc.version {
			t.Errorf("%s: got %q %q", tc.file, m.Package, m.VersionName)
		}
		if !m.Leanback {
			t.Errorf("%s: Android TV launcher not detected", tc.file)
		}
		if m.VersionCode == 0 {
			t.Errorf("%s: version code missing", tc.file)
		}
	}
}

func TestParseManifestRejectsGarbage(t *testing.T) {
	for _, in := range [][]byte{nil, []byte("hello"), {0x03, 0x00, 0x08, 0x00, 0xff, 0xff, 0xff, 0x7f}} {
		if _, err := ParseManifest(in); err == nil {
			t.Errorf("expected an error for %v", in)
		}
	}
	data, _ := os.ReadFile(filepath.Join("testdata", "prime.AndroidManifest.xml"))
	if _, err := ParseManifest(data[:len(data)/2]); err == nil {
		t.Error("truncated manifest should fail, not be trusted")
	}
}

// TestSignerRealAPKs runs against the real base.apk files when APK_REAL_DIR points at them.
func TestSignerRealAPKs(t *testing.T) {
	dir := os.Getenv("APK_REAL_DIR")
	if dir == "" {
		t.Skip("set APK_REAL_DIR to a folder of *.base.apk files")
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "*.base.apk"))
	if len(matches) == 0 {
		t.Skip("no base.apk files")
	}
	for _, f := range matches {
		fh, err := os.Open(f)
		if err != nil {
			t.Fatal(err)
		}
		st, _ := fh.Stat()
		got, err := SignerSHA256(fh, st.Size())
		fh.Close()
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		found := false
		for _, tc := range real {
			if got == tc.signer {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: unexpected signer %s", f, got)
		}
	}
}

func lp(b ...[]byte) []byte { // concatenated u32-length-prefixed elements
	var out []byte
	for _, e := range b {
		out = binary.LittleEndian.AppendUint32(out, uint32(len(e)))
		out = append(out, e...)
	}
	return out
}

func syntheticAPK(t *testing.T, id uint32) ([]byte, string) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test"},
		NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	signed := lp(lp(), lp(der), lp())
	signer := lp(signed, lp(), lp())
	value := lp(lp(signer)) // signers sequence, each signer length-prefixed
	pair := binary.LittleEndian.AppendUint64(nil, uint64(4+len(value)))
	pair = binary.LittleEndian.AppendUint32(pair, id)
	pair = append(pair, value...)
	block := binary.LittleEndian.AppendUint64(nil, uint64(len(pair)+24))
	block = append(block, pair...)
	block = binary.LittleEndian.AppendUint64(block, uint64(len(pair)+24))
	block = append(block, sigMagic...)

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("AndroidManifest.xml")
	w.Write([]byte("x"))
	zw.Close()
	z := buf.Bytes()
	eocd := bytes.LastIndex(z, []byte{0x50, 0x4b, 0x05, 0x06})
	cd := int(binary.LittleEndian.Uint32(z[eocd+16:]))
	out := append([]byte{}, z[:cd]...)
	out = append(out, block...)
	out = append(out, z[cd:]...)
	binary.LittleEndian.PutUint32(out[eocd+len(block)+16:], uint32(cd+len(block)))
	sum := sha256.Sum256(der)
	return out, hex.EncodeToString(sum[:])
}

func TestSignerSyntheticV2V3(t *testing.T) {
	for _, id := range []uint32{idV2, idV3, idV31} {
		apkBytes, want := syntheticAPK(t, id)
		got, err := SignerSHA256(bytes.NewReader(apkBytes), int64(len(apkBytes)))
		if err != nil || got != want {
			t.Errorf("id %#x: got %q err %v, want %q", id, got, err, want)
		}
	}
}

func TestSignerV1OnlyAndGarbage(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("a")
	w.Write([]byte("x"))
	zw.Close()
	if _, err := SignerSHA256(bytes.NewReader(buf.Bytes()), int64(buf.Len())); err != ErrNoSigningBlock {
		t.Errorf("v1-only apk: got %v", err)
	}
	if _, err := SignerSHA256(bytes.NewReader([]byte("nope")), 4); err == nil {
		t.Error("garbage should error")
	}
}
