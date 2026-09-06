package webapp

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// A person reading in the Mac app has to land in the ledger as a HUMAN read,
// keyed by their account — not as agent traffic keyed by a device.
//
// The sidecar answers the viewer routes from local state and keeps no ledger,
// so those reads reached nothing at all and the dashboard under-reported
// anyone who browses there. The obvious fix — post them through the existing
// report route — would have been worse than the gap: that route files reads as
// agent traffic, so a person's browsing would have shown up as a device's.
func humanRead(t *testing.T, h http.Handler, projectID string, c *http.Cookie, paths ...string) int {
	t.Helper()
	reads := make([]map[string]any, 0, len(paths))
	for _, p := range paths {
		reads = append(reads, map[string]any{"path": p})
	}
	// No X-Bdrive-Device header on purpose: a human read is keyed by the
	// account, so the device requirement must not apply to it.
	rec := secaud2Do(t, h, "POST", "/api/p/"+projectID+"/reads",
		map[string]any{"kind": "human", "reads": reads}, c, nil)
	if rec.Code != 200 {
		t.Fatalf("human read report: %d %s", rec.Code, rec.Body)
	}
	var out struct {
		Accepted int `json:"accepted"`
	}
	json.Unmarshal(rec.Body.Bytes(), &out)
	return out.Accepted
}

func TestReadReportCountsAHumanReadAsHuman(t *testing.T) {
	h, srv, c, p := secaud2ReadsHub(t)
	secauthzUpload(t, h, p.ID, "notes/read-here.md", "x", c["alice"])

	if n := humanRead(t, h, p.ID, c["alice"], "notes/read-here.md"); n != 1 {
		t.Fatalf("accepted %d human reads, want 1", n)
	}

	// It is human traffic — the same shape the hub's own viewer records, so the
	// web app and the Mac app agree about what a person reading looks like.
	e := srv.Reads.Heat(p.ID, "", time.Time{})["notes/read-here.md"]
	if e.Human != 1 {
		t.Errorf("human count = %d, want 1: the read reached no human bucket", e.Human)
	}
	if e.Agent != 0 {
		t.Errorf("agent count = %d, want 0: a person browsing is not a device", e.Agent)
	}
	if e.Readers != 1 {
		t.Errorf("distinct readers = %d, want 1: the actor should be the signed-in account", e.Readers)
	}

	// And it stays out of the agent surface, which is the one place /heat
	// reports an actor id at all.
	byDev := secaud2ByDevice(t, h, p.ID, c["alice"])
	for dev, folders := range byDev {
		if folders["notes"] != 0 {
			t.Errorf("a human read showed up under device %q as agent traffic", dev)
		}
	}
}

// The actor is the hub's answer about the caller, never the body's — so there
// is nothing to key a human read to without a session.
func TestHumanReadReportNeedsASignedInAccount(t *testing.T) {
	h, _, _, p := secaud2ReadsHub(t)
	rec := secaud2Do(t, h, "POST", "/api/p/"+p.ID+"/reads",
		map[string]any{"kind": "human", "reads": []map[string]any{{"path": "notes/x.md"}}}, nil, nil)
	if rec.Code == 200 {
		t.Fatalf("an unauthenticated human read report was accepted: %d %s", rec.Code, rec.Body)
	}
}

// Every client that predates the desktop app sends no kind and means agent.
func TestReadReportWithoutAKindIsStillAgent(t *testing.T) {
	h, srv, c, p := secaud2ReadsHub(t)
	const dev = "alice-laptop-5b73"
	secaud2Sync(t, h, p.ID, c["alice"], dev, "Alice's MacBook")
	secauthzUpload(t, h, p.ID, "notes/legacy.md", "x", c["alice"])

	if rec := secaud2Report(t, h, p.ID, c["alice"], dev, "notes/legacy.md"); rec.Code != 200 {
		t.Fatalf("legacy report: %d %s", rec.Code, rec.Body)
	}
	e := srv.Reads.Heat(p.ID, "", time.Time{})["notes/legacy.md"]
	if e.Agent != 1 || e.Human != 0 {
		t.Errorf("a kindless report landed as human=%d agent=%d; every existing client means agent",
			e.Human, e.Agent)
	}
}
