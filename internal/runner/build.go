package runner

// Deciding whether a build has to be made, and whether its result has to be installed. A patches
// release usually changes the patches of one or two apps, but its tag changes for all of them, so
// the tag alone cannot say whether an app really needs anything done to it. The answer comes from
// two identities: a build key (what goes into patching) and apk.ContentID (what came out of it).

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash/fnv"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/sloccy/morphe-tv-patcher/internal/apk"
	"github.com/sloccy/morphe-tv-patcher/internal/store"
)

// outputPath is where the patched APK for one app, version, patches release, CPU list and patch
// source goes. The short hash keeps builds that differ only by CPU list or patch source apart.
func (r *Runner) outputPath(pkg, version, tag, abiKey, srcID string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(abiKey + "|" + srcID))
	return filepath.Join(r.Cfg.Output(), fmt.Sprintf("%s-%s-%s-%s.apk",
		pkg, safe(version), safe(tag), strconv.FormatUint(uint64(h.Sum32()), 16)))
}

// buildKey identifies one patching job by everything that decides its result: the app and version,
// the file being patched, the patch bundle, the CPU list, and the patch options. Two jobs with the
// same key produce the same APK, so the first one's result can be reused. The version is kept in
// the clear at the front so dropOld can forget the keys of a version it dropped.
func (r *Runner) buildKey(pkg, version, srcID, mpp, abiKey, input string, ov store.Override) string {
	h := sha256.New()
	add := func(parts ...string) {
		for _, p := range parts {
			_, _ = h.Write([]byte(p))
			_, _ = h.Write([]byte{0})
		}
	}
	add("package", pkg, "version", version, "source", srcID, "abi", abiKey, "bytecode", r.Cfg.BytecodeMode)
	add("input", fileID(input), "bundle", fileID(mpp), "keystore", fileID(r.Cfg.Keystore()))
	// The patch selection decides what is applied, so it belongs here; the order it is given in
	// does not. The options file counts by what is in it, not where it is.
	add("enable", strings.Join(sorted(ov.Enable), ","), "disable", strings.Join(sorted(ov.Disable), ","))
	add("options", fileID(ov.OptionsFile))
	return version + "|" + hex.EncodeToString(h.Sum(nil))
}

// fileID is a cheap identity for a file that is replaced as a whole when it changes: its path,
// size and modification time. Reading a 100 MB bundle and APK on every pass would cost more than
// the rebuild it saves, and a file swapped for a different one of exactly the same size within the
// same nanosecond is not a case this has to survive.
func fileID(path string) string {
	if path == "" {
		return ""
	}
	st, err := os.Stat(path)
	if err != nil {
		return path + "|missing"
	}
	return path + "|" + strconv.FormatInt(st.Size(), 10) + "|" + strconv.FormatInt(st.ModTime().UnixNano(), 10)
}

func sorted(s []string) []string {
	out := slices.Clone(s)
	slices.Sort(out)
	return out
}

// reuse says whether the APK this build key produced before is still on disk unchanged, so that
// patching can be skipped altogether. It returns that APK's ContentID, or "".
func (r *Runner) reuse(pkg, buildKey, out string) string {
	want := r.Store.PatchedID(pkg, buildKey)
	if want == "" {
		return ""
	}
	got, err := apk.ContentID(out)
	if err != nil || got != want {
		return "" // deleted by the cleanup, or not the file we built
	}
	return want
}

// contentID identifies a patched APK. A file we cannot read the identity of is not an error: it is
// simply installed, as it was before any of this existed.
func contentID(pkg, out string) string {
	id, err := apk.ContentID(out)
	if err != nil {
		slog.Warn("could not identify the patched apk, so it will be installed", "package", pkg,
			"file", filepath.Base(out), "err", err)
		return ""
	}
	return id
}

// stillThere says whether the app a record describes is really on the TV. It never connects: a
// run that is not connected to this TV goes by the record, exactly as the up-to-date check does.
func (r *Runner) stillThere(ctx context.Context, c *conn, in store.Install, pkg string) bool {
	if !c.connected {
		return true
	}
	ip := in.InstalledPackage
	if ip == "" {
		ip = pkg
	}
	v, _ := r.ADB.InstalledVersion(ctx, c.serial, ip)
	return v != ""
}

// An install recorded before any of this existed has no ContentID, and there is no safe way to
// work one out afterwards: the patched file it was made from is still in the output folder, but its
// name says nothing about the patch options or the bytecode mode it was built with, so a dry run
// with changed settings may since have overwritten it with a build the TV never received. Such a
// record simply does not match, the app is installed once as it always was, and from then on its
// identity is known.
