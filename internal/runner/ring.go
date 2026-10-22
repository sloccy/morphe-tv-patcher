package runner

import (
	"io"
	"log/slog"
	"strings"
	"sync"
)

// Ring keeps the last N log lines for the UI and passes everything on to w.
type Ring struct {
	mu    sync.Mutex
	lines []string
	n     int
	w     io.Writer
	buf   string
}

// NewRing makes a ring of n lines that also writes to w (may be nil).
func NewRing(n int, w io.Writer) *Ring { return &Ring{n: n, w: w} }

// Write implements io.Writer (one slog text line per call, but tolerates partial lines).
func (r *Ring) Write(p []byte) (int, error) {
	r.mu.Lock()
	r.buf += string(p)
	for {
		i := strings.IndexByte(r.buf, '\n')
		if i < 0 {
			break
		}
		r.lines = append(r.lines, r.buf[:i])
		r.buf = r.buf[i+1:]
		if len(r.lines) > r.n {
			r.lines = r.lines[len(r.lines)-r.n:]
		}
	}
	r.mu.Unlock()
	if r.w != nil {
		return r.w.Write(p)
	}
	return len(p), nil
}

// Lines returns a copy of the stored lines.
func (r *Ring) Lines() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string{}, r.lines...)
}

// Logger returns a text slog logger writing through the ring.
func (r *Ring) Logger() *slog.Logger { return slog.New(slog.NewTextHandler(r, nil)) }
