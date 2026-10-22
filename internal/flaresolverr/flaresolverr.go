// Package flaresolverr is a thin client for a FlareSolverr sidecar: a headless Chrome that
// solves Cloudflare challenges and hands back the page plus the cookies it earned.
package flaresolverr

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// ErrUnavailable means the sidecar could not load the page (down, or the challenge won).
var ErrUnavailable = errors.New("flaresolverr unavailable")

// Result is a page loaded in the sidecar's browser.
type Result struct {
	HTML      string
	Status    int
	Cookies   map[string]string
	UserAgent string
}

// Solve loads url through the sidecar at base.
func Solve(ctx context.Context, hc *http.Client, base, url string) (*Result, error) {
	const maxTimeout = 60000
	body, _ := json.Marshal(map[string]any{"cmd": "request.get", "url": url, "maxTimeout": maxTimeout})
	ctx, cancel := context.WithTimeout(ctx, maxTimeout*time.Millisecond+15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	defer func() { _ = resp.Body.Close() }()
	var j struct {
		Status   string `json:"status"`
		Message  string `json:"message"`
		Solution struct {
			Response  string `json:"response"`
			Status    int    `json:"status"`
			UserAgent string `json:"userAgent"`
			Cookies   []struct {
				Name  string `json:"name"`
				Value string `json:"value"`
			} `json:"cookies"`
		} `json:"solution"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(&j); err != nil {
		return nil, fmt.Errorf("%w: bad response: %w", ErrUnavailable, err)
	}
	if j.Status != "ok" {
		return nil, fmt.Errorf("%w: %s", ErrUnavailable, j.Message)
	}
	r := &Result{HTML: j.Solution.Response, Status: j.Solution.Status, UserAgent: j.Solution.UserAgent, Cookies: map[string]string{}}
	if r.Status == 0 {
		r.Status = http.StatusOK
	}
	for _, c := range j.Solution.Cookies {
		r.Cookies[c.Name] = c.Value
	}
	return r, nil
}
