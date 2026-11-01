// Package notify sends Telegram messages with the same bot as the rest of the homelab.
package notify

import (
	"context"
	"errors"
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

// Configured says whether messages are actually delivered (a token and a chat are set).
func (t *Telegram) Configured() bool { return t != nil && t.Token != "" && t.Chat != "" }

// Send logs the message and delivers it if configured. Errors are logged without the URL,
// which contains the bot token.
func (t *Telegram) Send(ctx context.Context, title, body string) {
	slog.Info("notify", "title", title, "body", body)
	if !t.Configured() {
		return
	}
	if err := t.deliver(ctx, title, body); err != nil {
		slog.Warn("notify failed", "err", err)
	}
}

// Test sends a short message and reports why it failed, if it did.
func (t *Telegram) Test(ctx context.Context) error {
	if !t.Configured() {
		return errors.New("Telegram is not set up")
	}
	return t.deliver(ctx, "test", "This is a test message from the TV Patcher page.")
}

// deliver posts one message. Its errors never contain the bot token.
func (t *Telegram) deliver(ctx context.Context, title, body string) error {
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
		return errors.New("could not build the request")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := hc.Do(req)
	if err != nil {
		return errors.New(redact(err.Error(), t.Token))
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("Telegram answered HTTP %d", resp.StatusCode)
	}
	return nil
}

func redact(s, secret string) string { return strings.ReplaceAll(s, secret, "***") }
