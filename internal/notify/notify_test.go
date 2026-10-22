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
		body = r.PostForm.Get("chat_id") + "|" + r.PostForm.Get("text")
	}))
	defer srv.Close()
	(&Telegram{Token: "TOK", Chat: "42", API: srv.URL}).Send(context.Background(), "Updated", "Netflix 13.0.1")
	if path != "/botTOK/sendMessage" || !strings.HasPrefix(body, "42|tv-patcher: Updated\nNetflix") {
		t.Errorf("path %q body %q", path, body)
	}
}

func TestUnconfiguredOnlyLogsAndErrorsNeverLeakTheToken(t *testing.T) {
	(&Telegram{}).Send(context.Background(), "x", "y")
	var nilT *Telegram
	nilT.Send(context.Background(), "x", "y")

	var logs bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	(&Telegram{Token: "SECRETTOKEN", Chat: "1", API: "http://127.0.0.1:1"}).Send(context.Background(), "x", "y")
	if strings.Contains(logs.String(), "SECRETTOKEN") {
		t.Errorf("token leaked into logs: %s", logs.String())
	}
}
