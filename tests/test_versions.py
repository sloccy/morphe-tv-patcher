import json
import subprocess
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))
from patcher import apkmirror, versions  # noqa: E402


def test_newest_numeric():
    assert versions.newest(["7.8.100", "7.10.102", "7.5.102"]) == "7.10.102"
    assert versions.newest(["26.12.1+rc1-2026.07.15", "26.16.0+rc2-2026.09.08",
                            "26.9.2+rc1-2026.06.12"]) == "26.16.0+rc2-2026.09.08"
    assert versions.newest(["16.8.0", "16.17.0", "16.12.0"]) == "16.17.0"


def test_newest_netflix_build():
    assert versions.newest(["13.0.1 build 25028"]) == "13.0.1 build 25028"


def test_from_patches_list():
    d = {"patches": [{"compatiblePackages": {"a": ["1.2", "1.10"]}}, {"compatiblePackages": {"a": ["1.9"], "b": ["x"]}}]}
    assert versions.from_patches_list(d, "a") == ["1.2", "1.9", "1.10"]


def test_version_slug():
    assert apkmirror.version_slug("6.23.23+v15.5.0.70-armv7a") == "6-23-23v15-5-0-70-armv7a"
    assert apkmirror.version_slug("13.0.1 build 25028") == "13-0-1-build-25028"
    assert apkmirror.version_slug("26.16.0+rc2-2026.09.08") == "26-16-0rc2-2026-09-08"


def test_apkm_info_real_fixtures():
    from patcher import apkm
    fx = Path(__file__).parent / "fixtures"
    want = {
        "com.amazon.amazonvideo.livingroom": "6.23.23+v15.5.0.70-armv7a",
        "com.netflix.ninja": "13.0.1 build 25028",
        "com.disney.disneyplus": "26.16.0+rc2-2026.09.08",
    }
    for pkg, ver in want.items():
        i = apkm.parse_info(json.loads((fx / f"info-{pkg}.json").read_text()))
        assert (i["package"], i["version"]) == (pkg, ver)
        assert "armeabi-v7a" in i["arches"]
