import logging
import time

log = logging.getLogger("patcher")


def retry(fn, tries=3, delay=3.0, what="call"):
    for i in range(1, tries + 1):
        try:
            return fn()
        except Exception as e:  # noqa: BLE001
            if i == tries:
                raise
            log.warning("%s failed (%s), retry %d/%d", what, e, i, tries - 1)
            time.sleep(delay * i)
