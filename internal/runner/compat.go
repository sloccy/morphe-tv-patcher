package runner

import (
	"context"
	"fmt"
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

// learnWait bounds how long a save waits for the CPU checks it queued. Lookups run one at a
// time through APKMirror's gate, so a couple of apps usually finish; the rest carry on in the
// background and show up as warnings if they turn out to be a problem.
const learnWait = 12 * time.Second

// Compat checks what each of the given devices would receive (from the settings snapshot, which
// may be a preview of an unsaved change) against what is known about its CPU and about the CPU
// builds each app offers. Missing facts are queued for a background lookup, and Compat waits a
// few seconds for them. problems are known mismatches. unchecked lists what could not be judged
// (a device never reached, or an app whose builds are still being looked up); those do not block
// anything, and a mismatch found later shows up as a warning.
func (r *Runner) Compat(ctx context.Context, st store.Settings, deviceIDs []string) (problems []Problem, unchecked []string) {
	var waits []<-chan struct{}
	for _, j := range r.needs(st, deviceIDs) {
		waits = append(waits, r.want(ctx, j.src, j.pkg, j.name, j.version))
	}
	if len(waits) > 0 {
		timer := time.NewTimer(learnWait)
		defer timer.Stop()
	wait:
		for _, w := range waits {
			select {
			case <-w:
			case <-timer.C:
				break wait
			case <-ctx.Done():
				break wait
			}
		}
	}
	return r.judge(st, deviceIDs)
}

// judge compares devices with what is already known. It never touches the network.
func (r *Runner) judge(st store.Settings, deviceIDs []string) (problems []Problem, unchecked []string) {
	lists := r.Lists()
	seenUnchecked := map[string]bool{}
	note := func(msg string) {
		if !seenUnchecked[msg] {
			seenUnchecked[msg] = true
			unchecked = append(unchecked, msg)
		}
	}
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
			app, ok := r.appFor(l, e.Package)
			if !ok || app.Target == "" {
				continue
			}
			if !haveInfo || len(info.ABIs) == 0 {
				note(fmt.Sprintf("%s has not been reached yet, so its CPU is unknown (pair it first); %s was not checked", dev.Name, app.Name))
				continue
			}
			builds, known := r.Store.Builds(e.Package, app.Target)
			if !known {
				if r.checking(e.Package, app.Target) {
					note(fmt.Sprintf("still checking which CPUs %s has builds for", app.Name))
				} else {
					note(fmt.Sprintf("could not check which CPUs %s has builds for", app.Name))
				}
				continue
			}
			if !runnable(builds, info.ABIs) {
				problems = append(problems, Problem{Device: dev.Name, Package: e.Package, Name: app.Name,
					Reason: fmt.Sprintf("%s %s only has %s builds; %s runs %s. The patches cannot give it this app.",
						app.Name, app.Target, strings.Join(builds, ", "), dev.Name, strings.Join(info.ABIs, ", "))})
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
