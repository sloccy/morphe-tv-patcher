package apkmirror

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"time"
)

// Priority orders waiters at the Gate: real downloads go ahead of background probing.
type Priority int

const (
	// Background is for learning which CPU builds an app offers.
	Background Priority = iota
	// Foreground is for a real fetch that a patch run is waiting for.
	Foreground
)

// Gate is the one polite door to APKMirror. Every request from every caller goes through it:
// one at a time, with a gap between requests, and a shared cool-down once the site pushes back.
type Gate struct {
	MinGap   time.Duration // gap between requests (jittered up to 2x)
	Cooldown time.Duration // first cool-down after a block; doubles while blocks continue
	MaxCool  time.Duration

	mu           sync.Mutex
	busy         bool
	foreground   int           // Foreground waiters
	changed      chan struct{} // closed and replaced whenever busy/foreground change
	nextAt       time.Time
	blockedUntil time.Time
	strikes      int
}

// NewGate returns a gate with the given spacing and first cool-down.
func NewGate(minGap, cooldown time.Duration) *Gate {
	return &Gate{MinGap: minGap, Cooldown: cooldown, MaxCool: time.Hour, changed: make(chan struct{})}
}

// Shared is the gate production code uses; main sets its timings from the environment.
var Shared = NewGate(4*time.Second, 5*time.Minute)

// SharedFlare paces requests that go through a FlareSolverr sidecar (they leave from the same
// IP, but a solved session is treated differently, so a block on one route does not close the other).
var SharedFlare = NewGate(4*time.Second, 5*time.Minute)

// BlockedUntil reports when the cool-down ends (zero when the gate is open).
func (g *Gate) BlockedUntil() time.Time {
	g.mu.Lock()
	defer g.mu.Unlock()
	if time.Now().Before(g.blockedUntil) {
		return g.blockedUntil
	}
	return time.Time{}
}

func (g *Gate) wake() { close(g.changed); g.changed = make(chan struct{}) }

// acquire waits for the turn and the gap. It fails at once during a cool-down.
func (g *Gate) acquire(ctx context.Context, prio Priority) error {
	g.mu.Lock()
	if prio == Foreground {
		g.foreground++
		g.wake()
	}
	for g.busy || (prio == Background && g.foreground > 0) {
		if err := g.checkBlocked(); err != nil {
			g.leave(prio)
			g.mu.Unlock()
			return err
		}
		ch := g.changed
		g.mu.Unlock()
		select {
		case <-ctx.Done():
			g.mu.Lock()
			g.leave(prio)
			g.mu.Unlock()
			return ctx.Err()
		case <-ch:
		}
		g.mu.Lock()
	}
	g.busy = true
	g.leave(prio)
	if err := g.checkBlocked(); err != nil {
		g.busy = false
		g.wake()
		g.mu.Unlock()
		return err
	}
	wait := time.Until(g.nextAt)
	g.mu.Unlock()
	if wait > 0 {
		t := time.NewTimer(wait)
		defer t.Stop()
		select {
		case <-ctx.Done():
			g.mu.Lock()
			g.busy = false
			g.wake()
			g.mu.Unlock()
			return ctx.Err()
		case <-t.C:
		}
	}
	return nil
}

// leave drops a Foreground waiter's count (callers hold mu).
func (g *Gate) leave(prio Priority) {
	if prio == Foreground {
		g.foreground--
		g.wake()
	}
}

func (g *Gate) checkBlocked() error {
	if d := time.Until(g.blockedUntil); d > 0 {
		return fmt.Errorf("%w: APKMirror asked us to slow down; paused until %s", ErrBlocked, g.blockedUntil.Format("15:04"))
	}
	return nil
}

// release ends a turn. A blocked result closes the gate for everyone; a success reopens it.
func (g *Gate) release(err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	gap := g.MinGap
	if gap > 0 {
		gap += time.Duration(rand.Int64N(int64(gap) + 1)) //nolint:gosec // jitter, not security
	}
	g.nextAt = time.Now().Add(gap)
	switch {
	case err == nil:
		g.strikes = 0
	case errors.Is(err, ErrBlocked):
		cool := g.Cooldown << min(g.strikes, 10)
		if g.MaxCool > 0 && cool > g.MaxCool {
			cool = g.MaxCool
		}
		g.strikes++
		g.blockedUntil = time.Now().Add(cool)
	}
	g.busy = false
	g.wake()
}

// Gated wraps a Fetcher so each page or file request takes a turn at the Gate. The wrapper
// is long-lived, so the cookie session behind it is reused instead of warmed up every time.
type Gated struct {
	F    Fetcher
	G    *Gate
	Prio Priority

	mu       sync.Mutex
	warmedAt time.Time
}

// Page implements Fetcher.
func (g *Gated) Page(ctx context.Context, url, referer string) (*Page, error) {
	if err := g.G.acquire(ctx, g.Prio); err != nil {
		return nil, err
	}
	p, err := g.F.Page(ctx, url, referer)
	g.G.release(err)
	return p, err
}

// File implements Fetcher.
func (g *Gated) File(ctx context.Context, url, referer, dest string) error {
	if err := g.G.acquire(ctx, g.Prio); err != nil {
		return err
	}
	err := g.F.File(ctx, url, referer, dest)
	g.G.release(err)
	return err
}

// needsWarmup is true when the session has not loaded the home page in the last 30 minutes.
func (g *Gated) needsWarmup() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return time.Since(g.warmedAt) > 30*time.Minute
}

func (g *Gated) warmed() {
	g.mu.Lock()
	g.warmedAt = time.Now()
	g.mu.Unlock()
}
