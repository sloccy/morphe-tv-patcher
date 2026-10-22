import os

import requests

from .util import log


def send(title: str, body: str):
    """Telegram, using the same TELEGRAM_BOT_TOKEN / TELEGRAM_CHAT_ID as the homelab (Gatus)."""
    log.info("NOTIFY %s: %s", title, body)
    token, chat = os.environ.get("TELEGRAM_BOT_TOKEN"), os.environ.get("TELEGRAM_CHAT_ID")
    if not (token and chat):
        return
    try:
        r = requests.post(f"https://api.telegram.org/bot{token}/sendMessage",
                          data={"chat_id": chat, "text": f"tv-patcher: {title}\n{body}"[:4000]},
                          timeout=20)
        if not r.ok:
            log.warning("telegram HTTP %s", r.status_code)
    except Exception as e:  # noqa: BLE001
        log.warning("notify failed: %s", type(e).__name__)  # never log the URL (contains token)
