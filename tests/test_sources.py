from pathlib import Path
from types import SimpleNamespace

import pytest

from patcher import __main__ as M
from patcher import sources, verify
from patcher.config import App
from patcher.errors import Blocked, NotFound, VerifyError
from patcher.state import State


def make(tmp_path, **kw):
    cfg = SimpleNamespace(inputs=tmp_path / "input", cache=tmp_path / "cache", output=tmp_path / "out",
                          apkmirror_enabled=True, apkmirror_direct=True, flaresolverr_url="",
                          fetch_failures_before_alert=3)
    for d in (cfg.inputs, cfg.cache, cfg.output):
        d.mkdir()
    for k, v in kw.items():
        setattr(cfg, k, v)
    app = App(package="com.x", apkmirror="pub/app")
    return cfg, app, State(tmp_path / "state.json")


def ok_verify(tmp_path):
    def v(path, package, version, source, pinned=""):
        return verify.Verified(path, source, "ab" * 32, package, version)
    return v


def test_default_sources():
    assert App(package="com.netflix.ninja", apkmirror="x").sources == ["apkmirror"]
    assert App(package="com.other", apkmirror="x").sources == ["apkmirror", "apkpure"]


def test_apkmirror_first_and_learns_signer(tmp_path, monkeypatch):
    cfg, app, st = make(tmp_path)
    called = []
    monkeypatch.setattr(sources.apkmirror, "download", lambda *a, **k: (called.append("m"), (tmp_path / "f.apkm", "chrome"))[1])
    monkeypatch.setattr(sources, "verify", ok_verify(tmp_path))
    monkeypatch.setattr(sources.apkpure, "available", lambda: True)
    f = sources.fetch(cfg, app, "1.0", 28, st)
    assert f.detail == "apkmirror (chrome)" and f.learned_signer
    assert called == ["m"]
    assert st.get("com.x")["signer_sha256"] == "ab" * 32
    assert st.get("_apkmirror")["last_profile"] == "chrome"


def test_falls_through_blocked_apkmirror_to_flaresolverr_then_apkpure(tmp_path, monkeypatch):
    cfg, app, st = make(tmp_path, flaresolverr_url="http://f")
    order = []

    def mirror(slug, ver, sdk, dest, flare_url="", profile_hint=""):
        order.append("flare" if flare_url else "direct")
        raise Blocked("cf")
    monkeypatch.setattr(sources.apkmirror, "download", mirror)
    monkeypatch.setattr(sources.apkpure, "available", lambda: True)
    monkeypatch.setattr(sources.apkpure, "download", lambda p, v, d: (order.append("pure"), tmp_path / "p.xapk")[1])
    monkeypatch.setattr(sources, "verify", ok_verify(tmp_path))
    st.set("com.x", signer_sha256="ab" * 32)
    f = sources.fetch(cfg, app, "1.0", 28, st)
    assert order == ["direct", "flare", "pure"] and f.detail == "apkpure"


def test_all_fail_is_temporary_not_fatal(tmp_path, monkeypatch):
    cfg, app, st = make(tmp_path)
    monkeypatch.setattr(sources.apkmirror, "download", lambda *a, **k: (_ for _ in ()).throw(NotFound("nope")))
    monkeypatch.setattr(sources.apkpure, "available", lambda: False)
    with pytest.raises(sources.FetchError) as e:
        sources.fetch(cfg, app, "1.0", 28, st)
    assert not e.value.fatal and len(e.value.attempts) == 2


def test_signature_mismatch_is_fatal_and_file_deleted(tmp_path, monkeypatch):
    cfg, app, st = make(tmp_path)
    f = tmp_path / "evil.apkm"
    f.write_bytes(b"x")
    monkeypatch.setattr(sources.apkmirror, "download", lambda *a, **k: (f, "chrome"))
    monkeypatch.setattr(sources.apkpure, "available", lambda: False)

    def bad(*a, **k):
        raise VerifyError("signature", "signer mismatch")
    monkeypatch.setattr(sources, "verify", bad)
    with pytest.raises(sources.FetchError) as e:
        sources.fetch(cfg, app, "1.0", 28, st)
    assert e.value.fatal and not f.exists()


def test_dropfolder_wins_and_old_versions_are_ignored(tmp_path, monkeypatch):
    cfg, app, st = make(tmp_path)
    d = cfg.inputs / app.package
    d.mkdir()
    (d / "old.apkm").write_bytes(b"x")
    (d / "new.apkm").write_bytes(b"x")

    def v(path, package, version, source, pinned=""):
        if path.name == "old.apkm":
            raise VerifyError("version", "old")
        return verify.Verified(path, source, "cd" * 32, package, version)
    monkeypatch.setattr(sources, "verify", v)
    monkeypatch.setattr(sources.apkmirror, "download", lambda *a, **k: pytest.fail("should not scrape"))
    f = sources.fetch(cfg, app, "1.0", 28, st)
    assert f.detail == "dropfolder" and f.verified.path.name == "new.apkm"


# ---- verify() rules, with inspect() faked ----
def fake_inspect(monkeypatch, **over):
    base = dict(package="com.x", version="1.0", leanback=True, abis={"armeabi-v7a"},
                signer="aa" * 32, apksigner_ok=True, apksigner_msg="")
    base.update(over)
    monkeypatch.setattr(verify, "inspect", lambda p: base)


def test_verify_rules(monkeypatch):
    P = Path("x.apkm")
    fake_inspect(monkeypatch)
    assert verify.verify(P, "com.x", "1.0", "apkmirror", "").signer == "aa" * 32
    for over, kind in [(dict(leanback=False), "leanback"), (dict(abis={"arm64-v8a"}), "arch"),
                       (dict(apksigner_ok=False), "signature"), (dict(version="2"), "version"),
                       (dict(package="com.y"), "package")]:
        fake_inspect(monkeypatch, **over)
        with pytest.raises(VerifyError) as e:
            verify.verify(P, "com.x", "1.0", "apkmirror", "")
        assert e.value.kind == kind
    fake_inspect(monkeypatch)
    with pytest.raises(VerifyError) as e:
        verify.verify(P, "com.x", "1.0", "apkmirror", "bb" * 32)   # pin mismatch
    assert e.value.kind == "signature" and e.value.fatal
    with pytest.raises(VerifyError) as e:
        verify.verify(P, "com.x", "1.0", "apkpure", "")            # unpinned, untrusted source
    assert e.value.kind == "unpinned" and not e.value.fatal
    assert verify.verify(P, "com.x", "1.0", "apkpure", "aa" * 32)  # pinned: fine
    fake_inspect(monkeypatch, abis=set())                           # no native code at all
    assert verify.verify(P, "com.x", "1.0", "apkmirror", "")


# ---- alert rules ----
def test_alert_rules(tmp_path):
    cfg, app, st = make(tmp_path)
    temp = sources.FetchError(["blocked"], fatal=False)
    t0 = 1_000_000.0
    r1 = M.fetch_failed(cfg, app, st, "1.0", temp, now=t0)
    r2 = M.fetch_failed(cfg, app, st, "1.0", temp, now=t0 + 21600)
    r3 = M.fetch_failed(cfg, app, st, "1.0", temp, now=t0 + 43200)
    r4 = M.fetch_failed(cfg, app, st, "1.0", temp, now=t0 + 64800)   # within 24h of r3's alert
    assert [r.alert for r in (r1, r2, r3, r4)] == [False, False, True, False]
    r5 = M.fetch_failed(cfg, app, st, "1.0", temp, now=t0 + 43200 + 90000)
    assert r5.alert                                                   # cooldown over
    assert "input/com.x" in r1.msg.replace("\\", "/")


def test_fatal_alerts_immediately(tmp_path):
    cfg, app, st = make(tmp_path)
    r = M.fetch_failed(cfg, app, st, "1.0", sources.FetchError(["signature: x"], fatal=True), now=5_000_000.0)
    assert r.alert and not r.ok
