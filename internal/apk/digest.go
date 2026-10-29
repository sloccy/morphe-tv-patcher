package apk

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
)

// contentIDScheme prefixes every id, so a later change to what goes into the digest can never be
// mistaken for a build that stayed the same.
const contentIDScheme = "1"

// ContentID identifies what an APK installs, ignoring how it was packed and signed: every entry
// with its CRC-32 and uncompressed size, plus the signing certificate. Patching the same input
// with the same patches twice gives the same id even though the two files differ byte for byte,
// because the patcher rebuilds the archive (entry order, compression, timestamps) and signs it
// again every time, which a hash of the whole file would report as a change.
func ContentID(path string) (string, error) {
	fh, err := os.Open(path) //nolint:gosec // an APK path from our own output or cache directory
	if err != nil {
		return "", err
	}
	defer func() { _ = fh.Close() }()
	st, err := fh.Stat()
	if err != nil {
		return "", err
	}
	zr, err := zip.NewReader(fh, st.Size())
	if err != nil {
		return "", err
	}
	lines := make([]string, 0, len(zr.File))
	for _, f := range zr.File {
		if strings.HasSuffix(f.Name, "/") || isSignatureEntry(f.Name) {
			continue
		}
		lines = append(lines, f.Name+"\x00"+strconv.FormatUint(uint64(f.CRC32), 16)+
			"\x00"+strconv.FormatUint(f.UncompressedSize64, 10))
	}
	slices.Sort(lines)
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "apk-content\x00%d\n", len(lines))
	for _, l := range lines {
		_, _ = h.Write([]byte(l))
		_, _ = h.Write([]byte{'\n'})
	}
	// The entries say nothing about the signing key, and an APK signed with another key cannot
	// replace the installed one, so a new keystore has to count as a different build. An APK whose
	// key cannot be read has no identity at all: the caller is meant to treat the error as "do the
	// work", never as "nothing changed".
	signer, err := SignerSHA256(fh, st.Size())
	if err != nil {
		return "", fmt.Errorf("apk: cannot identify the signer: %w", err)
	}
	_, _ = h.Write([]byte("signer\x00" + signer))
	return contentIDScheme + ":" + hex.EncodeToString(h.Sum(nil)), nil
}

// isSignatureEntry says whether an entry is part of the v1 (JAR) signature, which is rewritten on
// every signing and says nothing about what the app installs. The rest of META-INF/ (services/,
// *.kotlin_module, *.version) is content a patch can change, so it counts.
func isSignatureEntry(name string) bool {
	rest, ok := strings.CutPrefix(name, "META-INF/")
	if !ok || strings.Contains(rest, "/") {
		return false
	}
	up := strings.ToUpper(rest)
	return up == "MANIFEST.MF" || strings.HasSuffix(up, ".SF") || strings.HasSuffix(up, ".RSA") ||
		strings.HasSuffix(up, ".DSA") || strings.HasSuffix(up, ".EC")
}
