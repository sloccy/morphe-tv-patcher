"""Thin client for a FlareSolverr sidecar (headless Chrome that solves Cloudflare)."""
import requests

from .errors import Blocked


def solve(base_url: str, url: str, timeout_ms: int = 60000) -> dict:
    """Return {"html","cookies","user_agent","status"} for a page loaded in the browser."""
    try:
        r = requests.post(f"{base_url}/v1", json={"cmd": "request.get", "url": url,
                          "maxTimeout": timeout_ms}, timeout=timeout_ms / 1000 + 15)
        j = r.json()
    except Exception as e:  # noqa: BLE001
        raise Blocked(f"flaresolverr unreachable: {type(e).__name__}")
    if j.get("status") != "ok":
        raise Blocked(f"flaresolverr: {j.get('message', 'error')}")
    sol = j["solution"]
    return {"html": sol.get("response", ""), "status": sol.get("status", 200),
            "cookies": {c["name"]: c["value"] for c in sol.get("cookies", [])},
            "user_agent": sol.get("userAgent", "")}
