package webapp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/runbear-io/beardrive/internal/journal"
)

// Remove un-creates a file — the other half of restore. Like restore it is a
// NEW op, never an edit to history: a delete op journaled under this server's
// own device, which every device then materializes like any other change. The
// blob stays in the store forever, so the resulting DELETED row restores the
// file straight back.
//
// Removing the offending ops instead is ruled out for the same reasons
// restore.go gives: it would break one-writer-per-journal, strand peers that
// already replayed them, and corrupt the push cursor.

// Remove appends a delete op for p to this server's own journal. A delete
// references no content, so there is no blob to push first.
func (r *RemoteSource) Remove(ctx context.Context, p string, who User, note string) error {
	if r.Device.ID == "" {
		return fmt.Errorf("no device identity configured for uploads")
	}
	return r.appendOp(ctx, journal.Op{
		Kind: journal.KindDelete, Path: p,
		User: who.Email, UserName: who.Name, Note: note,
	})
}

// handleRemove serves POST /api/p/<id>/remove {path}.
func (s *Server) handleRemove(v *volume, w http.ResponseWriter, r *http.Request) {
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
	if !s.writablePath(w, r, p) {
		return
	}
	// The volume snapshot is the same map the tree and viewer serve, so the
	// API agrees with what the caller was looking at. A stale-snapshot 404 is
	// a harmless retry; a second, hand-rolled replay could disagree and
	// delete the wrong thing.
	snap, err := v.snapshot(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if _, ok := snap.files[p]; !ok {
		http.Error(w, "no such file", http.StatusNotFound)
		return
	}
	// A delete stores no bytes — but an org whose plan is blocked must still
	// be blocked from writing.
	org := s.orgOf(r.PathValue("project"))
	if err := s.quota().CheckWrite(org, 0); err != nil {
		http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
		return
	}
	if err := rs.Remove(r.Context(), p, s.requestUser(r), "remove "+p); err != nil {
		http.Error(w, fmt.Sprintf("remove: %v", err), http.StatusBadGateway)
		return
	}
	s.quota().RecordUsage(org, 0)
	v.invalidate()
	s.captureChange(r, "browser", 0, 1)
	s.publishChange(r, "browser", []string{p}, 0, 1)
	writeJSON(w, map[string]any{"ok": true, "path": p})
}
