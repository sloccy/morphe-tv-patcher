import re
import socket
import subprocess
from concurrent.futures import ThreadPoolExecutor
from pathlib import Path

from .util import log, retry


class AdbError(Exception):
    pass


def _open(host, port, timeout=0.4):
    try:
        with socket.create_connection((host, port), timeout=timeout):
            return port
    except OSError:
        return None


def scan_ports(host, lo=30000, hi=50000) -> list:
    """Wireless-debugging ports are random high ports; find open ones."""
    with ThreadPoolExecutor(max_workers=400) as ex:
        return [p for p in ex.map(lambda p: _open(host, p), range(lo, hi)) if p]


class Adb:
    def __init__(self, host: str, port: int = 0, port_file: Path = None):
        self.host, self.hint, self.port_file = host, port, port_file
        self.serial = f"{host}:{port}" if port else host

    def pair(self, pair_port: int, code: str):
        """One-time: TV > Wireless debugging > Pair device with pairing code."""
        r = subprocess.run(["adb", "pair", f"{self.host}:{pair_port}", code],
                           capture_output=True, text=True, timeout=60)
        out = (r.stdout + r.stderr).strip()
        if "Successfully paired" not in out:
            raise AdbError(out)
        return out

    def _saved_port(self):
        try:
            return int(self.port_file.read_text())
        except Exception:  # noqa: BLE001
            return 0

    def _try(self, port) -> bool:
        code, out = self._run("connect", f"{self.host}:{port}", timeout=20)
        if "unauthorized" in out:
            raise AdbError("unauthorized/not paired: run `patcher pair` and approve on the TV")
        return "connected to" in out

    def _mdns_port(self):
        _, out = self._run("mdns", "services", timeout=15)
        for line in out.splitlines():
            m = re.search(r"_adb-tls-connect\S*\s+" + re.escape(self.host) + r":(\d+)", line)
            if m:
                return int(m.group(1))
        return 0

    def _run(self, *args, timeout=300):
        r = subprocess.run(["adb", *args], capture_output=True, text=True, timeout=timeout)
        return r.returncode, (r.stdout + r.stderr).strip()

    def connect(self):
        """Port changes whenever Wireless debugging is toggled/rebooted: try saved, hint, mDNS, then scan."""
        cands = [p for p in (self._saved_port(), self.hint) if p]
        for p in cands:
            if self._try(p):
                return self._ok(p)
        p = self._mdns_port()
        if p and p not in cands and self._try(p):
            return self._ok(p)
        log.info("known ports failed, scanning %s", self.host)
        for p in scan_ports(self.host):
            if p not in cands and self._try(p):
                return self._ok(p)
        raise AdbError(f"cannot reach {self.host}: is Wireless debugging on, and paired?")

    def _ok(self, port):
        self.serial = f"{self.host}:{port}"
        if self.port_file:
            self.port_file.write_text(str(port))
        log.info("adb connected %s", self.serial)

    def shell(self, cmd: str) -> str:
        code, out = self._run("-s", self.serial, "shell", cmd, timeout=60)
        if code != 0:
            raise AdbError(out)
        return out

    def abis(self) -> list:
        return [a for a in self.shell("getprop ro.product.cpu.abilist").strip().split(",") if a]

    def sdk(self) -> int:
        return int(self.shell("getprop ro.build.version.sdk").strip())

    def installed_version(self, package: str):
        out = self.shell(f"dumpsys package {package}")
        m = re.search(r"versionName=(\S.*)", out)
        return m.group(1).strip() if m else None

    def install(self, apk) -> tuple:
        code, out = self._run("-s", self.serial, "install", "-r", str(apk), timeout=900)
        ok = code == 0 and "Success" in out
        log.info("adb install: %s", out[-300:])
        return ok, out
