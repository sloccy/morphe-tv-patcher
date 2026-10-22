import json
import subprocess
from pathlib import Path

from .util import log


def patch(cfg, app, mpp: Path, apkm: Path, out: Path, abis: list, patches_ver: str) -> Path:
    result = out.with_suffix(".result.json")
    cmd = ["java", "-jar", cfg.morphe_jar, "patch", "-p", str(mpp),
           "--bytecode-mode", cfg.bytecode_mode,
           "--keystore", str(cfg.keystore),
           "-r", str(result), "-o", str(out)]
    if abis:
        cmd += ["--striplibs", ",".join(abis)]
    if app.options_file:
        cmd += ["--options-file", app.options_file]
    for p in app.enable:
        cmd += ["-e", p]
    for p in app.disable:
        cmd += ["-d", p]
    cmd.append(str(apkm))
    if not cfg.keystore.exists():
        log.warning("keystore %s will be created; BACK IT UP or updates will need uninstall", cfg.keystore)
    log.info("running: %s", " ".join(cmd))
    res = subprocess.run(cmd, capture_output=True, text=True)
    log.info(res.stdout[-3000:])
    if res.returncode != 0:
        raise RuntimeError(f"morphe exit {res.returncode}: {res.stderr[-1500:] or res.stdout[-1500:]}")
    if not out.exists():
        raise RuntimeError("morphe produced no output apk")
    _check_result(result)
    return out


def _check_result(result: Path):
    if not result.exists():
        return
    try:
        data = json.loads(result.read_text())
    except Exception:  # noqa: BLE001
        return
    failed = []
    items = data.get("patches") if isinstance(data, dict) else data
    for p in items or []:
        if isinstance(p, dict) and (p.get("success") is False or p.get("error")):
            failed.append(p.get("name", "?"))
    if failed:
        raise RuntimeError(f"patches failed: {', '.join(failed)}")
