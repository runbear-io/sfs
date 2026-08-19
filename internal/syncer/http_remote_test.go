package syncer

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/runbear-io/beardrive/internal/remote"
	"github.com/runbear-io/beardrive/internal/webapp"
)

// newHub spins up a bdrive web hub over a fresh storage root and returns the
// test server plus one project.
func newHub(t *testing.T, storage remote.Backend, upload bool) (*httptest.Server, webapp.Project) {
	t.Helper()
	db, err := webapp.OpenProjectDB(filepath.Join(t.TempDir(), "projects.json"))
	if err != nil {
		t.Fatal(err)
	}
	p, _, err := db.GetOrCreate("vol", "")
	if err != nil {
		t.Fatal(err)
	}
	srv := &webapp.Server{
		Root: storage, Projects: db, Refresh: 0,
		// The hub journals its own ops (browser uploads, restore, remove)
		// under this identity — a separate writer from every device's.
		Device: webapp.Identity{ID: "hubdev", Name: "hub", Author: "hub@test"},
		Upload: webapp.UploadConfig{Enabled: upload},
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, p
}

// A device syncing through a bdrive web server (https:// remote) must
// converge with a device talking to the object store directly: the server is
// just a broker, not a different sync model.
func TestSyncThroughWebServer(t *testing.T) {
	storage := sharedRemote(t) // the object store only the server knows about
	ts, p := newHub(t, storage, true)

	viaServer, err := remote.Open(context.Background(), ts.URL+"/p/"+p.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer viaServer.Close()

	a := newDevice(t, "deva", viaServer)                      // storage-blind client
	b := newDevice(t, "devb", remote.Prefixed(storage, p.ID)) // direct-to-storage device

	// client → server → storage → direct device
	write(t, a.Folder, "notes/from-client.md", "hello via server")
	cycle(t, a)
	res := cycle(t, b)
	if res.PulledOps != 1 || read(t, b.Folder, "notes/from-client.md") != "hello via server" {
		t.Fatalf("b did not receive client's file: %+v", res)
	}

	// direct device → storage → server → client
	time.Sleep(10 * time.Millisecond)
	write(t, b.Folder, "notes/from-direct.md", "hello back")
	write(t, b.Folder, "notes/from-client.md", "edited directly")
	cycle(t, b)
	cycle(t, a)
	if read(t, a.Folder, "notes/from-direct.md") != "hello back" {
		t.Fatal("client did not receive direct device's file")
	}
	if read(t, a.Folder, "notes/from-client.md") != "edited directly" {
		t.Fatal("client did not receive the edit")
	}

	// deletes propagate through the server too
	os.Remove(filepath.Join(a.Folder, "notes", "from-direct.md"))
	cycle(t, a)
	cycle(t, b)
	if _, err := os.Stat(filepath.Join(b.Folder, "notes", "from-direct.md")); !os.IsNotExist(err) {
		t.Fatal("delete via server did not propagate")
	}
}

// With uploads disabled on the server, a client can still pull (read-only
// follower) — its pushes report ReadOnly instead of failing the cycle. Not
// Offline: the server answered, it just said no, and retrying forever as if
// the network were down would hide that from the user.
func TestReadOnlyServerClientStillPulls(t *testing.T) {
	storage := sharedRemote(t)
	ts, p := newHub(t, storage, false) // read-only hub

	viaServer, err := remote.Open(context.Background(), ts.URL+"/p/"+p.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer viaServer.Close()

	b := newDevice(t, "devb", remote.Prefixed(storage, p.ID))
	write(t, b.Folder, "shared.md", "server-side truth")
	cycle(t, b)

	a := newDevice(t, "deva", viaServer)
	write(t, a.Folder, "local-only.md", "cannot push this")
	res, err := a.Cycle(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !res.ReadOnly || res.Offline {
		t.Fatalf("push against a read-only server should report ReadOnly, not Offline: %+v", res)
	}
	if read(t, a.Folder, "shared.md") != "server-side truth" {
		t.Fatal("client should still pull from a read-only server")
	}
}

// Undoing a whole agent run at the hub converges like any other change: the
// hub journals the undo under its OWN device, and every other device
// materializes the pre-run content on its next cycle. The repo's convention
// is that a sync feature without a multi-device test is untested where it
// matters — this is that test for BEA-82.
func TestUndoRunConverges(t *testing.T) {
	storage := sharedRemote(t)
	ts, p := newHub(t, storage, true)

	viaServer, err := remote.Open(context.Background(), ts.URL+"/p/"+p.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer viaServer.Close()

	a := newDevice(t, "deva", viaServer)                      // the agent's machine
	b := newDevice(t, "devb", remote.Prefixed(storage, p.ID)) // a teammate

	// Before the run.
	write(t, a.Folder, "notes/plan.md", "the plan, as written by a human")
	cycle(t, a)
	cycle(t, b)
	if read(t, b.Folder, "notes/plan.md") != "the plan, as written by a human" {
		t.Fatal("b never got the pre-run content")
	}

	// The run: one file rewritten, one created, both stamped with the session
	// id the agent hook sets.
	time.Sleep(10 * time.Millisecond)
	a.SessionID = "run-8f21e4"
	write(t, a.Folder, "notes/plan.md", "REWRITTEN BY THE AGENT")
	write(t, a.Folder, "notes/scratch.md", "invented by the agent")
	cycle(t, a)
	a.SessionID = ""
	cycle(t, b)
	if read(t, b.Folder, "notes/scratch.md") != "invented by the agent" {
		t.Fatal("b never saw the run")
	}

	// Undo the whole run from the hub.
	body := strings.NewReader(`{"session":"run-8f21e4","device":"deva"}`)
	resp, err := http.Post(ts.URL+"/api/p/"+p.ID+"/undo-run", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("undo-run: %d %s", resp.StatusCode, out)
	}

	// The teammate converges on the pre-run state without doing anything but
	// syncing, and so does the device that made the mess.
	time.Sleep(10 * time.Millisecond)
	for _, d := range []*Session{b, a} {
		cycle(t, d)
		if got := read(t, d.Folder, "notes/plan.md"); got != "the plan, as written by a human" {
			t.Fatalf("%s has %q after the undo, want the pre-run content", d.Device.ID, got)
		}
		if _, err := os.Stat(filepath.Join(d.Folder, "notes", "scratch.md")); !os.IsNotExist(err) {
			t.Fatalf("%s still has the file the run created", d.Device.ID)
		}
	}

	// The undo is append-only: the agent's own journal still holds every op
	// it ever wrote, and the undo lives in the hub's.
	rc, err := storage.Get(context.Background(), p.ID+"/journal/deva.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	devaJournal, _ := io.ReadAll(rc)
	rc.Close()
	// The run's ops are still there, session id and all: an undo appends, it
	// never rewrites the journal it is undoing.
	if !strings.Contains(string(devaJournal), "run-8f21e4") ||
		!strings.Contains(string(devaJournal), "notes/scratch.md") {
		t.Fatalf("the undo edited the run's own journal — it must only ever append to the hub's:\n%s", devaJournal)
	}
	if _, err := storage.Get(context.Background(), p.ID+"/journal/hubdev.jsonl"); err != nil {
		t.Fatalf("the hub journaled the undo somewhere other than its own journal: %v", err)
	}
}
