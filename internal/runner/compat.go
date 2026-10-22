package runner

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/sloccy/morphe-tv-patcher/internal/abi"
	"github.com/sloccy/morphe-tv-patcher/internal/store"
)

// Problem is an app a device is known not to be able to run.
type Problem struct {
	Device  string `json:"device"`
	Package string `json:"package"`
	Name    string `json:"name"`
	Reason  string `json:"reason"`
}

// probeBudget bounds how long a save may spend asking APKMirror what CPU builds apps offer.
const probeBudget = 25 * time.Second

// Compat checks what each of the given devices would receive (from the settings snapshot, which
// may be a preview of an unsaved change) against what is known about its CPU and about the CPU
// builds each app offers. problems are known mismatches. unchecked lists what could not be
// judged (a device never reached, or an app whose builds could not be learned); those do not
// block anything, a mismatch then shows up as the usual "no build for this CPU" on a run.
func (r *Runner) Compat(ctx context.Context, st store.Settings, deviceIDs []string) (problems []Problem, unchecked []string) {
	ctx, cancel := context.WithTimeout(ctx, probeBudget)
	defer cancel()
	lists := r.Lists()
	seenUnchecked := map[string]bool{}
	note := func(msg string) {
		if !seenUnchecked[msg] {
			seenUnchecked[msg] = true
			unchecked = append(unchecked, msg)
		}
	}
	probed := map[string]bool{} // "pkg@version" already probed during this check
	for _, id := range deviceIDs {
		var dev store.Device
		for _, d := range st.Devices {
			if d.ID == id {
				dev = d
			}
		}
		if dev.ID == "" {
			continue
		}
		info, haveInfo := r.Store.DeviceInfo(id)
		for _, e := range store.Effective(st, id) {
			l := lists[e.Source]
			if l == nil {
				continue // a source that has not loaded: nothing to judge against
			}
			app, ok := l.App(e.Package)
			if !ok || app.Newest == "" {
				continue
			}
			if !haveInfo || len(info.ABIs) == 0 {
				note(fmt.Sprintf("%s has not been reached yet, so its CPU is unknown (pair it first); %s was not checked", dev.Name, app.Name))
				continue
			}
			builds, known := r.Store.Builds(e.Package, app.Newest)
			key := e.Package + "@" + app.Newest
			if !known && r.Deps.Probe != nil && !probed[key] {
				probed[key] = true
				src, _ := r.Store.Source(e.Source)
				avail, err := r.Deps.Probe(ctx, src, e.Package, app.Name, app.Newest)
				if err != nil {
					slog.Info("could not learn which CPU builds an app offers", "package", e.Package, "err", err)
				}
				if len(avail) > 0 {
					_ = r.Store.SetBuilds(e.Package, app.Newest, avail)
					builds, known = avail, true
				}
			} else if !known && probed[key] {
				builds, known = r.Store.Builds(e.Package, app.Newest)
			}
			if !known {
				note(fmt.Sprintf("could not check which CPUs %s has builds for", app.Name))
				continue
			}
			if !runnable(builds, info.ABIs) {
				problems = append(problems, Problem{Device: dev.Name, Package: e.Package, Name: app.Name,
					Reason: fmt.Sprintf("%s %s only has %s builds; %s runs %s. The patches cannot give it this app.",
						app.Name, app.Newest, strings.Join(builds, ", "), dev.Name, strings.Join(info.ABIs, ", "))})
			}
		}
	}
	sort.Slice(problems, func(i, j int) bool {
		if problems[i].Device != problems[j].Device {
			return problems[i].Device < problems[j].Device
		}
		return problems[i].Package < problems[j].Package
	})
	return problems, unchecked
}

// runnable says whether any of the offered builds runs on a device ("*" means a universal build).
func runnable(builds, device []string) bool {
	have := map[string]bool{}
	for _, b := range builds {
		if b == "*" {
			return true
		}
		have[b] = true
	}
	_, ok := abi.Rank(have, device)
	return ok
}
