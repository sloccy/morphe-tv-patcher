import json
from pathlib import Path


class State:
    """JSON state. Per package: apk_version, patches_version, signer_sha256, fetch_failures, ..."""

    def __init__(self, path: Path):
        self.path = path
        try:
            self.d = json.loads(path.read_text())
        except Exception:  # noqa: BLE001
            self.d = {}

    def get(self, key):
        return self.d.get(key, {})

    def set(self, key, **kw):
        """Merge into the entry (None deletes a field) and save atomically."""
        cur = dict(self.d.get(key, {}))
        for k, v in kw.items():
            if v is None:
                cur.pop(k, None)
            else:
                cur[k] = v
        self.d[key] = cur
        self.path.parent.mkdir(parents=True, exist_ok=True)
        tmp = self.path.with_suffix(".tmp")
        tmp.write_text(json.dumps(self.d, indent=2))
        tmp.replace(self.path)
