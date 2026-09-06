package main

import (
	"context"
	"sync"
	"time"

	"github.com/runbear-io/beardrive/internal/remote"
)

// Viewer reads made in the Mac app, on their way to the project's hub.
//
// The sidecar answers the viewer routes from local state and keeps no
// ReadLedger, so before this a person reading a file in the app was counted
// nowhere — while the same file opened in the web app recorded a human read.
// The dashboard's reads-x-staleness quadrant therefore under-reported anyone
// who browses on their Mac, which is the reader the app exists for.
//
// Reported as HUMAN, not through the agent path a device would use: the actor
// is a person, and the hub resolves it from this device's own token. Filing
// them as device traffic would not have closed the gap, it would have moved
// the error somewhere harder to see.
//
// ponytail: one in-memory set, dropped on quit. Read telemetry is allowed to
// lose a few rows — the hub debounces to one visit per path per ten minutes
// anyway, so a lost flush costs at most one bucket. Persist it like the CLI's
// read spool only if that ever stops being true.

const readFlushEvery = 30 * time.Second

var readSpool = struct {
	mu sync.Mutex
	by map[string]map[string]bool // hub project id -> path set
}{by: map[string]map[string]bool{}}

// spoolRead queues one viewer read. Called on the request path, so it does
// nothing but take a lock: the flush is somebody else's problem.
func spoolRead(project, path string) {
	if project == "" || path == "" {
		return
	}
	readSpool.mu.Lock()
	defer readSpool.mu.Unlock()
	if readSpool.by[project] == nil {
		readSpool.by[project] = map[string]bool{}
	}
	readSpool.by[project][path] = true
}

// drainReads takes everything queued so far, leaving the spool empty.
func drainReads() map[string][]string {
	readSpool.mu.Lock()
	defer readSpool.mu.Unlock()
	if len(readSpool.by) == 0 {
		return nil
	}
	out := make(map[string][]string, len(readSpool.by))
	for project, paths := range readSpool.by {
		list := make([]string, 0, len(paths))
		for p := range paths {
			list = append(list, p)
		}
		out[project] = list
	}
	readSpool.by = map[string]map[string]bool{}
	return out
}

// flushReads reports one round to each project's own hub. Silent on every
// failure, by the package rule that telemetry never breaks what reported it —
// and lossy on purpose: a report that could not be sent is dropped rather than
// retried, because re-reporting a path the hub already debounced buys nothing.
func flushReads(ctx context.Context) {
	for project, paths := range drainReads() {
		m, ok := desktopMounts()[project]
		if !ok {
			continue // the folder went away while the window was open
		}
		be, err := remote.Open(ctx, m.server+"/p/"+project)
		if err != nil {
			continue
		}
		rr, ok := be.(remote.ReadReporter)
		if !ok {
			be.Close()
			continue
		}
		evs := make([]remote.ReadEvent, 0, len(paths))
		for _, p := range paths {
			evs = append(evs, remote.ReadEvent{Path: p, Time: time.Now().UTC()})
		}
		_ = rr.ReportReads(ctx, remote.ReadKindHuman, evs)
		be.Close()
	}
}

// startReadReporter runs the flush loop for the life of the process.
func startReadReporter(ctx context.Context) {
	go func() {
		t := time.NewTicker(readFlushEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				flushReads(context.WithoutCancel(ctx)) // best effort on the way out
				return
			case <-t.C:
				flushReads(ctx)
			}
		}
	}()
}
