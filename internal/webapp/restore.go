package webapp

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/runbear-io/beardrive/internal/journal"
)

// Restore puts an old version of a file back — as a NEW op, never by editing
// history. The blob is already in the store (they are retained forever), so
// this is the upload commit minus the upload: find the historical op, journal
// a put pointing at the same blob, done. Every device then converges on it
// like any other change, and the restore is itself restorable.
//
// What it deliberately is not: removing the offending ops. That would break
// one-writer-per-journal, strand peers that already replayed them, and
// corrupt the push cursor.

// handleRestore serves POST /api/p/<id>/restore {path, sha}.
func (s *Server) handleRestore(v *volume, w http.ResponseWriter, r *http.Request) {
	up := s.gateUpload(v, w) // a read-only hub stays read-only
	if up == nil {
		return
	}
	rs := storeSource(v, w)
	if rs == nil {
		return
	}
	var req struct {
		Path string `json:"path"`
		SHA  string `json:"sha"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	p, err := cleanUploadPath(req.Path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if !blobRe.MatchString(req.SHA) {
		http.Error(w, "sha must be 64 lowercase hex chars", http.StatusBadRequest)
		return
	}
	all, err := rs.loadOps(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	// The sha must be a version OF THIS FILE: without this, restore would
	// paste any blob in the store onto any path. "This file" includes the
	// paths it lived at before it moved — otherwise a moved file can never
	// be restored to anything older than its move. Ancestors only: the put
	// is written at the current path, so a descendant's versions are not
	// reachable from here anyway. buildMoveIndex needs journal order.
	journal.Sort(all)
	chain := chainSegments(buildMoveIndex(all), p)
	var found *journal.Op
	for i := range all {
		op := &all[i]
		if op.Kind == journal.KindPut && op.Blob == req.SHA && inSegments(chain, op.Path, op.Time) {
			found = op
			break
		}
	}
	if found == nil {
		http.Error(w, "no such version of that file", http.StatusNotFound)
		return
	}
	// A version that is already the file's content is not a change: writing it
	// would put a +0 −0 row in every teammate's history. journal.Replay sorts
	// internally, so this works on the unsorted slice loadOps returns — and a
	// deleted path has no state at all, so restoring it back still goes through.
	if journal.Replay(all)[p].Blob == req.SHA {
		http.Error(w, "that version is already the current content of "+p, http.StatusConflict)
		return
	}
	// The blob is already stored, so a restore adds no bytes — but an org
	// whose plan is blocked must still be blocked from writing.
	org := s.orgOf(r.PathValue("project"))
	if err := s.quota().CheckWrite(org, 0); err != nil {
		http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
		return
	}
	note := fmt.Sprintf("restore %s@%s", p, req.SHA[:8])
	// Size comes from the historical op, never from the request body.
	if err := rs.Commit(r.Context(), p, req.SHA, found.Size, s.requestUser(r), note); err != nil {
		code := http.StatusBadGateway
		if err == errBlobMissing {
			code = http.StatusConflict
		}
		http.Error(w, fmt.Sprintf("restore: %v", err), code)
		return
	}
	s.quota().RecordUsage(org, 0)
	v.invalidate()
	// A restore writes a put op like any other edit, so it belongs in the same
	// count. The frontend's `file_restored` says which BUTTON was pressed; this
	// says a file changed.
	s.captureChange(r, "browser", 1, 0)
	s.publishChange(r, "browser", []string{p}, 1, 0)
	writeJSON(w, map[string]any{"ok": true, "blob": req.SHA, "size": found.Size})
}
