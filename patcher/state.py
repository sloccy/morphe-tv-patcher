import json
from pathlib import Path


class State:
    def __init__(self, path: Path):
        self.path = path
        try:
            self.d = json.loads(path.read_text())
        except Exception:  # noqa: BLE001
            self.d = {}

    def get(self, package):
        return self.d.get(package, {})

    def set(self, package, **kw):
        self.d[package] = kw
        self.path.parent.mkdir(parents=True, exist_ok=True)
        tmp = self.path.with_suffix(".tmp")
        tmp.write_text(json.dumps(self.d, indent=2))
        tmp.replace(self.path)
