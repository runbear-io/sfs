package main

import (
	"sort"
	"strings"
	"testing"

	"github.com/runbear-io/beardrive/internal/webapp"
)

// TestDesktopRoutesClassified is the guard the desktop app did not have.
//
// The sidecar's mux overrides a list of routes and falls through to a local
// server for everything else, so a hub route nobody classified is answered
// from a registry that holds no hub metadata — an empty grants list, a project
// with no folder rules. It looks like an answer, not like a failure, which is
// why it shipped twice before anyone noticed.
//
// This is the cheap half of the fix: adding a per-project route to the hub now
// costs one line in desktopRoutes, and adding one without that line costs a
// red build. It cannot tell you a routeLocal decision was WRONG — only that
// somebody made one. Both misses so far were nobody deciding at all.
func TestDesktopRoutesClassified(t *testing.T) {
	var hub []string
	for _, pat := range webapp.APIRoutes() {
		if strings.Contains(pat, "{project}") {
			hub = append(hub, pat)
		}
	}
	if len(hub) == 0 {
		t.Fatal("APIRoutes reported no per-project routes at all — the recorder is broken, not the table")
	}

	listed := map[string]bool{}
	for _, rt := range desktopRoutes {
		if listed[rt.pattern] {
			t.Errorf("desktopRoutes lists %q twice; the mux would panic on the second", rt.pattern)
		}
		listed[rt.pattern] = true
		if rt.mode == routeLocal && strings.TrimSpace(rt.why) == "" {
			t.Errorf("%q is routeLocal with no reason — the reason is the point: say why "+
				"this machine can answer it correctly", rt.pattern)
		}
	}

	known := map[string]bool{}
	for _, pat := range hub {
		known[pat] = true
		if !listed[pat] {
			t.Errorf("hub route %q is not classified for the desktop app.\n"+
				"Add it to desktopRoutes in desktop.go as routeProxy, or as routeLocal with a reason.\n"+
				"Unclassified means the Mac app answers it from local state, which for anything the\n"+
				"hub owns is a plausible wrong answer rather than an error.", pat)
		}
	}
	var stale []string
	for pat := range listed {
		if !known[pat] {
			stale = append(stale, pat)
		}
	}
	sort.Strings(stale)
	for _, pat := range stale {
		t.Errorf("desktopRoutes lists %q, which the hub no longer serves — renamed or removed?", pat)
	}
}
