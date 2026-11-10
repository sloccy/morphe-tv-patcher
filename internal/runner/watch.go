package runner

import (
	"context"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"net"
	"strconv"
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
	// The connection the adb server already holds is the normal case: it needs no traffic to the
	// TV at all. Only when it has gone is the TV looked for and connected to again.
	if live := r.ADB.LivePort(ctx, host, fixed); live > 0 {
		r.reached(ctx, id, host, name, live)
		return
	}
	var known []int
	if fixed > 0 {
		known = append(known, fixed)
	}
	if p := r.Store.Port(id); p > 0 && p != fixed {
		known = append(known, p)
	}
	port, ok := r.ADB.Probe(ctx, host, known...)
	if !ok {
		r.noteReach(id, "down", 0, "", false)
		r.silent(ctx, id, name)
		return
	}
	if fixed == 0 || port != fixed {
		_ = r.Store.SetPort(id, port) // where a run finds it without searching
	}
	if fixed > 0 && port == fixed {
		r.noteReach(id, "fixed", port, "", false)
	} else {
		r.noteReach(id, "wireless", port, r.noteOf(id), false)
	}
	r.answered(id)
	r.reconnect(ctx, id, name)
}

// reached records a TV whose connection is still up and, on a random port, puts the fixed one back.
func (r *Runner) reached(ctx context.Context, id, host, name string, port int) {
	fixed := r.Cfg.TVFixedADBPort
	if fixed > 0 && port == fixed {
		r.noteReach(id, "fixed", port, "", false)
		r.answered(id)
		return
	}
	_ = r.Store.SetPort(id, port)
	r.noteReach(id, "wireless", port, r.noteOf(id), false)
	r.answered(id)
	if fixed > 0 {
		if p, ok := r.ADB.Probe(ctx, host, fixed); !ok || p != fixed {
			r.restoreFixed(ctx, id, name)
		}
	}
}

// reconnect brings back a dropped connection to a TV that answers, then puts the fixed port back if
// the connection came back on the random one. A failure is waited out with a growing pause, so a TV
// that refuses this server is not dialled every few minutes.
func (r *Runner) reconnect(ctx context.Context, id, name string) {
	if r.watchPaused(id) {
		return
	}
	dev, ok := r.device(id)
	if !ok {
		return
	}
	c, err := r.connect(ctx, dev)
	if err != nil {
		r.watchFail(id, name, "could not reconnect: "+err.Error())
		return
	}
	r.watchOK(id)
	slog.Info("reconnected to the TV", "device", name, "serial", c.serial)
	if fixed := r.Cfg.TVFixedADBPort; fixed > 0 && c.serial != net.JoinHostPort(dev.Host, strconv.Itoa(fixed)) {
		r.restoreFixedOn(ctx, id, name, &c)
	}
}

func (r *Runner) device(id string) (store.Device, bool) {
	for _, d := range r.Store.Settings().Devices {
		if d.ID == id {
			return d, true
		}
	}
	return store.Device{}, false
}

// watchPaused says whether a failed reconnect or restore is still being waited out.
func (r *Runner) watchPaused(id string) bool {
	w := &r.watching
	return r.Now().Before(w.next[id])
}

func (r *Runner) watchOK(id string) {
	w := &r.watching
	if w.fails != nil {
		w.fails[id] = 0
		delete(w.next, id)
	}
}

// watchFail records why the TV could not be put right and waits longer each time.
func (r *Runner) watchFail(id, name, why string) {
	w := &r.watching
	if w.fails == nil {
		w.fails, w.next = map[string]int{}, map[string]time.Time{}
	}
	w.fails[id]++
	pause := min(r.Cfg.WatchEvery<<min(w.fails[id], 6), time.Hour)
	w.next[id] = r.Now().Add(pause)
	r.mu.Lock()
	if r.status.Reach == nil {
		r.status.Reach = map[string]Reach{}
	}
	cur := r.status.Reach[id]
	cur.Note = why
	r.status.Reach[id] = cur
	r.mu.Unlock()
	slog.Warn("could not keep the TV connected", "device", name, "why", why, "retry in", pause)
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
	r.Notify(ctx, fmt.Sprintf("<b>Problems</b>\n%s: not answering for %s. Turn Wireless debugging back on",
		html.EscapeString(name), r.Cfg.WatchAlertAfter.Round(time.Minute)))
}

// answered clears the silence counter. Coming back is not worth a message of its own.
func (r *Runner) answered(id string) { r.noteRecovered("reach:" + id) }

// restoreFixed puts the fixed ADB port back on a TV that is reachable only on Wireless
// debugging's random port, which is what a restart leaves behind.
func (r *Runner) restoreFixed(ctx context.Context, id, name string) {
	if r.watchPaused(id) {
		return
	}
	r.restoreFixedOn(ctx, id, name, nil)
}

// restoreFixedOn is restoreFixed over a connection already made (nil = connect first).
func (r *Runner) restoreFixedOn(ctx context.Context, id, name string, c *conn) {
	if c == nil {
		dev, ok := r.device(id)
		if !ok {
			return
		}
		got, err := r.connect(ctx, dev)
		if err != nil {
			r.watchFail(id, name, err.Error())
			return
		}
		c = &got
	}
	for _, l := range r.fixedPortStep(ctx, c, true) {
		if !l.OK {
			r.watchFail(id, name, l.Message)
			return
		}
	}
	r.watchOK(id)
	r.noteReach(id, "wireless", r.Store.Port(id), "", true)
	slog.Info("put the fixed ADB port back", "device", name, "port", r.Cfg.TVFixedADBPort)
}
