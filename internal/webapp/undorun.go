package webapp

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"

	"github.com/runbear-io/beardrive/internal/journal"
)

// Undo a whole agent run — the run-wide verb the run card was grouped for.
//
// Like restore and remove it is append-only: the run's ops are never edited
// or deleted (that would break one-writer-per-journal, strand peers that
// already replayed them, and corrupt the push cursor). What lands instead is
// one new op per path the run touched, putting the path back to the content it
// held just before the run — a put at the pre-run blob, or a delete for a file
// the run created.
//
// The whole batch goes down in ONE journal write (appendOps). That is the
// atomicity argument: one Put of one object either lands or it does not, so
// there is no half-undone run to report and no per-path rollback to design. A
// loop of appendOp here would silently make the feature partially-applicable
// with no error anywhere — the backend Put count in the tests is what protects
// it.

// undoSel identifies one run. Device is the journal the run's ops were READ
// FROM (sourcedOp.From), never op.Device: that field is arbitrary JSON any
// member with write access can put in their own journal, and the run card
// attributes rows by the journal key for exactly that reason (history.go).
// Exactly one of Session/Note is set — Session wherever the run has one, since
// a note is user-settable (`bdrive sync --note`) and could be forged to
// collide with a teammate's run.
type undoSel struct{ Device, Session, Note string }

// undoAction is one path's fate, for the confirm dialog and the response.
type undoAction struct {
	Path   string `json:"path"`
	Action string `json:"action"` // "restore" or "remove"
}

// undoPlan is what an undo would do, computed server-side. The preview and the
// write both come from here, so the dialog cannot describe one thing and the
// write do another.
type undoPlan struct {
	Ops     []journal.Op // exactly what will be journaled
	Actions []undoAction // the same ops, as the dialog reads them
	Skipped []string     // already at pre-run content — nothing to write
	After   []string     // someone landed an op on this path after the run
	Refused []string     // a path the hub's own upload door would refuse
}

// planUndo works out, for every path the selected run touched, the op that
// puts it back where it was. sourced may be in any order; it is sorted here.
func planUndo(sourced []sourcedOp, sel undoSel) undoPlan {
	ops := make([]journal.Op, len(sourced))
	inRun := make([]bool, len(sourced))
	for i, so := range sourced {
		ops[i] = so.Op
		inRun[i] = matchesRun(so, sel)
	}
	// One sort of an index permutation, so ops and inRun stay aligned.
	order := make([]int, len(ops))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return journal.Less(ops[order[a]], ops[order[b]]) })

	// first/last positions (in sorted order) of the run's ops per path, and
	// the ops that precede and follow them.
	type span struct{ first, last int }
	spans := map[string]span{}
	for pos, i := range order {
		if !inRun[i] {
			continue
		}
		p := ops[i].Path
		if s, ok := spans[p]; ok {
			s.last = pos
			spans[p] = s
			continue
		}
		spans[p] = span{first: pos, last: pos}
	}

	// Current state, replayed over everything — the same fold the tree and
	// the viewer serve, so "already at the pre-run content" means what a
	// reader sees.
	current := journal.Replay(ops)

	plan := undoPlan{}
	paths := make([]string, 0, len(spans))
	for p := range spans {
		paths = append(paths, p)
	}
	sort.Strings(paths) // a stable plan: the dialog and the journal agree run to run
	for _, p := range paths {
		s := spans[p]
		// The hub must not journal what its own upload door refuses: a peer
		// can push a path with a control character or under .bdrive/, and a
		// hub that hands one back to every device has already lost.
		if _, err := cleanUploadPath(p); err != nil {
			plan.Refused = append(plan.Refused, p)
			continue
		}
		// Anything on this path after the run's LAST op there is work the undo
		// is about to overwrite — usually a teammate's. It is still undone
		// (last-writer-wins is the model, and per-row restore already behaves
		// this way), but the confirm has to say so out loud.
		for _, i := range order[s.last+1:] {
			if ops[i].Path == p {
				plan.After = append(plan.After, p)
				break
			}
		}
		// The path's state just before the run: the newest op on it that sorts
		// before the run's FIRST op there.
		var before *journal.Op
		for k := s.first - 1; k >= 0; k-- {
			if op := &ops[order[k]]; op.Path == p {
				before = op
				break
			}
		}
		now := current[p]
		switch {
		case before == nil || before.Kind == journal.KindDelete:
			// The run created the file (or re-created a deleted one): undoing
			// it means taking it away. Already gone → nothing to write.
			if now.Blob == "" {
				plan.Skipped = append(plan.Skipped, p)
				continue
			}
			plan.Ops = append(plan.Ops, journal.Op{Kind: journal.KindDelete, Path: p})
			plan.Actions = append(plan.Actions, undoAction{Path: p, Action: "remove"})
		default:
			// An earlier version exists. Restoring content that is already the
			// file's content would put a +0 −0 row in every teammate's history
			// — the batch form of the 409 restore returns for a no-op version.
			if now.Blob == before.Blob {
				plan.Skipped = append(plan.Skipped, p)
				continue
			}
			plan.Ops = append(plan.Ops, journal.Op{
				Kind: journal.KindPut, Path: p,
				Blob: before.Blob, Size: before.Size, Mode: before.Mode,
			})
			plan.Actions = append(plan.Actions, undoAction{Path: p, Action: "restore"})
		}
	}
	return plan
}

// matchesRun is the selection rule, and it is groupRuns' key in Go. The
// empty-Session clause on the note form is load-bearing: runs.ts keys a
// session-carrying op as "s\0…" and can never file it under a note-keyed
// group, so a note-keyed undo that ignored it would revert ops the card
// never showed.
func matchesRun(so sourcedOp, sel undoSel) bool {
	if so.From != sel.Device {
		return false
	}
	if sel.Session != "" {
		return so.Op.Session == sel.Session
	}
	return so.Op.Note == sel.Note && so.Op.Session == ""
}

// undoNote is the note the undo's own ops carry. Built from the MATCHED ops,
// never from the request body: what lands in every teammate's history row then
// provably came through the /store door's SafeText gate. Written under the
// hub's own device with no session, so the undo groups as its own run card —
// itself undoable.
func undoNote(sel undoSel) string {
	if sel.Session != "" {
		return "undo run " + sel.Session
	}
	return "undo run " + sel.Note
}

// handleUndoRun serves POST /api/p/<id>/undo-run
// {session|note, device, preview}.
func (s *Server) handleUndoRun(v *volume, w http.ResponseWriter, r *http.Request) {
	up := s.gateUpload(v, w) // a read-only hub stays read-only
	if up == nil {
		return
	}
	rs := storeSource(v, w)
	if rs == nil {
		return
	}
	var req struct {
		Session string `json:"session"`
		Note    string `json:"note"`
		Device  string `json:"device"`
		Preview bool   `json:"preview"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	// Both halves of the run identity, or neither — the same rule the other
	// run-identity route applies (reads.go's ?session=&device=). A session-only
	// or note-only undo would revert every device's ops that happen to share
	// the string.
	if !deviceIDRe.MatchString(req.Device) {
		http.Error(w, "device must be given and be a valid device id", http.StatusBadRequest)
		return
	}
	if (req.Session == "") == (req.Note == "") {
		http.Error(w, "give exactly one of session or note", http.StatusBadRequest)
		return
	}
	for _, f := range []string{req.Session, req.Note} {
		if len(f) > 512 || !journal.SafeText(f) {
			http.Error(w, "invalid session or note", http.StatusBadRequest)
			return
		}
	}
	sel := undoSel{Device: req.Device, Session: req.Session, Note: req.Note}
	sourced, err := rs.loadSourcedOps(r.Context())
	if err != nil {
		storageErr(w, http.StatusBadGateway, "history is temporarily unavailable", err)
		return
	}
	plan := planUndo(sourced, sel)
	if len(plan.Ops) == 0 && len(plan.Skipped) == 0 && len(plan.Refused) == 0 {
		http.Error(w, "no such run", http.StatusNotFound)
		return
	}
	if req.Preview {
		writeJSON(w, undoResponse(plan))
		return
	}
	// An undo stores no bytes — every blob it points at is already in the
	// store — but an org whose plan is blocked must still be blocked from
	// writing, exactly like restore and remove.
	org := s.orgOf(r.PathValue("project"))
	if err := s.quota().CheckWrite(org, 0); err != nil {
		http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
		return
	}
	// Blobs before the journal. They are all already stored, so this is a
	// check rather than an upload — but a missing one fails the WHOLE undo
	// instead of writing a run that points at content no peer can fetch.
	seen := map[string]bool{}
	for _, op := range plan.Ops {
		if op.Kind != journal.KindPut || seen[op.Blob] {
			continue
		}
		seen[op.Blob] = true
		if _, ok, err := rs.blobStat(r.Context(), op.Blob); err != nil {
			http.Error(w, fmt.Sprintf("undo run: %v", err), http.StatusBadGateway)
			return
		} else if !ok {
			http.Error(w, "content for "+op.Path+" is no longer in the store", http.StatusConflict)
			return
		}
	}
	who := s.requestUser(r)
	note := undoNote(sel)
	for i := range plan.Ops {
		plan.Ops[i].User, plan.Ops[i].UserName, plan.Ops[i].Note = who.Email, who.Name, note
	}
	if err := rs.appendOps(r.Context(), plan.Ops); err != nil {
		http.Error(w, fmt.Sprintf("undo run: %v", err), http.StatusBadGateway)
		return
	}
	s.quota().RecordUsage(org, 0)
	v.invalidate()
	writeJSON(w, undoResponse(plan))
}

// undoResponse is the one shape both the preview and the write answer with.
// Empty lists, never nulls the client has to special-case.
func undoResponse(plan undoPlan) map[string]any {
	if plan.Actions == nil {
		plan.Actions = []undoAction{}
	}
	return map[string]any{
		"ok":            true,
		"undone":        plan.Actions,
		"skipped":       orEmpty(plan.Skipped),
		"changed_after": orEmpty(plan.After),
		"refused":       orEmpty(plan.Refused),
	}
}

func orEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
