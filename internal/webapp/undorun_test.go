package webapp

import (
	"math"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/runbear-io/beardrive/internal/journal"
	"github.com/runbear-io/beardrive/internal/remote"
)

// ---- the planner ----

// sop is a sourcedOp built the way loadSourcedOps builds one: the journal it
// was read FROM is the attribution, and everything inside the op is just what
// the pusher wrote there.
func sop(from string, op journal.Op) sourcedOp {
	if op.Device == "" {
		op.Device = from
	}
	return sourcedOp{Op: op, From: from}
}

// seq stamps ops with an increasing (seq, lamport, time) so journal.Less
// orders them in the order they are written, like a real journal.
type seqf struct {
	lam int64
	seq map[string]int64
	t0  time.Time
}

func newSeqf() *seqf {
	return &seqf{seq: map[string]int64{}, t0: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (s *seqf) put(from, path, blob string, mut ...func(*journal.Op)) sourcedOp {
	return s.op(from, journal.Op{Kind: journal.KindPut, Path: path, Blob: blob, Size: int64(len(blob)), Mode: 0o644}, mut...)
}

func (s *seqf) del(from, path string, mut ...func(*journal.Op)) sourcedOp {
	return s.op(from, journal.Op{Kind: journal.KindDelete, Path: path}, mut...)
}

func (s *seqf) op(from string, op journal.Op, mut ...func(*journal.Op)) sourcedOp {
	s.lam++
	s.seq[from]++
	op.Lamport, op.Seq, op.Time = s.lam, s.seq[from], s.t0.Add(time.Duration(s.lam)*time.Minute)
	for _, m := range mut {
		m(&op)
	}
	return sop(from, op)
}

func inSession(id string) func(*journal.Op) {
	return func(o *journal.Op) { o.Session = id; o.Note = "claude-code session " + id }
}

// The four rows of the spec's table, in one run: a file the run edited comes
// back to its pre-run blob, a file it created is removed, a file it deleted
// comes back, and a path already at its pre-run content is skipped rather
// than written.
func TestPlanUndoActions(t *testing.T) {
	s := newSeqf()
	ops := []sourcedOp{
		s.put("dev1", "edited.md", "v1"),
		s.put("dev1", "deleted.md", "keepme"),
		s.put("dev1", "noop.md", "same"),
		// the run
		s.put("dev1", "edited.md", "v2", inSession("run1")),
		s.put("dev1", "created.md", "brand new", inSession("run1")),
		s.del("dev1", "deleted.md", inSession("run1")),
		s.put("dev1", "noop.md", "changed", inSession("run1")),
		// someone put noop.md back to its pre-run content afterwards
		s.put("dev1", "noop.md", "same"),
	}
	plan := planUndo(ops, undoSel{Device: "dev1", Session: "run1"})

	want := map[string]struct {
		action string
		blob   string
	}{
		"created.md": {"remove", ""},
		"deleted.md": {"restore", "keepme"},
		"edited.md":  {"restore", "v1"},
	}
	if len(plan.Ops) != len(want) {
		t.Fatalf("plan wrote %d ops, want %d: %+v", len(plan.Ops), len(want), plan.Ops)
	}
	for i, op := range plan.Ops {
		w, ok := want[op.Path]
		if !ok {
			t.Fatalf("unexpected path in plan: %s", op.Path)
		}
		if plan.Actions[i].Path != op.Path {
			t.Fatalf("action %d = %+v, does not line up with op %+v", i, plan.Actions[i], op)
		}
		switch w.action {
		case "remove":
			if op.Kind != journal.KindDelete || plan.Actions[i].Action != "remove" {
				t.Fatalf("%s = %+v / %+v, want a delete", op.Path, op, plan.Actions[i])
			}
		case "restore":
			if op.Kind != journal.KindPut || op.Blob != w.blob || plan.Actions[i].Action != "restore" {
				t.Fatalf("%s = %+v, want a put of %q", op.Path, op, w.blob)
			}
			if op.Size != int64(len(w.blob)) || op.Mode != 0o644 {
				t.Fatalf("%s size/mode came from somewhere other than the historical op: %+v", op.Path, op)
			}
		}
	}
	if len(plan.Skipped) != 1 || plan.Skipped[0] != "noop.md" {
		t.Fatalf("skipped = %v, want [noop.md]", plan.Skipped)
	}
	// noop.md is also the path someone touched after the run.
	if len(plan.After) != 1 || plan.After[0] != "noop.md" {
		t.Fatalf("changed-after = %v, want [noop.md]", plan.After)
	}
	// Emitted sorted by path, so the dialog and the journal read the same
	// every time.
	for i := 1; i < len(plan.Ops); i++ {
		if plan.Ops[i-1].Path > plan.Ops[i].Path {
			t.Fatalf("plan is not sorted by path: %+v", plan.Ops)
		}
	}
}

// A path nobody has touched since the run produces no warning at all — the
// dialog's whole warning block is absent at zero.
func TestPlanUndoNoChangedAfter(t *testing.T) {
	s := newSeqf()
	plan := planUndo([]sourcedOp{
		s.put("dev1", "a.md", "v1"),
		s.put("dev1", "a.md", "v2", inSession("run1")),
	}, undoSel{Device: "dev1", Session: "run1"})
	if len(plan.After) != 0 {
		t.Fatalf("changed-after = %v, want empty", plan.After)
	}
}

// The empty-Session clause on the note form: runs.ts keys a session-carrying
// op as "s\0…" and can never file it under a note-keyed group, so an undo
// that ignored the clause would revert ops the card never showed.
func TestUndoRunNoteKeyedIgnoresSessionOps(t *testing.T) {
	s := newSeqf()
	note := "nightly docs pass"
	plan := planUndo([]sourcedOp{
		s.put("dev1", "old.md", "v1"),
		s.put("dev1", "sessioned.md", "v1"),
		// legacy run: a note, no session
		s.put("dev1", "old.md", "v2", func(o *journal.Op) { o.Note = note }),
		// same note, but it carries a session — a different card entirely
		s.put("dev1", "sessioned.md", "v2", func(o *journal.Op) { o.Note, o.Session = note, "run1" }),
	}, undoSel{Device: "dev1", Note: note})

	if len(plan.Ops) != 1 || plan.Ops[0].Path != "old.md" {
		t.Fatalf("note-keyed undo = %+v, want only old.md", plan.Ops)
	}
}

// Selection is by the journal an op was READ FROM, never by the op's own
// Device field: that field is arbitrary JSON any member with write access can
// put in their own journal.
func TestUndoRunSelectsByJournalKey(t *testing.T) {
	s := newSeqf()
	ops := []sourcedOp{
		s.put("dev1", "mine.md", "v1"),
		s.put("dev2", "theirs.md", "v1"),
		// dev1's real run op
		s.put("dev1", "mine.md", "v2", inSession("run1")),
		// dev2 forges dev1's device id AND session inside its own journal
		s.op("dev2", journal.Op{
			Kind: journal.KindPut, Path: "theirs.md", Blob: "v2", Size: 2, Mode: 0o644,
			Device: "dev1",
		}, inSession("run1")),
	}
	plan := planUndo(ops, undoSel{Device: "dev1", Session: "run1"})
	if len(plan.Ops) != 1 || plan.Ops[0].Path != "mine.md" {
		t.Fatalf("plan = %+v, want only dev1's own journal's op", plan.Ops)
	}
}

// A path the hub's own upload door would refuse never gets journaled by the
// undo — a peer can push one, and a hub that hands it back to every device
// has already lost.
func TestPlanUndoRefusesReservedPaths(t *testing.T) {
	s := newSeqf()
	plan := planUndo([]sourcedOp{
		s.put("dev1", ".bdrive/config.json", "v1"),
		s.put("dev1", ".bdrive/config.json", "v2", inSession("run1")),
		s.put("dev1", "ok.md", "v1"),
		s.put("dev1", "ok.md", "v2", inSession("run1")),
	}, undoSel{Device: "dev1", Session: "run1"})

	if len(plan.Ops) != 1 || plan.Ops[0].Path != "ok.md" {
		t.Fatalf("plan = %+v, want only ok.md", plan.Ops)
	}
	if len(plan.Refused) != 1 || plan.Refused[0] != ".bdrive/config.json" {
		t.Fatalf("refused = %v", plan.Refused)
	}
}

// ---- the endpoint ----

// runHub seeds a project with a two-file agent run on device "seed", and
// returns the handler, the API base, and the project's storage dir.
func runHub(t *testing.T, srv *Server, p Project, root string) (http.Handler, string, string) {
	t.Helper()
	dir := filepath.Join(root, p.ID)
	f := newFakeRemoteAt(t, dir)
	f.put("seed", "notes/readme.md", "before the run")
	f.append("seed", journal.Op{
		Kind: journal.KindPut, Path: "notes/readme.md", Blob: shaOf("during the run"),
		Size: int64(len("during the run")), Mode: 0o644,
		Note: "claude-code session 8f21e4", Session: "8f21e4",
	})
	writeBlob(t, dir, "during the run")
	f.append("seed", journal.Op{
		Kind: journal.KindPut, Path: "runbook.md", Blob: shaOf("created by the run"),
		Size: int64(len("created by the run")), Mode: 0o644,
		Note: "claude-code session 8f21e4", Session: "8f21e4",
	})
	writeBlob(t, dir, "created by the run")
	return srv.Handler(), "/api/p/" + p.ID + "/", dir
}

type undoResp struct {
	OK           bool         `json:"ok"`
	Undone       []undoAction `json:"undone"`
	Skipped      []string     `json:"skipped"`
	ChangedAfter []string     `json:"changed_after"`
	Refused      []string     `json:"refused"`
}

// The whole verb, end to end: the edited file goes back to its pre-run
// content and the created file is gone — in ONE write to the hub's journal.
func TestUndoRunOneJournalWrite(t *testing.T) {
	var cb *countBackend
	srv, p, root := newHub(t, true, func(be remote.Backend) remote.Backend {
		cb = newCountBackend(be)
		return cb
	})
	h, base, dir := runHub(t, srv, p, root)
	key := "journal/" + webDevice.ID + ".jsonl"
	before := cb.putsTo(p.ID + "/" + key)

	rec := do(t, h, "POST", base+"undo-run", map[string]any{"session": "8f21e4", "device": "seed"})
	var out undoResp
	mustJSON(t, rec, &out)
	if !out.OK || len(out.Undone) != 2 {
		t.Fatalf("undo = %+v, want 2 actions", out)
	}
	byPath := map[string]string{}
	for _, a := range out.Undone {
		byPath[a.Path] = a.Action
	}
	if byPath["notes/readme.md"] != "restore" || byPath["runbook.md"] != "remove" {
		t.Fatalf("actions = %+v", out.Undone)
	}

	// N paths, exactly one Put of our journal. Asserted by counting backend
	// calls, not by reading the result: a loop of appendOp answers 200 too.
	if got := cb.putsTo(p.ID+"/"+key) - before; got != 1 {
		t.Fatalf("undo of 2 paths made %d journal Puts, want exactly 1", got)
	}

	if rec := do(t, h, "GET", base+"file?path=notes/readme.md", nil); rec.Body.String() != "before the run" {
		t.Fatalf("edited file after undo = %q", rec.Body)
	}
	if rec := do(t, h, "GET", base+"file?path=runbook.md", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("created file after undo: %d, want 404", rec.Code)
	}
	// Only the hub's own journal moved — the run's ops are never edited or
	// removed.
	js := journalsAt(t, dir)
	if _, ok := js[webDevice.ID+".jsonl"]; !ok {
		t.Fatalf("the hub wrote no journal of its own: %v", js)
	}
	seed := js["seed.jsonl"]
	if n := countLines(seed); n != 3 {
		t.Fatalf("the run's own journal now has %d lines, want its original 3", n)
	}

	// The undo's ops carry a note naming the run, so it renders as its own
	// run card — itself undoable.
	entries := historyOf(t, h, base, "notes/readme.md")
	if entries[0].Note != "undo run 8f21e4" {
		t.Fatalf("undo note = %q", entries[0].Note)
	}
	if entries[0].Session != "" {
		t.Fatalf("the undo carries a session (%q) — it must group as a note-keyed card", entries[0].Session)
	}
}

// A preview describes the write without making one.
func TestUndoRunPreviewWritesNothing(t *testing.T) {
	srv, p, root := newHub(t, true, nil)
	h, base, dir := runHub(t, srv, p, root)
	before := journalsAt(t, dir)

	rec := do(t, h, "POST", base+"undo-run", map[string]any{"session": "8f21e4", "device": "seed", "preview": true})
	var out undoResp
	mustJSON(t, rec, &out)
	if len(out.Undone) != 2 {
		t.Fatalf("preview = %+v, want the same 2 actions", out)
	}
	if got := journalsAt(t, dir); len(got) != len(before) {
		t.Fatalf("a preview wrote a journal: %v → %v", before, got)
	}
	for name, data := range before {
		if journalsAt(t, dir)[name] != data {
			t.Fatalf("a preview changed journal %s", name)
		}
	}
}

// Both halves of the run identity, or neither — and nothing reaches a
// journal on the way to a 400.
func TestUndoRunBadRequests(t *testing.T) {
	srv, p, root := newHub(t, true, nil)
	h, base, dir := runHub(t, srv, p, root)
	before := journalsAt(t, dir)

	for _, body := range []map[string]any{
		{"session": "8f21e4"},                                  // no device
		{"device": "seed"},                                     // neither session nor note
		{"device": "seed", "session": "8f21e4", "note": "n"},   // both
		{"device": "not a device id", "session": "8f21e4"},     // malformed device
		{"device": "seed", "session": "bad\x00session"},        // control character
		{"device": "seed", "note": string(make([]byte, 1024))}, // over the cap
	} {
		if rec := do(t, h, "POST", base+"undo-run", body); rec.Code != http.StatusBadRequest {
			t.Fatalf("undo %v: %d %s, want 400", body, rec.Code, rec.Body)
		}
	}
	// A run nobody ever wrote is a 404, not an empty success.
	if rec := do(t, h, "POST", base+"undo-run", map[string]any{"device": "seed", "session": "nosuchrun"}); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown run: %d %s, want 404", rec.Code, rec.Body)
	}
	for name, data := range before {
		if journalsAt(t, dir)[name] != data {
			t.Fatalf("a refused undo wrote journal %s", name)
		}
	}
}

// Write permission, like its two siblings: an outsider and a read-only member
// both get 403.
func TestUndoRunPermissions(t *testing.T) {
	h, srv, c, p, root := permHubAt(t)
	if err := srv.Projects.SetPerm(p.ID, "carol@x.io", PermRead); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, p.ID)
	f := newFakeRemoteAt(t, dir)
	f.put("seed", "a.md", "v1")
	f.append("seed", journal.Op{
		Kind: journal.KindPut, Path: "a.md", Blob: shaOf("v2"), Size: 2, Mode: 0o644,
		Note: "claude-code session r1", Session: "r1",
	})
	writeBlob(t, dir, "v2")
	base := "/api/p/" + p.ID + "/"
	body := map[string]any{"session": "r1", "device": "seed"}

	for _, who := range []string{"dave", "carol"} { // outsider, read-only member
		if rec := doAs(t, h, "POST", base+"undo-run", body, c[who]); rec.Code != http.StatusForbidden {
			t.Fatalf("%s undo: %d %s, want 403", who, rec.Code, rec.Body)
		}
	}
	// A preview is still a write route: it reads the whole journal set and
	// names every path in the project's history.
	if rec := doAs(t, h, "POST", base+"undo-run", map[string]any{"session": "r1", "device": "seed", "preview": true}, c["carol"]); rec.Code != http.StatusForbidden {
		t.Fatalf("read-only preview: %d, want 403", rec.Code)
	}
	if rec := doAs(t, h, "POST", base+"undo-run", body, c["bob"]); rec.Code != 200 {
		t.Fatalf("member with write: %d %s", rec.Code, rec.Body)
	}
}

// An undo stores no bytes, but an org whose plan is blocked must still be
// blocked from writing.
func TestUndoRunQuota(t *testing.T) {
	h, srv, c, p, root := permHubAt(t)
	q := &recQuota{}
	srv.Quota = q
	dir := filepath.Join(root, p.ID)
	f := newFakeRemoteAt(t, dir)
	f.put("seed", "a.md", "v1")
	f.append("seed", journal.Op{
		Kind: journal.KindPut, Path: "a.md", Blob: shaOf("v2"), Size: 2, Mode: 0o644,
		Note: "claude-code session r1", Session: "r1",
	})
	writeBlob(t, dir, "v2")
	base := "/api/p/" + p.ID + "/"
	body := map[string]any{"session": "r1", "device": "seed"}
	before := journalsAt(t, dir)

	q.denyW = true
	rec := doAs(t, h, "POST", base+"undo-run", body, c["alice"])
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("blocked plan: %d %s, want 413", rec.Code, rec.Body)
	}
	for name, data := range before {
		if journalsAt(t, dir)[name] != data {
			t.Fatalf("a quota-blocked undo wrote journal %s", name)
		}
	}
	if len(q.writes) != 1 || q.writes[0].bytes != 0 {
		t.Fatalf("quota calls = %+v, want one CheckWrite of 0 bytes", q.writes)
	}

	q.denyW = false
	if rec := doAs(t, h, "POST", base+"undo-run", body, c["alice"]); rec.Code != 200 {
		t.Fatalf("unblocked undo: %d %s", rec.Code, rec.Body)
	}
	if len(q.usage) != 1 {
		t.Fatalf("usage = %+v, want one RecordUsage", q.usage)
	}
}

// ---- appendOps ----

// N ops, one read-modify-write. This is the atomicity argument for undo-run:
// one Put of one object either lands or it does not.
func TestAppendOpsOneWrite(t *testing.T) {
	var cb *countBackend
	srv, p, _ := newHub(t, true, func(be remote.Backend) remote.Backend {
		cb = newCountBackend(be)
		return cb
	})
	rs := &RemoteSource{Backend: remote.Prefixed(srv.Root, p.ID+"/"), Device: webDevice}
	key := p.ID + "/journal/" + webDevice.ID + ".jsonl"

	ops := make([]journal.Op, 5)
	for i := range ops {
		ops[i] = journal.Op{Kind: journal.KindDelete, Path: string(rune('a'+i)) + ".md"}
	}
	if err := rs.appendOps(t.Context(), ops); err != nil {
		t.Fatal(err)
	}
	if got := cb.putsTo(key); got != 1 {
		t.Fatalf("5 ops made %d Puts, want 1", got)
	}
	all, err := rs.loadOps(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 5 {
		t.Fatalf("journal holds %d ops, want 5", len(all))
	}

	// An empty batch never rewrites the key: a Put of identical bytes still
	// bumps Modified and invalidates every reader's journal cache.
	if err := rs.appendOps(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	if got := cb.putsTo(key); got != 1 {
		t.Fatalf("an empty batch wrote the journal (%d Puts)", got)
	}
}

// Seq and Lamport increase across a batch, and saturate rather than wrap when
// a peer has already claimed MaxInt64.
func TestAppendOpsOrdering(t *testing.T) {
	srv, p, root := newHub(t, true, nil)
	dir := filepath.Join(root, p.ID)
	newFakeRemoteAt(t, dir)
	rs := &RemoteSource{Backend: remote.Prefixed(srv.Root, p.ID+"/"), Device: webDevice}

	ops := make([]journal.Op, 4)
	for i := range ops {
		ops[i] = journal.Op{Kind: journal.KindDelete, Path: string(rune('a'+i)) + ".md"}
	}
	if err := rs.appendOps(t.Context(), ops); err != nil {
		t.Fatal(err)
	}
	for i := 1; i < len(ops); i++ {
		if ops[i].Seq <= ops[i-1].Seq || ops[i].Lamport <= ops[i-1].Lamport {
			t.Fatalf("op %d = (seq %d, lamport %d) after (seq %d, lamport %d)",
				i, ops[i].Seq, ops[i].Lamport, ops[i-1].Seq, ops[i-1].Seq)
		}
	}

	// A peer pushes MaxInt64. Every remaining lamport saturates there — and
	// journal.Less still totally orders them through (time, device, seq), so
	// replay stays deterministic.
	f := &fakeRemote{t: t, dir: dir, seq: map[string]int64{}}
	f.append("peer", journal.Op{Kind: journal.KindDelete, Path: "z.md", Lamport: math.MaxInt64})
	// fakeRemote assigns its own lamport, so write the op straight in.
	if err := journal.Append(filepath.Join(dir, "journal", "peer2.jsonl"), []journal.Op{{
		Seq: 1, Lamport: math.MaxInt64, Time: time.Now().UTC(),
		Device: "peer2", Kind: journal.KindDelete, Path: "zz.md",
	}}); err != nil {
		t.Fatal(err)
	}
	more := []journal.Op{
		{Kind: journal.KindDelete, Path: "m1.md"},
		{Kind: journal.KindDelete, Path: "m2.md"},
	}
	if err := rs.appendOps(t.Context(), more); err != nil {
		t.Fatal(err)
	}
	for i, op := range more {
		if op.Lamport != math.MaxInt64 {
			t.Fatalf("op %d lamport = %d, want it saturated at MaxInt64 (a wrap is MinInt64)", i, op.Lamport)
		}
	}
	if more[1].Seq <= more[0].Seq {
		t.Fatalf("seq stopped increasing at lamport saturation: %d then %d", more[0].Seq, more[1].Seq)
	}
}

// ---- helpers ----

func countLines(s string) int {
	n := 0
	for _, c := range s {
		if c == '\n' {
			n++
		}
	}
	return n
}

// writeBlob stores content under its own sha, the way an upload would —
// fakeRemote.append journals an op without one.
func writeBlob(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "blobs", shaOf(content)), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
