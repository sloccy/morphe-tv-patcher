// Package notify sends Telegram messages with the same bot as the rest of the homelab.
package notify

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Telegram sends to one chat. A zero value (no token or chat) only logs.
type Telegram struct {
	Token, Chat string
	HC          *http.Client
	API         string // default https://api.telegram.org
}

// Send logs the message and delivers it if configured. Errors are logged without the URL,
// which contains the bot token.
func (t *Telegram) Send(ctx context.Context, title, body string) {
	slog.Info("notify", "title", title, "body", body)
	if t == nil || t.Token == "" || t.Chat == "" {
		return
	}
	api := t.API
	if api == "" {
		api = "https://api.telegram.org"
	}
	hc := t.HC
	if hc == nil {
		hc = &http.Client{Timeout: 20 * time.Second}
	}
	text := "tv-patcher: " + title + "\n" + body
	if len(text) > 4000 {
		text = text[:4000]
	}
	form := url.Values{"chat_id": {t.Chat}, "text": {text}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("%s/bot%s/sendMessage", api, t.Token),
		strings.NewReader(form.Encode()))
	if err != nil {
		slog.Warn("notify failed")
		return
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := hc.Do(req)
	if err != nil {
		slog.Warn("notify failed", "err", redact(err.Error(), t.Token))
		return
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		slog.Warn("notify failed", "status", resp.StatusCode)
	}
}

func redact(s, secret string) string { return strings.ReplaceAll(s, secret, "***") }
