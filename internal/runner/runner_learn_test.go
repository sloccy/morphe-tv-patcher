package runner

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sloccy/morphe-tv-patcher/internal/store"
)

func TestLearnerProbesEachAppOnceOneAtATime(t *testing.T) {
	rg := newRig(t)
	rg.r.RefreshLists(context.Background())
	a := rg.knownDevice(t, "A", "10.0.0.1", "arm64-v8a")
	_, _ = rg.st.AddSet("S", []store.AppChoice{{Package: netflix}, {Package: prime}}, []string{a.ID})
	var calls, active, peak atomic.Int32
	rg.r.Deps.Probe = func(ctx context.Context, src store.Source, pkg, name, version string) ([]string, error) {
		calls.Add(1)
		n := active.Add(1)
		if n > peak.Load() {
			peak.Store(n)
		}
		time.Sleep(20 * time.Millisecond)
		active.Add(-1)
		return []string{"armeabi-v7a"}, nil
	}
	var wg sync.WaitGroup
	for range 10 { // ten identical saves at once
		wg.Go(func() { rg.r.Compat(context.Background(), rg.st.Settings(), []string{a.ID}) })
	}
	wg.Wait()
	if calls.Load() != 2 || peak.Load() != 1 {
		t.Fatalf("each app once, one at a time: calls %d peak %d", calls.Load(), peak.Load())
	}
	if l := rg.r.Learning(); len(l.Pending) != 0 {
		t.Fatalf("%+v", l)
	}
}

func TestCompatDoesNotWaitForSlowLookupsAndWarnsLater(t *testing.T) {
	rg := newRig(t)
	rg.r.RefreshLists(context.Background())
	a := rg.knownDevice(t, "A", "10.0.0.1", "arm64-v8a")
	_, _ = rg.st.AddSet("S", []store.AppChoice{{Package: netflix}}, []string{a.ID})
	release := make(chan struct{})
	rg.r.Deps.Probe = func(ctx context.Context, src store.Source, pkg, name, version string) ([]string, error) {
		<-release
		return []string{"armeabi-v7a"}, nil
	}
	done := make(chan struct{})
	go func() {
		// this save is still waiting when the answer arrives, so it sees the conflict
		problems, _ := rg.r.Compat(context.Background(), rg.st.Settings(), []string{a.ID})
		if len(problems) != 1 {
			t.Errorf("%+v", problems)
		}
		close(done)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	// cancelling the request ends the wait, but the lookup carries on in the background
	problems, _ := rg.r.Compat(ctx, rg.st.Settings(), []string{a.ID})
	if len(problems) != 0 {
		t.Fatalf("%+v", problems)
	}
	if l := rg.r.Learning(); len(l.Pending) != 1 {
		t.Fatalf("pending %+v", l)
	}
	if len(rg.r.Warnings()) != 0 {
		t.Fatal("nothing known yet")
	}
	close(release)
	<-done
	deadline := time.Now().Add(time.Second)
	for len(rg.r.Warnings()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if len(rg.r.Warnings()) != 1 {
		t.Fatalf("a conflict found later is a warning: %+v", rg.r.Warnings())
	}
}

func TestFailedLookupsAreNotRepeatedAtOnce(t *testing.T) {
	rg := newRig(t)
	rg.r.RefreshLists(context.Background())
	a := rg.knownDevice(t, "A", "10.0.0.1", "arm64-v8a")
	_, _ = rg.st.AddSet("S", []store.AppChoice{{Package: netflix}}, []string{a.ID})
	var calls atomic.Int32
	rg.r.Deps.Probe = func(context.Context, store.Source, string, string, string) ([]string, error) {
		calls.Add(1)
		return nil, context.DeadlineExceeded
	}
	for range 3 {
		rg.r.Compat(context.Background(), rg.st.Settings(), []string{a.ID})
	}
	if calls.Load() != 1 {
		t.Fatalf("a failure is remembered for a while; probed %d times", calls.Load())
	}
}
