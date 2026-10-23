package apk

import (
	"archive/zip"
	"errors"
	"io"
)

// ReadManifest reads the manifest of a plain APK on disk.
func ReadManifest(path string) (*Manifest, error) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = zr.Close() }()
	for _, f := range zr.File {
		if f.Name != "AndroidManifest.xml" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		b, err := io.ReadAll(io.LimitReader(rc, 16<<20))
		_ = rc.Close()
		if err != nil {
			return nil, err
		}
		return ParseManifest(b)
	}
	return nil, errors.New("apk: no AndroidManifest.xml")
}
