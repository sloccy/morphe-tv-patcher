import os
from dataclasses import dataclass, field
from pathlib import Path

import yaml


@dataclass
class App:
    package: str
    apkmirror: str
    installed_package: str = ""
    options_file: str = ""
    enable: list = field(default_factory=list)
    disable: list = field(default_factory=list)
    allow_uninstall: bool = False

    def __post_init__(self):
        self.installed_package = self.installed_package or self.package


@dataclass
class Config:
    host: str
    port: int
    interval_s: int
    patches_repo: str
    prerelease: bool
    bytecode_mode: str
    apps: list
    data: Path
    morphe_jar: str
    apkmirror_enabled: bool
    github_token: str

    @property
    def cache(self): return self.data / "work" / "cache"
    @property
    def inputs(self): return self.data / "work" / "input"
    @property
    def output(self): return self.data / "work" / "output"
    @property
    def keystore(self): return self.data / "keystore" / "morphe.keystore"
    @property
    def state_file(self): return self.data / "state.json"


def parse_interval(s) -> int:
    s = str(s).strip().lower()
    mult = {"s": 1, "m": 60, "h": 3600, "d": 86400}
    if s[-1] in mult:
        return int(float(s[:-1]) * mult[s[-1]])
    return int(s)


def load(path: str) -> Config:
    raw = yaml.safe_load(Path(path).read_text())
    dev = raw.get("device", {})
    host = os.environ.get("DEVICE_HOST") or dev.get("host") or ""
    port = int(os.environ.get("DEVICE_PORT") or dev.get("port") or 0)
    legacy = os.environ.get("DEVICE_SERIAL") or dev.get("serial") or ""
    if not host and legacy:
        host, _, p = legacy.partition(":")
        port = port or int(p or 0)
    if not host:
        raise ValueError("device.host required")
    apps = [App(**a) for a in raw["apps"]]
    if not apps:
        raise ValueError("no apps configured")
    p = raw.get("patches", {})
    cfg = Config(
        host=host,
        port=port,
        interval_s=parse_interval(raw.get("schedule", {}).get("interval", "6h")),
        patches_repo=p.get("repo", "ajstrick81/morphe-androidtv-patches"),
        prerelease=bool(p.get("prerelease", False)),
        bytecode_mode=raw.get("morphe", {}).get("bytecode_mode", "STRIP_SAFE"),
        apps=apps,
        data=Path(os.environ.get("DATA_DIR", "/data")),
        morphe_jar=os.environ.get("MORPHE_JAR", "/opt/morphe/morphe.jar"),
        apkmirror_enabled=os.environ.get("APKMIRROR_DISABLED", "") not in ("1", "true"),
        github_token=os.environ.get("GITHUB_TOKEN", ""),
    )
    for d in (cfg.cache, cfg.inputs, cfg.output, cfg.keystore.parent):
        d.mkdir(parents=True, exist_ok=True)
    return cfg
