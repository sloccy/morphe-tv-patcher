package apk

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sloccy/morphe-tv-patcher/internal/apk/apktest"
)

// One key per test run: Cert makes a fresh one on every call, and a different key is meant to give
// a different id.
var testCert, _ = apktest.Cert("tv-patcher test")

// write puts b in a temporary file and returns its path.
func write(t *testing.T, name string, b []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func id(t *testing.T, name string, b []byte) string {
	t.Helper()
	got, err := ContentID(write(t, name, b))
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return got
}

// signedFiles is an APK of these files, signed with the test key.
func signedFiles(files map[string][]byte) []byte { return apktest.SignedAPK(files, testCert) }

// repack writes the same entries in the given order, each with its own modification time and
// compression, the way two runs of the patcher over the same input may differ.
func repack(entries []apktest.Entry, when time.Time, store bool) []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		h := &zip.FileHeader{Name: e.Name, Modified: when, Method: zip.Deflate}
		if store {
			h.Method = zip.Store
		}
		w, _ := zw.CreateHeader(h)
		_, _ = w.Write(e.Data)
	}
	_ = zw.Close()
	return apktest.SignZip(buf.Bytes(), testCert)
}

var files = []apktest.Entry{
	{Name: "AndroidManifest.xml", Data: []byte("manifest")},
	{Name: "classes.dex", Data: []byte("dex one")},
	{Name: "res/layout/main.xml", Data: []byte("layout")},
}

// TestContentIDIgnoresPackaging is the whole point of the digest: the patcher rebuilds the archive
// every run, so a hash of the file itself would report a change that is not there.
func TestContentIDIgnoresPackaging(t *testing.T) {
	a := repack(files, time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), false)
	reordered := []apktest.Entry{files[2], files[0], files[1]}
	b := repack(reordered, time.Date(2019, 9, 8, 7, 6, 5, 0, time.UTC), true)
	if bytes.Equal(a, b) {
		t.Fatal("the two archives are meant to differ byte for byte")
	}
	if id(t, "a.apk", a) != id(t, "b.apk", b) {
		t.Error("entry order, modification time or compression changed the id")
	}
}

// TestContentIDSkipsOnlyTheSignatureFiles: the rest of META-INF/ is content a patch can change.
func TestContentIDSkipsOnlyTheSignatureFiles(t *testing.T) {
	plain := map[string][]byte{"AndroidManifest.xml": []byte("manifest"), "classes.dex": []byte("dex one")}
	want := id(t, "plain.apk", signedFiles(plain))

	v1 := map[string][]byte{"AndroidManifest.xml": []byte("manifest"), "classes.dex": []byte("dex one"),
		"META-INF/MANIFEST.MF": []byte("Manifest-Version: 1.0"), "META-INF/CERT.SF": []byte("sf"),
		"META-INF/CERT.RSA": []byte("rsa")}
	if id(t, "v1.apk", signedFiles(v1)) != want {
		t.Error("the v1 signature files must not count towards the id")
	}

	content := map[string][]byte{"AndroidManifest.xml": []byte("manifest"), "classes.dex": []byte("dex one"),
		"META-INF/services/x": []byte("a service the app really loads")}
	if id(t, "content.apk", signedFiles(content)) == want {
		t.Error("META-INF/services/ is content, not a signature, and must count")
	}
}

func TestContentIDFollowsContent(t *testing.T) {
	base := repack(files, time.Time{}, false)
	changed := repack([]apktest.Entry{files[0], {Name: "classes.dex", Data: []byte("dex two")}, files[2]}, time.Time{}, false)
	added := repack(append(append([]apktest.Entry{}, files...),
		apktest.Entry{Name: "lib/armeabi-v7a/libx.so", Data: []byte("so")}), time.Time{}, false)
	renamed := repack([]apktest.Entry{files[0], {Name: "classes2.dex", Data: files[1].Data}, files[2]}, time.Time{}, false)

	want := id(t, "base.apk", base)
	for name, b := range map[string][]byte{"changed.apk": changed, "added.apk": added, "renamed.apk": renamed} {
		if id(t, name, b) == want {
			t.Errorf("%s: a different app got the same id", name)
		}
	}
}

// TestContentIDFollowsSigner matters because an APK signed with another key cannot replace the
// installed one, so it must never be taken for the same build.
func TestContentIDFollowsSigner(t *testing.T) {
	plain := map[string][]byte{"AndroidManifest.xml": []byte("manifest"), "classes.dex": []byte("dex one")}
	other, _ := apktest.Cert("another key")
	withTest := id(t, "a.apk", signedFiles(plain))
	if withTest == id(t, "b.apk", apktest.SignedAPK(plain, other)) {
		t.Error("two signing keys gave the same id")
	}
	if again := id(t, "a2.apk", signedFiles(plain)); again != withTest {
		t.Error("the same key gave two different ids")
	}
}

// TestContentIDRejectsWhatItCannotIdentify: every error has to mean "do the work", so an APK with
// no readable signer must not come back with an id that ignores the key.
func TestContentIDRejectsWhatItCannotIdentify(t *testing.T) {
	plain := map[string][]byte{"AndroidManifest.xml": []byte("manifest")}
	for name, b := range map[string][]byte{
		"unsigned.apk": apktest.Zip(plain),         // v1-only: no signing block to read
		"garbage.apk":  []byte("not a zip at all"), //
	} {
		if _, err := ContentID(write(t, name, b)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	if _, err := ContentID(filepath.Join(t.TempDir(), "nothing.apk")); err == nil {
		t.Error("a missing file should fail")
	}
}
