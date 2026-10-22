package apk

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
)

// Signing block pair IDs (APK Signature Scheme v2, v3, v3.1).
const (
	idV2  = 0x7109871a
	idV3  = 0xf05368c0
	idV31 = 0x1b93ad61
)

var sigMagic = []byte("APK Sig Block 42")

// ErrNoSigningBlock means the APK has no v2/v3 signature (a v1-only APK).
var ErrNoSigningBlock = errors.New("apk: no v2/v3 signing block")

// SignerSHA256 returns the hex SHA-256 of the first signer's certificate (DER) from the
// APK Signing Block. It reads the certificate; cryptographic verification is apksigner's job.
func SignerSHA256(r io.ReaderAt, size int64) (string, error) {
	cdOff, err := centralDirOffset(r, size)
	if err != nil {
		return "", err
	}
	if cdOff < 32 {
		return "", ErrNoSigningBlock
	}
	tail := make([]byte, 24)
	if _, err := r.ReadAt(tail, cdOff-24); err != nil {
		return "", err
	}
	if !bytes.Equal(tail[8:], sigMagic) {
		return "", ErrNoSigningBlock
	}
	blockSize := int64(binary.LittleEndian.Uint64(tail))
	start := cdOff - blockSize - 8
	if blockSize < 24 || start < 0 || blockSize > 64<<20 {
		return "", fmt.Errorf("apk: bad signing block size %d", blockSize)
	}
	pairs := make([]byte, blockSize-24)
	if _, err := r.ReadAt(pairs, start+8); err != nil {
		return "", err
	}
	// Prefer the newest scheme present.
	var v2, v3 []byte
	for len(pairs) >= 12 {
		l := binary.LittleEndian.Uint64(pairs)
		if l < 4 || l > uint64(len(pairs)-8) {
			return "", errTruncated
		}
		id := binary.LittleEndian.Uint32(pairs[8:])
		val := pairs[12 : 8+l]
		switch id {
		case idV3, idV31:
			if v3 == nil {
				v3 = val
			}
		case idV2:
			v2 = val
		}
		pairs = pairs[8+l:]
	}
	for _, c := range []struct {
		val []byte
		v3  bool
	}{{v3, true}, {v2, false}} {
		if c.val == nil {
			continue
		}
		der, err := firstCertificate(c.val)
		if err != nil {
			return "", err
		}
		sum := sha256.Sum256(der)
		return hex.EncodeToString(sum[:]), nil
	}
	return "", ErrNoSigningBlock
}

// lenPrefixed returns the u32-length-prefixed element at the start of b and the rest.
func lenPrefixed(b []byte) (elem, rest []byte, err error) {
	if len(b) < 4 {
		return nil, nil, errTruncated
	}
	n := int(binary.LittleEndian.Uint32(b))
	if n < 0 || 4+n > len(b) {
		return nil, nil, errTruncated
	}
	return b[4 : 4+n], b[4+n:], nil
}

// firstCertificate walks signers -> first signer -> signed data -> certificates -> first.
// Both v2 and v3 lay out signed data as: digests, certificates, ...
func firstCertificate(val []byte) ([]byte, error) {
	signers, _, err := lenPrefixed(val)
	if err != nil {
		return nil, err
	}
	signer, _, err := lenPrefixed(signers)
	if err != nil {
		return nil, err
	}
	signed, _, err := lenPrefixed(signer)
	if err != nil {
		return nil, err
	}
	_, rest, err := lenPrefixed(signed) // digests
	if err != nil {
		return nil, err
	}
	certs, _, err := lenPrefixed(rest)
	if err != nil {
		return nil, err
	}
	der, _, err := lenPrefixed(certs)
	if err != nil {
		return nil, err
	}
	if len(der) == 0 {
		return nil, errors.New("apk: empty certificate")
	}
	return der, nil
}

// centralDirOffset finds the zip end-of-central-directory record and returns where the
// central directory starts (the APK Signing Block, if any, ends right before it).
func centralDirOffset(r io.ReaderAt, size int64) (int64, error) {
	const eocdMin = 22
	n := int64(1 << 16)
	if size < n+eocdMin {
		n = size
	}
	buf := make([]byte, n)
	if _, err := r.ReadAt(buf, size-n); err != nil && !errors.Is(err, io.EOF) {
		return 0, err
	}
	i := bytes.LastIndex(buf, []byte{0x50, 0x4b, 0x05, 0x06})
	if i < 0 || i+eocdMin > len(buf) {
		return 0, errors.New("apk: zip end record not found")
	}
	return int64(binary.LittleEndian.Uint32(buf[i+16:])), nil
}
