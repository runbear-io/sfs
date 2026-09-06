package syncer

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/runbear-io/beardrive/internal/remote"
)

// readReportingRemote wraps a backend with the hub's ReadReporter capability,
// standing in for the https:// backend in the multi-device harness.
type readReportingRemote struct {
	remote.Backend

	mu      sync.Mutex
	fail    bool
	reports [][]remote.ReadEvent
}

func (r *readReportingRemote) ReportReads(_ context.Context, _ string, reads []remote.ReadEvent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail {
		return fmt.Errorf("hub unreachable")
	}
	cp := make([]remote.ReadEvent, len(reads))
	copy(cp, reads)
	r.reports = append(r.reports, cp)
	return nil
}

func (r *readReportingRemote) setFail(v bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fail = v
}

func (r *readReportingRemote) all() [][]remote.ReadEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reports
}

// TestAgentReadReporting drives the read spool through real sync cycles: the
// queued reads flush to a reporting hub (deduped), survive an unreachable hub
// and retry, and never disturb the sync result itself.
func TestAgentReadReporting(t *testing.T) {
	hub := &readReportingRemote{Backend: sharedRemote(t)}
	a := newDevice(t, "deva", hub)
	write(t, a.Folder, "wiki/a.md", "content")

	// The agent read a.md twice and b.md once before this cycle.
	a.Store.LogRead("wiki/a.md", "")
	a.Store.LogRead("wiki/a.md", "")
	a.Store.LogRead("b.md", "")
	res := cycle(t, a)
	if !res.Pushed {
		t.Fatal("cycle should have pushed")
	}
	reports := hub.all()
	if len(reports) != 1 || len(reports[0]) != 2 {
		t.Fatalf("reports = %+v, want one deduped batch of 2", reports)
	}
	if reports[0][0].Path != "wiki/a.md" || reports[0][1].Path != "b.md" {
		t.Fatalf("batch = %+v", reports[0])
	}
	// Drained: an idle cycle reports nothing.
	cycle(t, a)
	if len(hub.all()) != 1 {
		t.Fatal("empty spool still produced a report")
	}

	// Hub down: the cycle still succeeds and the batch stays queued.
	hub.setFail(true)
	a.Store.LogRead("wiki/a.md", "")
	if res := cycle(t, a); res.Offline {
		t.Fatal("a failed read report must not mark the cycle offline")
	}
	if len(hub.all()) != 1 {
		t.Fatal("failed report should not have landed")
	}
	// Hub back: the next cycle retries the same batch.
	hub.setFail(false)
	cycle(t, a)
	reports = hub.all()
	if len(reports) != 2 || len(reports[1]) != 1 || reports[1][0].Path != "wiki/a.md" {
		t.Fatalf("retry reports = %+v", reports)
	}

	// A backend without the capability (plain object store) is untouched by
	// queued reads: the cycle runs, the spool just keeps waiting.
	b := newDevice(t, "devb", sharedRemote(t))
	write(t, b.Folder, "x.md", "x")
	b.Store.LogRead("x.md", "")
	cycle(t, b)
	if evs, err := b.Store.PendingReads(); err != nil || len(evs) != 1 {
		t.Fatalf("spool on a hubless device = %v, %v; want the read still queued", evs, err)
	}
}

// TestSessionCarriesThroughTwoDevices is the multi-device shape of the join:
// a device syncing under an agent session stamps that session onto every op
// it commits AND onto every read it reports, its peer converges on ops that
// carry the id, and a device with no session leaves both empty — so a run
// card can never claim another device's work.
func TestSessionCarriesThroughTwoDevices(t *testing.T) {
	shared := sharedRemote(t)
	hubA := &readReportingRemote{Backend: shared}
	hubB := &readReportingRemote{Backend: shared}
	a := newDevice(t, "deva", hubA)
	b := newDevice(t, "devb", hubB)

	// Device A works inside an agent session: it reads two files and writes one.
	a.SessionID = "8f21e4"
	write(t, a.Folder, "wiki/a.md", "written by the run")
	a.Store.LogRead("wiki/a.md", "8f21e4")
	a.Store.LogRead("wiki/reference.md", "8f21e4")
	cycle(t, a)

	if reports := hubA.all(); len(reports) != 1 || len(reports[0]) != 2 {
		t.Fatalf("reports = %+v, want one batch of 2", reports)
	} else {
		for _, e := range reports[0] {
			if e.Session != "8f21e4" {
				t.Fatalf("reported read %+v lost its session", e)
			}
		}
	}

	// Device B, no session at all: its own op carries none, and the read it
	// reports carries none — nothing of B's can land on A's card.
	write(t, b.Folder, "wiki/b.md", "written by a human")
	b.Store.LogRead("wiki/b.md", "")
	cycle(t, b)
	if reports := hubB.all(); len(reports) != 1 || reports[0][0].Session != "" {
		t.Fatalf("sessionless device reported %+v, want an empty session", reports)
	}

	// Both peers converge, and each op keeps the session of the device that
	// wrote it — replay does not touch the field.
	cycle(t, a)
	cycle(t, b)
	for _, d := range []*Session{a, b} {
		ops, err := d.Store.AllOps()
		if err != nil {
			t.Fatal(err)
		}
		seen := map[string]string{}
		for _, op := range ops {
			seen[op.Path] = op.Session
		}
		if seen["wiki/a.md"] != "8f21e4" {
			t.Errorf("%s sees wiki/a.md session %q, want 8f21e4", d.Device.ID, seen["wiki/a.md"])
		}
		if seen["wiki/b.md"] != "" {
			t.Errorf("%s sees wiki/b.md session %q, want empty", d.Device.ID, seen["wiki/b.md"])
		}
	}
	// Convergence itself: both folders hold both files.
	if got, want := snapshotDir(t, a.Folder), snapshotDir(t, b.Folder); len(got) != len(want) {
		t.Fatalf("folders diverged: %v vs %v", got, want)
	}
}
