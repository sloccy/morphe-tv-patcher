package notify

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSendPostsToTelegram(t *testing.T) {
	var path, body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		_ = r.ParseForm()
		body = r.PostForm.Get("chat_id") + "|" + r.PostForm.Get("parse_mode") + "|" + r.PostForm.Get("text")
	}))
	defer srv.Close()
	(&Telegram{Token: "TOK", Chat: "42", API: srv.URL}).Send(context.Background(), "<b>TV</b>\nNetflix: 1 \u2192 2 (patches v1)")
	if path != "/botTOK/sendMessage" || !strings.HasPrefix(body, "42|HTML|<b>TV</b>\nNetflix") {
		t.Errorf("path %q body %q", path, body)
	}
}

func TestUnconfiguredOnlyLogsAndErrorsNeverLeakTheToken(t *testing.T) {
	(&Telegram{}).Send(context.Background(), "x")
	var nilT *Telegram
	nilT.Send(context.Background(), "x")

	var logs bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	(&Telegram{Token: "SECRETTOKEN", Chat: "1", API: "http://127.0.0.1:1"}).Send(context.Background(), "x")
	if strings.Contains(logs.String(), "SECRETTOKEN") {
		t.Errorf("token leaked into logs: %s", logs.String())
	}
}

func TestTestReportsSuccessAndRefusalWithoutTheToken(t *testing.T) {
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) }))
	defer srv.Close()
	tg := &Telegram{Token: "SECRETTOKEN", Chat: "42", API: srv.URL}
	if !tg.Configured() {
		t.Error("should be configured")
	}
	if err := tg.Test(context.Background()); err != nil {
		t.Errorf("test message: %v", err)
	}
	status = http.StatusUnauthorized
	err := tg.Test(context.Background())
	if err == nil || strings.Contains(err.Error(), "SECRETTOKEN") || !strings.Contains(err.Error(), "401") {
		t.Errorf("refusal error: %v", err)
	}
	if (&Telegram{}).Configured() || (*Telegram)(nil).Configured() {
		t.Error("an empty bot is not configured")
	}
	if err := (&Telegram{}).Test(context.Background()); err == nil {
		t.Error("an unconfigured test should fail")
	}
}
