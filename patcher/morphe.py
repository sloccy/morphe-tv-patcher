import json
import re
import shutil
import subprocess
from pathlib import Path

from .util import log


def patch(cfg, app, mpp: Path, apkm: Path, out: Path, abis: list, patches_ver: str) -> Path:
    result = out.with_suffix(".result.json")
    cmd = ["java", "-jar", cfg.morphe_jar, "patch", "-p", str(mpp),
           "--bytecode-mode", cfg.bytecode_mode,
           "-r", str(result), "-o", str(out)]
    # Morphe errors on a missing --keystore path; with no flag it creates its own default
    # next to the jar. So: reuse our persisted copy if present, else let it create one and adopt it.
    have_keystore = cfg.keystore.exists()
    if have_keystore:
        cmd += ["--keystore", str(cfg.keystore)]
    if abis:
        cmd += ["--striplibs", ",".join(abis)]
    if app.options_file:
        cmd += ["--options-file", app.options_file]
    for p in app.enable:
        cmd += ["-e", p]
    for p in app.disable:
        cmd += ["-d", p]
    cmd.append(str(apkm))
    log.info("running: %s", " ".join(cmd))
    res = subprocess.run(cmd, capture_output=True, text=True)
    log.info(res.stdout[-3000:])
    if res.returncode == 0 and not have_keystore:
        _adopt_keystore(cfg, res.stdout + res.stderr)
    if res.returncode != 0:
        raise RuntimeError(f"morphe exit {res.returncode}: {res.stderr[-1500:] or res.stdout[-1500:]}")
    if not out.exists():
        raise RuntimeError("morphe produced no output apk")
    _check_result(result)
    return out


def _adopt_keystore(cfg, output: str):
    """Copy Morphe's auto-created keystore into the persistent volume."""
    m = re.search(r"Morphe data root: (.+?) \(", output)
    root = Path(m.group(1)) if m else Path(cfg.morphe_jar).parent / "morphe-data"
    src = root / "morphe.keystore"
    if not src.exists():
        raise RuntimeError(f"patched, but Morphe's keystore not found at {src}; "
                           "the next run would sign with a different key")
    cfg.keystore.parent.mkdir(parents=True, exist_ok=True)
    shutil.copy2(src, cfg.keystore)
    log.warning("new signing keystore saved to %s. BACK IT UP: losing it means "
                "uninstalling each app before it can be updated", cfg.keystore)


def _check_result(result: Path):
    """Fail on anything but a clean result.json (success true, no failedPatches)."""
    if not result.exists():
        raise RuntimeError("morphe wrote no result file")
    data = json.loads(result.read_text())
    if not data.get("success"):
        raise RuntimeError(f"patching failed: {data.get('failureReason') or data.get('failureDetail')}")
    failed = data.get("failedPatches") or []
    if failed:
        raise RuntimeError(f"patches failed: {json.dumps(failed)[:400]}")
