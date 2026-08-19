package webapp

// E2E hub harness for the frontend's Playwright suite (frontend/e2e). Not
// part of the normal test suite: it only runs with BDRIVE_E2E_SERVE=1, where
// it serves a small deterministic hub on :8993 until killed. Playwright
// starts it via its webServer config and tears it down after the run.
//
// Unlike the manual demo harness, state is wiped on every start so the
// tests always see the same world: one org ("default", owned by the admin
// account), one project ("wiki") with a handful of seeded files, two
// accounts, one share-ready markdown tree with wikilinks, and enough read
// heat to light up the insights views.

import (
	"crypto/sha256"
	"encoding/hex"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/runbear-io/beardrive/internal/journal"
	"github.com/runbear-io/beardrive/internal/remote"
)

const (
	e2eAddr = "0.0.0.0:8993"
	// e2eSession is the agent session the seeded run card belongs to.
	e2eSession  = "8f21e4"
	e2eAdmin    = "e2e@example.com"
	e2eMember   = "member@example.com"
	e2eSolo     = "solo@example.com"
	e2eReader   = "reader@example.com" // org member with a read-only grant on "wiki"
	e2ePassword = "e2e-pass-1"
)

// e2eState is the fixed state dir TestE2EServe wipes and reseeds on every
// start. Fixed on purpose (the determinism contract above) — which is why
// listenHub has to run before anything touches it.
func e2eState() string { return filepath.Join(os.TempDir(), "bdrive-e2e-hub") }

// listenHub binds the port both harnesses share. Called BEFORE any state is
// touched: a second run must not wipe a live hub's storage on its way to
// failing to bind.
func listenHub(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", e2eAddr)
	if err != nil {
		t.Fatalf("cannot bind %s (is an e2e or demo hub already running?): %v", e2eAddr, err)
	}
	t.Cleanup(func() { ln.Close() })
	return ln
}

// serveHub serves until d elapses or the server errors — an error after a
// successful bind fails the test instead of vanishing into a goroutine.
func serveHub(t *testing.T, ln net.Listener, h http.Handler, d time.Duration) {
	t.Helper()
	errc := make(chan error, 1)
	go func() { errc <- http.Serve(ln, h) }()
	select {
	case err := <-errc:
		t.Fatalf("serve: %v", err)
	case <-time.After(d):
	}
}

func TestE2EServe(t *testing.T) {
	if os.Getenv("BDRIVE_E2E_SERVE") == "" {
		t.Skip("frontend e2e harness; set BDRIVE_E2E_SERVE=1 to run")
	}
	ln := listenHub(t) // before the wipe below: a busy port must cost nothing
	state := e2eState()
	if err := os.RemoveAll(state); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(state, "storage"), 0o755); err != nil {
		t.Fatal(err)
	}

	be, err := remote.Open(t.Context(), "file://"+filepath.Join(state, "storage"))
	if err != nil {
		t.Fatal(err)
	}
	db, err := OpenProjectDB(filepath.Join(state, "projects.json"))
	if err != nil {
		t.Fatal(err)
	}
	p, _, err := db.GetOrCreate("wiki", "")
	if err != nil {
		t.Fatal(err)
	}
	// Volume is deliberately the lowercase storage basename, so hub.spec.ts's
	// "#vault-name reads BearDrive" assertion actually catches a brand that
	// falls back to storage instead of the product name.
	srv := &Server{Root: be, Projects: db, Device: webDevice, Refresh: 0, Volume: "beardrive", Upload: UploadConfig{Enabled: true}}

	seedE2E(t, state, filepath.Join(state, "storage", p.ID), p.ID)

	srv.Reads, err = OpenReadLedger(filepath.Join(state, "reads.json"), 0)
	if err != nil {
		t.Fatal(err)
	}
	// Per-session read detail, so the seeded run card has both halves of the
	// story: what the run changed AND what it read (BEA-98).
	srv.Reads.WithSessions(OpenSessionReadRepo(filepath.Join(state, "sessions.json")), 0)
	for _, path := range []string{
		"notes/readme.md",         // read AND rewritten by the run
		"index.md",                // read, never changed
		"archive/retired-spec.md", // read, never changed — the hot+stale one
		// Read by the run, then deleted by the seed: the run card has to keep
		// it and label it "no longer in the project", the way the Dashboard
		// already does with its heat row (BEA-152). Drop this and the label
		// has nothing to render against.
		"scratch.md",
	} {
		srv.Reads.RecordSession(p.ID, e2eSession, "seed", path)
	}
	srv.Devices, _ = OpenDeviceRegistry(filepath.Join(state, "devices.json"))
	srv.Devices.Observe(DeviceInfo{ID: "seed", Name: "seed-agent", OS: "linux/amd64"})

	auth, err := OpenBuiltinAuth(filepath.Join(state, "auth.json"), false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := auth.signup(e2eAdmin, "E2E Admin", e2ePassword); err != nil {
		t.Fatal(err)
	}
	if _, err := auth.signup(e2eMember, "E2E Member", e2ePassword); err != nil {
		t.Fatal(err)
	}
	// In no org: sees the onboarding empty state (agent paste prompt).
	if _, err := auth.signup(e2eSolo, "E2E Solo", e2ePassword); err != nil {
		t.Fatal(err)
	}
	if _, err := auth.signup(e2eReader, "E2E Reader", e2ePassword); err != nil {
		t.Fatal(err)
	}
	auth.Admins = map[string]bool{e2eAdmin: true}
	srv.Auth = auth

	orgs, err := OpenOrgDB(filepath.Join(state, "orgs.json"))
	if err != nil {
		t.Fatal(err)
	}
	org, err := orgs.Create("default", e2eAdmin)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []string{e2eMember, e2eReader} {
		if err := orgs.AddMember(org.ID, m, RoleMember); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.SetOrg(p.ID, org.ID); err != nil {
		t.Fatal(err)
	}
	// One member cut back to read: the suite checks that write affordances
	// are absent for them, not merely that the server would 403.
	if err := db.SetPerm(p.ID, e2eReader, PermRead); err != nil {
		t.Fatal(err)
	}
	srv.Dir = LocalDirectory{OrgDB: orgs}
	auth.InviteValid = orgs.ValidInvite

	shares, err := OpenShareDB(filepath.Join(state, "shares.json"))
	if err != nil {
		t.Fatal(err)
	}
	srv.Shares = shares

	t.Logf("e2e hub on http://%s (state: %s) — %s / %s", e2eAddr, state, e2eAdmin, e2ePassword)
	serveHub(t, ln, srv.Handler(), 2*time.Hour) // Playwright kills the process when the run ends
}

// TestHarnessFailsFastOnBoundPort is the regression test for the bug that
// bit: a second TestE2EServe used to wipe a live hub's state dir on its way
// to silently failing to bind. Ungated, so it runs in the normal suite.
func TestHarnessFailsFastOnBoundPort(t *testing.T) {
	if testing.Short() {
		t.Skip("shells out to go test")
	}
	ln, err := net.Listen("tcp", e2eAddr)
	if err != nil {
		t.Skipf("%s already in use — nothing safe to prove: %v", e2eAddr, err)
	}
	defer ln.Close()

	canary := filepath.Join(e2eState(), "canary.txt")
	if err := os.MkdirAll(e2eState(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(canary, []byte("survive"), 0o644); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(canary)

	cmd := exec.Command("go", "test", "-count=1", "-run", "TestE2EServe", "./internal/webapp")
	cmd.Dir = "../.."
	cmd.Env = append(os.Environ(), "BDRIVE_E2E_SERVE=1")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("harness started with %s already bound:\n%s", e2eAddr, out)
	}
	if !strings.Contains(string(out), "8993") {
		t.Errorf("failure does not name the port:\n%s", out)
	}
	if b, err := os.ReadFile(canary); err != nil || string(b) != "survive" {
		t.Errorf("state dir was wiped by a run that could not bind: %v %q", err, b)
	}
}

// seedE2E journals a small fixed file tree (with history on one path and one
// binary) and read heat for it.
func seedE2E(t *testing.T, state, prefix, projectID string) {
	t.Helper()
	os.MkdirAll(filepath.Join(prefix, "journal"), 0o755)
	os.MkdirAll(filepath.Join(prefix, "blobs"), 0o755)
	now := time.Now().UTC()
	var ops []journal.Op
	var lam, seq int64
	put := func(path, content string, age time.Duration) {
		sum := sha256.Sum256([]byte(content))
		blob := hex.EncodeToString(sum[:])
		if err := os.WriteFile(filepath.Join(prefix, "blobs", blob), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		lam++
		seq++
		ops = append(ops, journal.Op{
			Seq: seq, Lamport: lam, Time: now.Add(-age),
			Device: "seed", DeviceName: "seed-agent", Author: "alice@x.io",
			User: "alice@x.io", UserName: "Alice",
			Kind: journal.KindPut, Path: path, Blob: blob,
			Size: int64(len(content)), Mode: 0o644,
		})
	}
	// The dangling [[nowhere]] is deliberate: the frontend has to render a
	// wikilink with no matching file as an unresolved anchor, not as a dead
	// "wiki:" href (BEA-136).
	put("index.md", "# Wiki\n\nStart at the [[guide]] or browse [notes](notes/readme.md). Nothing at [[nowhere]].\n", 72*time.Hour)
	put("guide.md", "# Guide\n\nFirst version of the guide.\n", 48*time.Hour)
	put("guide.md", "# Guide\n\nSecond version of the guide, with more detail.\n", 2*time.Hour)
	put("notes/readme.md", "# Notes\n\nNested folder content.\n", 24*time.Hour)
	put("notes/deep/topic.md", "# Topic\n\nDeeply nested file.\n", 24*time.Hour)
	// The share gate needs something to fire on. Fabricated, AWS-shaped —
	// not a credential, and the only seeded file that holds one.
	put("deploy.md", "# Deploy\n\nexport AWS_ACCESS_KEY_ID=AKIAIOSFODNN7EXAMPLE\n", 24*time.Hour)
	// Tiny valid PNG (1x1), enough to exercise the binary/download path.
	png := "\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00\x1f\x15\xc4\x89" +
		"\x00\x00\x00\nIDATx\x9cc\x00\x01\x00\x00\x05\x00\x01\r\n-\xb4\x00\x00\x00\x00IEND\xaeB`\x82"
	put("assets/logo.png", png, 24*time.Hour)
	// Hot but stale: read a lot, unchanged for months. Every other seeded file
	// is hours old, so without these the Dashboard's danger quadrant — the one
	// thing that panel exists to surface — was empty in every test.
	put("archive/retired-spec.md", "# Retired spec\n\nNobody has touched this in months.\n", 210*24*time.Hour)
	put("archive/old-runbook.md", "# Old runbook\n\nStill read, never maintained.\n", 150*24*time.Hour)
	put("archive/legacy-notes.md", "# Legacy notes\n\nStale, still consulted.\n", 95*24*time.Hour)
	ops[2].Note = "expanded the guide — https://claude.ai/session/e2e" // the one row with a note expander
	// A second account, so the history filter bar has more than one name to
	// offer — and something to exclude when a reader picks one.
	ops[4].User, ops[4].UserName, ops[4].Author = "bob@x.io", "Bob", "bob@x.io"
	// One agent run that touched two files — the history feed groups it into
	// a single card. One file it edited and one it created (whose undo is a
	// removal, since restore cannot un-create). Both of these ops are the head
	// of their path, so neither row offers a restore (BEA-57).
	put("notes/readme.md", "# Notes\n\nRewritten during the agent run.\n", 90*time.Minute)
	put("runbook.md", "# Runbook\n\nCreated during the agent run.\n", 90*time.Minute)
	ops[len(ops)-1].Note = "claude-code session " + e2eSession
	ops[len(ops)-2].Note = "claude-code session " + e2eSession
	// The un-forgeable half of the run identity: the note is what a reader
	// sees, this is what the card groups and joins its reads on.
	ops[len(ops)-1].Session = e2eSession
	ops[len(ops)-2].Session = e2eSession
	// A second version of the same binary, so the history diff has a
	// predecessor to refuse to diff (the "binary — no diff" path).
	put("assets/logo.png", png+"\x00trailing", 3*time.Hour)
	// A file that MOVED: the same blob put at the new path and the old path
	// deleted, one device, one cycle — the shape the scanner emits for a
	// rename. The old URL has to keep working (BEA-81).
	put("old-guide.md", "# Old guide\n\nThis file has been moved.\n", 30*time.Hour)
	put("archive/moved-guide.md", "# Old guide\n\nThis file has been moved.\n", 5*time.Hour)
	lam++
	seq++
	ops = append(ops, journal.Op{
		Seq: seq, Lamport: lam, Time: now.Add(-5 * time.Hour).Add(time.Second),
		Device: "seed", DeviceName: "seed-agent", Author: "alice@x.io",
		User: "alice@x.io", UserName: "Alice",
		Kind: journal.KindDelete, Path: "old-guide.md",
	})
	// One removed file, so the history feed has a delete row: deletes have no
	// content, so their rows stay unclickable while every other row is now an
	// address for its own version.
	put("scratch.md", "# Scratch\n\nTemporary.\n", 12*time.Hour)
	// One good fence and one deliberately broken one on the same page: the
	// point of the fallback is that a diagram nobody can parse doesn't take
	// the diagrams around it down with it. The markup inside the broken fence
	// is there so the parser quotes it into its own message — the diagnostic
	// is inserted as text, and this is what proves it. The tag is short on
	// purpose: the parser's window is 20 characters of preceding source, and a
	// tag truncated by it would leave no start tag for an innerHTML bug to
	// mount, so the test would pass either way. Appended LAST on purpose — the
	// mutations above address ops by index, so an insert anywhere earlier
	// hands one file's author or note to another file.
	put("diagram.md", "# Diagram\n\n```mermaid\ngraph TD\n  A[Agent] --> B[Hub]\n  B --> C[Teammate]\n```\n\n"+
		"Broken one below.\n\n```mermaid\ngraph TD\n  A[<img onerror=x>[[[[ --> ???\n```\n", 24*time.Hour)
	lam++
	seq++
	ops = append(ops, journal.Op{
		Seq: seq, Lamport: lam, Time: now.Add(-6 * time.Hour),
		Device: "seed", DeviceName: "seed-agent", Author: "alice@x.io",
		User: "alice@x.io", UserName: "Alice",
		Kind: journal.KindDelete, Path: "scratch.md",
	})
	// A conflict copy of guide.md, named exactly the way syncer.conflictName
	// writes one. The suffix is the whole contract the hub reads a conflict
	// out of (BEA-128), so the timestamp is a fixed literal: a relative one
	// would make the banner's text move under the assertion.
	// Under archive/ on purpose: notes/ is where two other specs pin an exact
	// file count and measure every row's width on a 360px phone, and a 53-
	// character filename is not what those are about.
	put("archive/old-runbook.md.bdrive-conflict-mira-laptop-20260814T060945Z",
		"# Old runbook\n\nMira's version, written at the same time as the other one.\n", 2*time.Hour)
	ops[len(ops)-1].Note = "conflict copy of archive/old-runbook.md"
	if err := journal.Append(filepath.Join(prefix, "journal", "seed.jsonl"), ops); err != nil {
		t.Fatal(err)
	}

	day := now.Format("2006-01-02")
	var stats []ReadStat
	for _, rd := range []struct {
		path  string
		kind  string
		actor string
		n     int64
	}{
		{"index.md", ReadKindHuman, "alice@x.io", 12},
		{"index.md", ReadKindAgent, "seed", 30},
		{"guide.md", ReadKindHuman, "bob@x.io", 5},
		{"guide.md", ReadKindAgent, "seed", 9},
		{"notes/readme.md", ReadKindAgent, "seed", 2},
		// The danger quadrant: hot enough to matter, stale enough to worry.
		{"archive/retired-spec.md", ReadKindAgent, "seed", 14},
		{"archive/old-runbook.md", ReadKindHuman, "bob@x.io", 11},
		{"archive/legacy-notes.md", ReadKindAgent, "seed", 8},
		// Read history for the file the seed deletes above: heat rows outlive
		// their file, and the Dashboard must say so rather than drop them.
		{"scratch.md", ReadKindHuman, "alice@x.io", 4},
		// The only share reads in the seed, and deliberately on a path no
		// other assertion counts: the Dashboard's share lens has to isolate
		// exactly one file, which it can't prove if the file has other reads.
		{"notes/deep/topic.md", ReadKindShare, "tok-e2e/203.0.113.7", 3},
	} {
		stats = append(stats, ReadStat{Project: projectID, Path: rd.path, Day: day,
			Kind: rd.kind, Actor: rd.actor, Count: rd.n, Last: now})
	}
	if err := newFileReadRepo(filepath.Join(state, "reads.json")).PutBatch(stats); err != nil {
		t.Fatal(err)
	}
}
