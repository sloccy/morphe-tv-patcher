package runner

import (
	"context"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sloccy/morphe-tv-patcher/internal/store"
)

const (
	probeTimeout = 90 * time.Second // one probe, including its wait for the APKMirror gate
	retryAfter   = 30 * time.Minute // a probe that learned nothing is not repeated sooner
)

// Learning is what the page shows about background CPU checks.
type Learning struct {
	Pending     []string  `json:"pending"`               // app names still waiting
	PausedUntil time.Time `json:"paused_until,omitzero"` // APKMirror asked us to slow down
}

type job struct {
	src     store.Source
	pkg     string
	name    string
	version string
	done    chan struct{}
}

// learner finds out which CPU builds apps offer, one APKMirror lookup at a time, in the
// background. Asking twice for the same app version never repeats the work.
type learner struct {
	mu      sync.Mutex
	queue   []*job
	byKey   map[string]*job      // queued or running
	failed  map[string]time.Time // key -> when the last attempt learned nothing
	running bool
}

func jobKey(pkg, version string) string { return pkg + "@" + version }

// want queues an app version unless it is known, already queued, or failed recently. The
// returned channel closes when the answer is in (or at once when there is nothing to wait for).
func (r *Runner) want(ctx context.Context, src store.Source, pkg, name, version string) <-chan struct{} {
	closed := make(chan struct{})
	close(closed)
	if r.Deps.Probe == nil {
		return closed
	}
	if _, ok := r.Store.Builds(pkg, version); ok {
		return closed
	}
	k := jobKey(pkg, version)
	l := &r.learn
	l.mu.Lock()
	defer l.mu.Unlock()
	if j, ok := l.byKey[k]; ok {
		return j.done
	}
	if t, ok := l.failed[k]; ok && r.Now().Sub(t) < retryAfter {
		return closed
	}
	j := &job{src: src, pkg: pkg, name: name, version: version, done: make(chan struct{})}
	if l.byKey == nil {
		l.byKey = map[string]*job{}
	}
	l.byKey[k] = j
	l.queue = append(l.queue, j)
	if !l.running {
		l.running = true
		go r.learnLoop(context.WithoutCancel(ctx)) // outlives the request that asked
	}
	return j.done
}

func (r *Runner) learnLoop(base context.Context) {
	l := &r.learn
	for {
		l.mu.Lock()
		if len(l.queue) == 0 {
			l.running = false
			l.mu.Unlock()
			return
		}
		j := l.queue[0]
		l.queue = l.queue[1:]
		l.mu.Unlock()

		ctx, cancel := context.WithTimeout(base, probeTimeout)
		avail, err := r.Deps.Probe(ctx, j.src, j.pkg, j.name, j.version)
		cancel()
		switch {
		case err != nil:
			slog.Info("could not learn which CPU builds an app offers", "app", j.name, "version", j.version, "err", err)
		case len(avail) == 0:
			slog.Info("APKMirror does not say which CPU builds an app offers", "app", j.name, "version", j.version)
		default:
			_ = r.Store.SetBuilds(j.pkg, j.version, avail)
			slog.Info("learned CPU builds", "app", j.name, "version", j.version, "builds", strings.Join(avail, ","))
		}
		l.mu.Lock()
		if len(avail) == 0 {
			if l.failed == nil {
				l.failed = map[string]time.Time{}
			}
			l.failed[jobKey(j.pkg, j.version)] = r.Now()
		}
		delete(l.byKey, jobKey(j.pkg, j.version))
		l.mu.Unlock()
		close(j.done)
	}
}

// checking says whether a lookup for this app version is queued or running.
func (r *Runner) checking(pkg, version string) bool {
	r.learn.mu.Lock()
	defer r.learn.mu.Unlock()
	_, ok := r.learn.byKey[jobKey(pkg, version)]
	return ok
}

// Learning reports the checks still waiting and whether APKMirror has paused them.
func (r *Runner) Learning() Learning {
	l := &r.learn
	l.mu.Lock()
	names := make([]string, 0, len(l.byKey))
	seen := map[string]bool{}
	for _, j := range l.byKey {
		if !seen[j.name] {
			seen[j.name] = true
			names = append(names, j.name)
		}
	}
	l.mu.Unlock()
	sort.Strings(names)
	out := Learning{Pending: names}
	if r.Deps.PausedUntil != nil {
		out.PausedUntil = r.Deps.PausedUntil()
	}
	return out
}

// needs lists the app versions that the given devices would receive and whose CPU builds are
// not known yet. Devices whose CPU is unknown are skipped (nothing to compare against).
func (r *Runner) needs(st store.Settings, deviceIDs []string) []job {
	lists := r.Lists()
	var out []job
	seen := map[string]bool{}
	for _, id := range deviceIDs {
		if info, ok := r.Store.DeviceInfo(id); !ok || len(info.ABIs) == 0 {
			continue
		}
		for _, e := range store.Effective(st, id) {
			l := lists[e.Source]
			if l == nil {
				continue
			}
			app, ok := r.appFor(l, e.Package)
			if !ok || app.Target == "" || seen[jobKey(e.Package, app.Target)] {
				continue
			}
			seen[jobKey(e.Package, app.Target)] = true
			if _, known := r.Store.Builds(e.Package, app.Target); known {
				continue
			}
			src, _ := r.Store.Source(e.Source)
			out = append(out, job{src: src, pkg: e.Package, name: app.Name, version: app.Target})
		}
	}
	return out
}

// LearnSelected queues background checks for every app already selected on a device. It
// returns at once.
func (r *Runner) LearnSelected(ctx context.Context) {
	st := r.Store.Settings()
	ids := make([]string, 0, len(st.Devices))
	for _, d := range st.Devices {
		ids = append(ids, d.ID)
	}
	for _, j := range r.needs(st, ids) {
		r.want(ctx, j.src, j.pkg, j.name, j.version)
	}
}

// Warnings are the apps each saved device is known not to be able to run, judged from what has
// been learned so far. It never touches the network.
func (r *Runner) Warnings() []Problem {
	st := r.Store.Settings()
	ids := make([]string, 0, len(st.Devices))
	for _, d := range st.Devices {
		ids = append(ids, d.ID)
	}
	problems, _ := r.judge(st, ids)
	return problems
}
