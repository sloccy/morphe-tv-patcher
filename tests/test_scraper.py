from pathlib import Path

import pytest
from bs4 import BeautifulSoup

from patcher import apkmirror as A
from patcher import flaresolverr
from patcher.errors import Blocked, NotFound

FX = Path(__file__).parent / "fixtures"


def soup(name):
    return BeautifulSoup((FX / f"apkmirror-{name}.html").read_text(), "html.parser")


def test_pick_variant_from_real_release_page():
    url = A.pick_variant(soup("release"), 28)
    assert url.endswith("-armv7a-android-apk-download/")


def test_download_chain_links_from_real_pages():
    dl_page = A.final_link(soup("variant"))
    assert "/download/?key=" in dl_page
    assert "download.php?id=" in A.file_link(soup("download"))


def test_file_link_missing_means_blocked():
    with pytest.raises(Blocked):
        A.file_link(BeautifulSoup("<html>nothing</html>", "html.parser"))


class FakeClient:
    """Serves saved pages; 404 for the guessed release URL to force the app-page lookup."""
    def __init__(self):
        self.seen = []

    def html(self, url, referer=None):
        self.seen.append(url)
        if url.endswith("-release/") and "/apk/amazon-mobile-llc/prime-video-android-tv-android-tv/prime-video" in url:
            raise NotFound(url)
        return soup("app")


def test_release_lookup_falls_back_to_app_page(monkeypatch):
    monkeypatch.setattr(A, "_sleep", lambda *a: None)
    # a version that exists on the saved app page but whose guessed URL we force to 404
    html = (FX / "apkmirror-app.html").read_text()
    import re
    m = re.search(r"prime-video-android-tv-([a-z0-9-]+)-release/", html)
    slug = "amazon-mobile-llc/prime-video-android-tv-android-tv"
    c = FakeClient()
    found, _ = A.find_release(c, slug, "6.24.8+v16.0.0.361-allabis")
    assert found.endswith("6-24-8v16-0-0-361-allabis-release/")


def test_release_lookup_not_found(monkeypatch):
    monkeypatch.setattr(A, "_sleep", lambda *a: None)
    with pytest.raises(NotFound):
        A.find_release(FakeClient(), "amazon-mobile-llc/prime-video-android-tv-android-tv", "0.0.1")


def test_flaresolverr_ok_and_error(monkeypatch):
    class R:
        def __init__(self, j): self.j = j
        def json(self): return self.j
    monkeypatch.setattr(flaresolverr.requests, "post", lambda *a, **k: R(
        {"status": "ok", "solution": {"response": "<html/>", "status": 200, "userAgent": "UA",
                                       "cookies": [{"name": "cf", "value": "1"}]}}))
    r = flaresolverr.solve("http://x", "https://y")
    assert r["cookies"] == {"cf": "1"} and r["user_agent"] == "UA"
    monkeypatch.setattr(flaresolverr.requests, "post", lambda *a, **k: R({"status": "error", "message": "boom"}))
    with pytest.raises(Blocked):
        flaresolverr.solve("http://x", "https://y")
