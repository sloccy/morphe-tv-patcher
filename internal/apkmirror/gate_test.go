package apkmirror

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestGateOneAtATimeAndSpaced(t *testing.T) {
	g := NewGate(30*time.Millisecond, time.Minute)
	var in, peak atomic.Int32
	var wg sync.WaitGroup
	start := time.Now()
	for range 4 {
		wg.Go(func() {
			if err := g.acquire(context.Background(), Background); err != nil {
				t.Error(err)
				return
			}
			n := in.Add(1)
			if n > peak.Load() {
				peak.Store(n)
			}
			time.Sleep(5 * time.Millisecond)
			in.Add(-1)
			g.release(nil)
		})
	}
	wg.Wait()
	if peak.Load() != 1 {
		t.Fatalf("peak concurrency %d", peak.Load())
	}
	if el := time.Since(start); el < 90*time.Millisecond {
		t.Fatalf("requests not spaced: %v", el)
	}
}

func TestGateBlockClosesForEveryoneAndGrows(t *testing.T) {
	g := NewGate(0, 50*time.Millisecond)
	if err := g.acquire(context.Background(), Background); err != nil {
		t.Fatal(err)
	}
	g.release(ErrBlocked)
	if err := g.acquire(context.Background(), Foreground); !errors.Is(err, ErrBlocked) {
		t.Fatalf("want blocked, got %v", err)
	}
	if g.BlockedUntil().IsZero() {
		t.Fatal("expected cool-down")
	}
	time.Sleep(60 * time.Millisecond)
	if err := g.acquire(context.Background(), Background); err != nil {
		t.Fatal(err)
	}
	g.release(ErrBlocked) // second strike: doubled
	if d := time.Until(g.BlockedUntil()); d < 70*time.Millisecond {
		t.Fatalf("cool-down did not grow: %v", d)
	}
	time.Sleep(110 * time.Millisecond)
	if err := g.acquire(context.Background(), Background); err != nil {
		t.Fatal(err)
	}
	g.release(nil)
	if g.strikes != 0 {
		t.Fatal("success should reset strikes")
	}
}

func TestGateCancelledWaiterLeaves(t *testing.T) {
	g := NewGate(0, time.Minute)
	_ = g.acquire(context.Background(), Background)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := g.acquire(ctx, Background); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v", err)
	}
	g.release(nil)
	if err := g.acquire(context.Background(), Background); err != nil {
		t.Fatal(err)
	}
	g.release(nil)
}

func TestGateForegroundFirst(t *testing.T) {
	g := NewGate(0, time.Minute)
	_ = g.acquire(context.Background(), Background)
	var mu sync.Mutex
	var order []string
	var wg sync.WaitGroup
	run := func(name string, p Priority) {
		defer wg.Done()
		if g.acquire(context.Background(), p) == nil {
			mu.Lock()
			order = append(order, name)
			mu.Unlock()
			g.release(nil)
		}
	}
	wg.Add(1)
	go run("bg", Background)
	time.Sleep(20 * time.Millisecond)
	wg.Add(1)
	go run("fg", Foreground)
	time.Sleep(20 * time.Millisecond)
	g.release(nil)
	wg.Wait()
	if len(order) != 2 || order[0] != "fg" {
		t.Fatalf("order %v", order)
	}
}
