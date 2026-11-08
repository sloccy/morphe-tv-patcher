package runner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/sloccy/morphe-tv-patcher/internal/store"
)

// Reach is how a TV could be reached the last time the watch looked.
type Reach struct {
	// State is "fixed" (the fixed ADB port answers), "wireless" (only Wireless debugging's random
	// port does) or "down" (neither).
	State string `json:"state"`
	Port  int    `json:"port,omitempty"`
	// Since is when State last changed.
	Since time.Time `json:"since"`
	// Restored is when the watch last put the fixed port back after a restart.
	Restored time.Time `json:"restored,omitzero"`
	// Note says why putting the fixed port back did not work, while it does not.
	Note string `json:"note,omitempty"`
}

// watchState is what the watch remembers between ticks. Only the Loop goroutine touches it.
type watchState struct {
	fails map[string]int       // device id -> restores that failed in a row
	next  map[string]time.Time // device id -> not before this time
}

// watch looks at every TV with plain TCP dials, which open no ADB session and so show no notice
// on the TV. It records how each can be reached, puts the fixed ADB port back after a restart
// (the one time it connects), and says on Telegram when a TV has been silent for a long time.
func (r *Runner) watch(ctx context.Context) {
	if r.Cfg.WatchEvery <= 0 {
		return
	}
	devs := r.Store.Settings().Devices
	keep := map[string]bool{}
	for _, d := range devs {
		keep[d.ID] = true
		if ctx.Err() != nil {
			return
		}
		// A TV that was never connected to has nothing to keep reachable yet, and nobody to alarm.
		if _, ok := r.Store.DeviceInfo(d.ID); !ok {
			continue
		}
		r.watchOne(ctx, d.ID, d.Host, d.Name)
	}
	r.mu.Lock()
	for id := range r.status.Reach {
		if !keep[id] {
			delete(r.status.Reach, id)
		}
	}
	r.mu.Unlock()
}

func (r *Runner) watchOne(ctx context.Context, id, host, name string) {
	fixed := r.Cfg.TVFixedADBPort
	var known []int
	if fixed > 0 {
		known = append(known, fixed)
	}
	if p := r.Store.Port(id); p > 0 && p != fixed {
		known = append(known, p)
	}
	port, ok := r.ADB.Probe(ctx, host, known...)
	switch {
	case !ok:
		r.noteReach(id, "down", 0, "", false)
		r.silent(ctx, id, name)
	case fixed > 0 && port == fixed:
		r.noteReach(id, "fixed", port, "", false)
		r.answered(ctx, id, name, port)
	default:
		_ = r.Store.SetPort(id, port) // where a run finds it without searching
		r.noteReach(id, "wireless", port, r.noteOf(id), false)
		r.answered(ctx, id, name, port)
		if fixed > 0 {
			r.restoreFixed(ctx, id, name)
		}
	}
}

func (r *Runner) noteOf(id string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.status.Reach[id].Note
}

// noteReach records how a TV can be reached now. restored marks the moment the fixed port was
// put back.
func (r *Runner) noteReach(id, state string, port int, note string, restored bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.status.Reach == nil {
		r.status.Reach = map[string]Reach{}
	}
	cur := r.status.Reach[id]
	if cur.State != state {
		cur.Since = r.Now()
	}
	cur.State, cur.Port, cur.Note = state, port, note
	if state == "fixed" {
		cur.Note = ""
	}
	if restored {
		cur.Restored = r.Now()
	}
	r.status.Reach[id] = cur
}

// silent counts a tick with no answer and, once the TV has been silent for WatchAlertAfter, says
// what to do about it.
func (r *Runner) silent(ctx context.Context, id, name string) {
	if r.Cfg.WatchAlertAfter <= 0 {
		return
	}
	after := max(1, int((r.Cfg.WatchAlertAfter+r.Cfg.WatchEvery-1)/r.Cfg.WatchEvery))
	if !r.noteFailureAfter("reach:"+id, errors.New("not answering"), false, after) || r.Notify == nil {
		return
	}
	r.Notify(ctx, "PROBLEMS", fmt.Sprintf("%s has not answered for %s. If it is on, Android has switched Wireless debugging off, "+
		"most likely because it joined an access point it has not been told to trust. On the TV: Settings > Developer options > "+
		"Wireless debugging > on, and tick \"Always allow on this network\" if it asks (once for each access point and band). "+
		"The pairing is kept.", name, r.Cfg.WatchAlertAfter.Round(time.Minute)))
}

// answered clears the silence counter and, if a message had gone out about it, says it is over.
func (r *Runner) answered(ctx context.Context, id, name string, port int) {
	if r.noteRecovered("reach:"+id) && r.Notify != nil {
		r.Notify(ctx, "updated", fmt.Sprintf("%s is answering again (port %d).", name, port))
	}
}

// restoreFixed puts the fixed ADB port back on a TV that is reachable only on Wireless
// debugging's random port, which is what a restart leaves behind. A failure is waited out with a
// growing pause, so a TV that cannot take it is not connected to every few minutes.
func (r *Runner) restoreFixed(ctx context.Context, id, name string) {
	w := &r.watching
	if w.fails == nil {
		w.fails, w.next = map[string]int{}, map[string]time.Time{}
	}
	if r.Now().Before(w.next[id]) {
		return
	}
	fail := func(why string) {
		w.fails[id]++
		pause := min(r.Cfg.WatchEvery<<min(w.fails[id], 6), time.Hour)
		w.next[id] = r.Now().Add(pause)
		r.mu.Lock()
		cur := r.status.Reach[id]
		cur.Note = why
		r.status.Reach[id] = cur
		r.mu.Unlock()
		slog.Warn("could not put the fixed ADB port back", "device", name, "why", why, "retry in", pause)
	}
	var dev store.Device
	for _, d := range r.Store.Settings().Devices {
		if d.ID == id {
			dev = d
		}
	}
	if dev.ID == "" {
		return
	}
	c, err := r.connect(ctx, dev)
	if err != nil {
		fail(err.Error())
		return
	}
	for _, l := range r.fixedPortStep(ctx, &c, true) {
		if !l.OK {
			fail(l.Message)
			return
		}
	}
	w.fails[id] = 0
	delete(w.next, id)
	r.noteReach(id, "wireless", r.Store.Port(id), "", true)
	slog.Info("put the fixed ADB port back", "device", name, "port", r.Cfg.TVFixedADBPort)
}
