"""APKPure via the apkeep binary (EFForg/apkeep). Second, independent source."""
import shutil
import subprocess
import tempfile
from pathlib import Path

from .errors import NotFound
from .util import log

EXTS = (".xapk", ".apks", ".apkm", ".apk")


def available() -> bool:
    return shutil.which("apkeep") is not None


def download(package: str, version: str, dest_dir: Path) -> Path:
    exe = shutil.which("apkeep")
    if not exe:
        raise NotFound("apkeep not installed")
    with tempfile.TemporaryDirectory() as t:
        res = subprocess.run([exe, "-a", f"{package}@{version}", "-d", "apk-pure", "-s", "3000", t],
                             capture_output=True, text=True, timeout=1200)
        files = [f for f in Path(t).iterdir() if f.suffix in EXTS]
        if res.returncode != 0 or not files:
            log.info("apkeep: %s", (res.stdout + res.stderr)[-300:])
            raise NotFound(f"APKPure has no {package} {version}")
        f = max(files, key=lambda p: p.stat().st_size)
        dest = dest_dir / f"apkpure-{package}-{version.replace(' ', '_')}{f.suffix}"
        shutil.move(f, dest)
        return dest
