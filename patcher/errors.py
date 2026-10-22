class Blocked(RuntimeError):
    """Cloudflare / rate limit: temporary, try another route or wait."""


class NotFound(RuntimeError):
    """The source doesn't have this version (yet)."""


class VerifyError(RuntimeError):
    """A candidate file failed a check. kind: version|package|leanback|arch|signature|format|unpinned."""

    def __init__(self, kind: str, msg: str):
        super().__init__(f"{kind}: {msg}")
        self.kind = kind

    @property
    def fatal(self) -> bool:
        # these mean "wrong or tampered app", never "try again later"
        return self.kind in ("signature", "leanback", "package")
