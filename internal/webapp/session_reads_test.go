package webapp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// sessionHub is permHub with read telemetry that also keeps session detail —
// the shape a served hub has (cmd/bdrive/web.go).
func sessionHub(t *testing.T) (http.Handler, *Server, map[string]*http.Cookie, Project, string) {
	t.Helper()
	h, srv, c, p, root := permHubAt(t)
	reads, err := OpenReadLedger(filepath.Join(t.TempDir(), "reads.json"), 0)
	if err != nil {
		t.Fatal(err)
	}
	srv.Reads = reads.WithSessions(OpenSessionReadRepo(filepath.Join(t.TempDir(), "sessions.json")), 0)
	return h, srv, c, p, root
}

func sessionPaths(t *testing.T, h http.Handler, p Project, c *http.Cookie, session, device string) []string {
	t.Helper()
	rec := doAs(t, h, "GET",
		"/api/p/"+p.ID+"/heat?session="+session+"&device="+device, nil, c)
	if rec.Code != 200 {
		t.Fatalf("session heat: %d %s", rec.Code, rec.Body)
	}
	var out struct {
		Paths []string `json:"paths"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out.Paths
}

func reportRead(t *testing.T, h http.Handler, p Project, c *http.Cookie, device string, reads []map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	return secfixDo(t, h, "POST", "/api/p/"+p.ID+"/reads",
		map[string]any{"reads": reads}, c, map[string]string{"X-Bdrive-Device": device})
}

// The join, end to end: a session's reads come back for the session+device
// pair its own ops carry, and only for files the project actually has.
func TestSessionReadsRoundTrip(t *testing.T) {
	h, _, c, p, root := sessionHub(t)
	f := newFakeRemoteAt(t, filepath.Join(root, p.ID))
	f.putAs("dev1", "alice@x.io", "Alice", "wiki/plan.md", "# plan")
	f.putAs("dev1", "alice@x.io", "Alice", "wiki/spec.md", "# spec")

	// dev1 is alice's: one sync registers it, as /store/* traffic does.
	if rec := secfixSync(t, h, p.ID, c["alice"], "dev1", "laptop", "mac"); rec.Code != 200 {
		t.Fatalf("alice sync: %d %s", rec.Code, rec.Body)
	}
	rec := reportRead(t, h, p, c["alice"], "dev1", []map[string]string{
		{"path": "wiki/plan.md", "session": "8f21e4"},
		{"path": "wiki/spec.md", "session": "8f21e4"},
		{"path": "wiki/gone.md", "session": "8f21e4"}, // landmine 3: no such file, records nothing
		{"path": "wiki/plan.md", "session": "other"},  // a different session, its own row
	})
	if rec.Code != 200 {
		t.Fatalf("report: %d %s", rec.Code, rec.Body)
	}

	got := sessionPaths(t, h, p, c["alice"], "8f21e4", "dev1")
	if len(got) != 2 || got[0] != "wiki/plan.md" || got[1] != "wiki/spec.md" {
		t.Fatalf("session paths = %v, want plan.md + spec.md (gone.md is not in the project)", got)
	}
	if got := sessionPaths(t, h, p, c["alice"], "other", "dev1"); len(got) != 1 || got[0] != "wiki/plan.md" {
		t.Fatalf("second session = %v, want only plan.md — sessions must not merge", got)
	}
	// A member who is not the reporting device still sees the run's reads:
	// the card is project-wide, and this response carries no identities.
	if got := sessionPaths(t, h, p, c["bob"], "8f21e4", "dev1"); len(got) != 2 {
		t.Fatalf("member view = %v, want the same two paths", got)
	}
	// An unknown session is an empty list, never an error and never a hint
	// that some other session exists.
	if got := sessionPaths(t, h, p, c["alice"], "no-such-session", "dev1"); len(got) != 0 {
		t.Fatalf("unknown session = %v, want empty", got)
	}
}

// Landmine 1's read-half twin: the session id in a read report is a CLIENT
// string, so bob can report reads naming alice's session. The row is pinned
// to the device the hub validated — bob's, never alice's — and the query
// requires both, so his rows can never surface on her run card.
func TestSessionReadsCannotBePaintedOntoAnotherDevicesRun(t *testing.T) {
	h, srv, c, p, root := sessionHub(t)
	f := newFakeRemoteAt(t, filepath.Join(root, p.ID))
	f.putAs("dev1", "alice@x.io", "Alice", "wiki/plan.md", "# plan")
	f.putAs("dev1", "alice@x.io", "Alice", "payroll.md", "secret")

	// alice-mbp is claimed by alice, as `bdrive login` claims it
	// (DeviceRegistry.Bind) — the state in which MayActAs has something to
	// refuse.
	srv.Devices.Observe(DeviceInfo{ID: "alice-mbp", Name: "laptop", OS: "mac", User: "alice@x.io"})
	if rec := secfixSync(t, h, p.ID, c["bob"], "bob-mbp", "laptop", "linux"); rec.Code != 200 {
		t.Fatalf("bob sync: %d %s", rec.Code, rec.Body)
	}
	if rec := reportRead(t, h, p, c["alice"], "alice-mbp", []map[string]string{
		{"path": "wiki/plan.md", "session": "8f21e4"},
	}); rec.Code != 200 {
		t.Fatalf("alice report: %d %s", rec.Code, rec.Body)
	}
	// bob reports under ALICE's session id, from his own device.
	if rec := reportRead(t, h, p, c["bob"], "bob-mbp", []map[string]string{
		{"path": "payroll.md", "session": "8f21e4"},
	}); rec.Code != 200 {
		t.Fatalf("bob report: %d %s", rec.Code, rec.Body)
	}
	// bob naming alice's DEVICE outright is refused by ownsDevice, so it
	// records for nobody.
	if rec := reportRead(t, h, p, c["bob"], "alice-mbp", []map[string]string{
		{"path": "payroll.md", "session": "8f21e4"},
	}); rec.Code != 200 {
		t.Fatalf("bob's forged-device report: %d %s", rec.Code, rec.Body)
	}

	got := sessionPaths(t, h, p, c["alice"], "8f21e4", "alice-mbp")
	if len(got) != 1 || got[0] != "wiki/plan.md" {
		t.Fatalf("alice's run card = %v, want only her own read — bob painted onto it", got)
	}
}

// The API shape: ?session= is a filter INPUT that requires its device, and
// the route is membership-gated exactly as /heat is.
func TestSessionHeatQueryContract(t *testing.T) {
	h, _, c, p, _ := sessionHub(t)
	base := "/api/p/" + p.ID + "/heat"

	for _, u := range []string{base + "?session=8f21e4", base + "?device=dev1"} {
		if rec := doAs(t, h, "GET", u, nil, c["alice"]); rec.Code != 400 {
			t.Fatalf("GET %s: %d, want 400 (session and device are required together)", u, rec.Code)
		}
	}
	// dave is in no org here: a non-member is walled out of the session
	// query exactly as they are out of plain heat.
	for _, u := range []string{base, base + "?session=8f21e4&device=dev1"} {
		if rec := doAs(t, h, "GET", u, nil, c["dave"]); rec.Code != http.StatusForbidden {
			t.Fatalf("outsider GET %s: %d, want 403", u, rec.Code)
		}
	}
}

// The privacy ruling, tested: nothing enumerates sessions. ?by=device output
// is byte-identical with session rows present, and plain /heat never grows a
// session column.
func TestSessionIdsAreNeverEnumerated(t *testing.T) {
	h, _, c, p, root := sessionHub(t)
	f := newFakeRemoteAt(t, filepath.Join(root, p.ID))
	f.putAs("dev1", "alice@x.io", "Alice", "wiki/plan.md", "# plan")
	if rec := secfixSync(t, h, p.ID, c["alice"], "dev1", "laptop", "mac"); rec.Code != 200 {
		t.Fatalf("alice sync: %d %s", rec.Code, rec.Body)
	}

	if rec := reportRead(t, h, p, c["alice"], "dev1", []map[string]string{
		{"path": "wiki/plan.md", "session": "8f21e4"},
	}); rec.Code != 200 {
		t.Fatalf("report: %d %s", rec.Code, rec.Body)
	}
	for _, body := range []string{
		doAs(t, h, "GET", "/api/p/"+p.ID+"/heat?by=device", nil, c["alice"]).Body.String(),
		doAs(t, h, "GET", "/api/p/"+p.ID+"/heat", nil, c["alice"]).Body.String(),
	} {
		if strings.Contains(body, "8f21e4") || strings.Contains(body, "session") {
			t.Fatalf("a heat response enumerated a session: %s", body)
		}
	}
}

// Retention: session rows past the horizon are DELETED, not folded, and the
// path's heat totals — which were never derived from them — are unchanged.
func TestSessionReadRetentionPrunes(t *testing.T) {
	repo := OpenSessionReadRepo(filepath.Join(t.TempDir(), "sessions.json"))
	l, _ := openTestLedger(t, 0)
	l.WithSessions(repo, 1) // one day

	l.Record("p-1", "a.md", ReadKindAgent, "dev1")
	l.RecordSession("p-1", "old", "dev1", "a.md")
	l.RecordSession("p-1", "new", "dev1", "a.md")
	// Age the "old" session past the horizon, then force a prune.
	l.mu.Lock()
	for k, sr := range l.pendingSess {
		if k.Session == "old" {
			sr.Last = time.Now().UTC().Add(-48 * time.Hour)
			l.pendingSess[k] = sr
		}
	}
	l.lastSessPrun = time.Time{}
	l.flushSessionsLocked()
	l.mu.Unlock()

	if got, _ := repo.ListBySession("p-1", "old", "dev1"); len(got) != 0 {
		t.Fatalf("expired session rows survived: %+v", got)
	}
	if got, _ := repo.ListBySession("p-1", "new", "dev1"); len(got) != 1 {
		t.Fatalf("recent session rows = %+v, want the one row", got)
	}
	// The aggregate is untouched by any of it.
	if e := l.Heat("p-1", "", time.Time{})["a.md"]; e.Agent != 1 {
		t.Fatalf("heat after the session prune = %+v, want agent 1", e)
	}
}

// With no session repo the ledger behaves exactly as before: recording is a
// no-op and a lookup is empty, never a panic.
func TestSessionReadsOffByDefault(t *testing.T) {
	l, _ := openTestLedger(t, 0)
	l.RecordSession("p-1", "s1", "dev1", "a.md")
	if got := l.SessionPaths("p-1", "s1", "dev1"); len(got) != 0 {
		t.Fatalf("session paths with no repo = %v, want none", got)
	}
	var nilLedger *ReadLedger
	nilLedger.RecordSession("p-1", "s1", "dev1", "a.md")
	if got := nilLedger.SessionPaths("p-1", "s1", "dev1"); got != nil {
		t.Fatalf("nil ledger = %v", got)
	}
}

// BEA-152: a read outlives the file it read. The History run card used to
// carry a footnote claiming reads are kept only for paths the project still
// has — nothing implemented that, and the Dashboard had been showing those
// reads (labelled) all along. This is the regression pin for the policy the
// card now states out loud: the ledger records what the agent did, so the
// audit surface reports it. It passes today; that is the point.
func TestSessionPathsSurviveDeletion(t *testing.T) {
	h, _, c, p, root := sessionHub(t)
	f := newFakeRemoteAt(t, filepath.Join(root, p.ID))
	f.putAs("dev1", "alice@x.io", "Alice", "scratch.md", "# scratch")

	if rec := secfixSync(t, h, p.ID, c["alice"], "dev1", "laptop", "mac"); rec.Code != 200 {
		t.Fatalf("alice sync: %d %s", rec.Code, rec.Body)
	}
	if rec := reportRead(t, h, p, c["alice"], "dev1", []map[string]string{
		{"path": "scratch.md", "session": "8f21e4"},
	}); rec.Code != 200 {
		t.Fatalf("report: %d %s", rec.Code, rec.Body)
	}
	if got := sessionPaths(t, h, p, c["alice"], "8f21e4", "dev1"); len(got) != 1 {
		t.Fatalf("before the delete = %v, want scratch.md", got)
	}

	// The agent (or anyone) deletes the file it read. A fresh report of the
	// same path is now refused (ingest only records paths the project has),
	// which is what proves the delete actually landed in the replayed state.
	f.delAt("dev1", "scratch.md", time.Now())
	rec := reportRead(t, h, p, c["alice"], "dev1", []map[string]string{
		{"path": "scratch.md", "session": "later"},
	})
	if !strings.Contains(rec.Body.String(), `"accepted":0`) {
		t.Fatalf("the file is still in the project, so this test proves nothing: %s", rec.Body)
	}
	if got := sessionPaths(t, h, p, c["alice"], "8f21e4", "dev1"); len(got) != 1 || got[0] != "scratch.md" {
		t.Fatalf("after the delete = %v, want scratch.md — a read is not undone by deleting the file", got)
	}
}
