package webapp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/runbear-io/beardrive/internal/remote"
)

// Administration surfaces: project lifecycle (rename/delete by the owning
// org's owner), hub-admin approval of pending signups, and an org-wide view
// of public share links. All of this is what makes a hub actually
// operable — an admin can offboard, clean up, and audit — without editing
// JSON files on the server by hand.

// handleProjectUpdate edits a project's name, description and icon. Project
// admins (and, implicitly, the owners of its org) only. It's a partial update:
// every field is a pointer, so only the keys actually present in the body
// change — {"description":""} clears the description, omitting the key leaves
// it alone.
func (s *Server) handleProjectUpdate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("project")
	if _, ok := s.project(w, r, id, PermAdmin); !ok {
		return
	}
	var req struct {
		Name        *string `json:"name"`
		Description *string `json:"description"`
		Icon        *string `json:"icon"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.Projects.Update(id, req.Name, req.Description, req.Icon); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// handleProjectDelete removes a project from the registry and purges its
// storage prefix (blobs, journals). Project admins only.
func (s *Server) handleProjectDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("project")
	if _, ok := s.project(w, r, id, PermAdmin); !ok {
		return
	}
	if err := s.deleteProject(r.Context(), id, s.requestUser(r).Email); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// deleteProject tombstones the project (the registry row stays, marked
// deleted-by-whom-when — the audit record, queryable via /api/projects
// ?deleted=1), revokes its share links, drops its cached volume, and purges
// its objects from the storage root. The tombstone write is the operation;
// the purge is best effort — a storage error after the row is tombstoned
// leaves orphaned objects (the pre-purge status quo), never a half-deleted
// project. `by` is the deleting account's email.
func (s *Server) deleteProject(ctx context.Context, id, by string) error {
	p, _ := s.Projects.Get(id)
	if err := s.Projects.Delete(id, by); err != nil {
		return err
	}
	log.Printf("audit: project deleted id=%s org=%s by=%s", id, p.Org, by)
	s.capture(by, "project_deleted", map[string]any{"project": id, "org": p.Org})
	if s.Shares != nil {
		for _, sh := range s.Shares.List(id) {
			s.Shares.Revoke(sh.Token)
		}
	}
	s.volsMu.Lock()
	delete(s.vols, id)
	s.volsMu.Unlock()
	if err := s.purgeStorage(ctx, id); err != nil {
		log.Printf("project %s deleted but storage purge failed (objects remain): %v", id, err)
	}
	return nil
}

// purgeStorage deletes every object under the project's storage prefix. A
// Root without the delete capability keeps the old behavior: the id is
// retired, the objects stay for out-of-band cleanup.
func (s *Server) purgeStorage(ctx context.Context, id string) error {
	d, ok := s.Root.(remote.Deleter)
	if !ok {
		return nil
	}
	objs, err := s.Root.List(ctx, id+"/")
	if err != nil {
		return err
	}
	var firstErr error
	for _, o := range objs {
		// List answers by string prefix, so "p-abc/" can surface a sibling
		// like "p-abcd/x" on some backends — never delete outside the id.
		if !strings.HasPrefix(o.Key, id+"/") {
			continue
		}
		if err := d.Delete(ctx, o.Key); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// handleOrgDelete deletes an organization: every project it owns (registry
// and storage, via deleteProject) and then the org itself. Owners only. The
// org row goes first — it settles that this directory owns its orgs at all
// (ErrManagedElsewhere) before anything irreversible touches storage.
func (s *Server) handleOrgDelete(w http.ResponseWriter, r *http.Request) {
	orgID := r.PathValue("org")
	by, ok := s.requireOwner(w, r, orgID)
	if !ok {
		return
	}
	if err := s.Dir.Delete(orgID); err != nil {
		s.writeDirErr(w, orgID, err)
		return
	}
	deleted := 0
	var failed []string
	if s.Projects != nil {
		for _, p := range s.Projects.List() {
			if p.Org != orgID {
				continue
			}
			if err := s.deleteProject(r.Context(), p.ID, by); err != nil {
				log.Printf("org %s deleted but project %s was not: %v", orgID, p.ID, err)
				failed = append(failed, p.ID)
				continue
			}
			deleted++
		}
	}
	log.Printf("audit: org deleted id=%s by=%s projects=%d", orgID, by, deleted)
	s.capture(by, "org_deleted", map[string]any{"org": orgID, "projects": deleted})
	if len(failed) > 0 {
		http.Error(w, fmt.Sprintf("organization deleted, but these projects were not: %s",
			strings.Join(failed, ", ")), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// handleOrgShares lists every live public share across the org's projects,
// so an owner can audit "what have we made public?" in one place. Any org
// member may view; only owners revoke (via the existing per-share endpoint).
func (s *Server) handleOrgShares(w http.ResponseWriter, r *http.Request) {
	if s.Shares == nil || s.Dir == nil {
		http.Error(w, "sharing is not enabled on this server", http.StatusNotFound)
		return
	}
	orgID := r.PathValue("org")
	if s.Dir.Role(orgID, s.requestUser(r).Email) == "" {
		http.Error(w, "you are not a member of this organization", http.StatusForbidden)
		return
	}
	out := []map[string]any{}
	for _, p := range s.Projects.List() {
		if p.Org != orgID {
			continue
		}
		// Org membership is not access to every project in the org: a share
		// row carries the public /s/ token, so listing one to a member who is
		// denied that project hands them the file the denial exists to
		// withhold. Same resolver the per-project /shares route uses.
		if !atLeast(s.projectPermOf(r, p), PermRead) {
			continue
		}
		// One scan per visible project, zero per share — and after the
		// permission check, since there is no reason to scan for a project
		// the caller cannot see.
		opens := s.Reads.ShareOpens(p.ID)
		for _, sh := range s.Shares.List(p.ID) {
			j := shareJSON(r, sh, opens)
			j["project_name"] = p.Name
			out = append(out, j)
		}
	}
	writeJSON(w, map[string]any{"shares": out})
}

// approver returns the auth provider's account-administration half, if it has
// one. A provider whose accounts live in an external identity system does not:
// there is no local approval queue to show and no local policy to flip.
func (s *Server) approver(w http.ResponseWriter) (AccountApprover, bool) {
	a, ok := s.Auth.(AccountApprover)
	if !ok {
		// 503, not an empty list: "no queue here" and "queue is empty" are
		// different answers, and only one of them is true.
		http.Error(w, "accounts on this hub are administered in its identity provider",
			http.StatusServiceUnavailable)
		return nil, false
	}
	return a, true
}

// handleAdminPending lists accounts awaiting approval. Hub admins only.
func (s *Server) handleAdminPending(w http.ResponseWriter, r *http.Request) {
	if !s.requestUser(r).Admin {
		http.Error(w, "hub admins only", http.StatusForbidden)
		return
	}
	a, ok := s.approver(w)
	if !ok {
		return
	}
	writeJSON(w, map[string]any{"pending": a.PendingUsers()})
}

// handleAdminPolicy reads (GET) or updates (POST) the signup/access policy.
// Domains and the admin list are reported read-only — they're server-config
// owned so a browser session can't widen access — while verification and
// approval toggles can be flipped live and are persisted.
func (s *Server) handleAdminPolicy(w http.ResponseWriter, r *http.Request) {
	if !s.requestUser(r).Admin {
		http.Error(w, "hub admins only", http.StatusForbidden)
		return
	}
	a, ok := s.approver(w)
	if !ok {
		return
	}
	if r.Method == http.MethodPost {
		var req struct {
			RequireVerification bool `json:"require_verification"`
			RequireApproval     bool `json:"require_approval"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
			http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
			return
		}
		// No gating rule of its own: SetPolicy runs the same validator startup
		// does, so a browser session cannot reach a posture the binary refuses
		// to boot in — which is what the mailer check here used to be one third
		// of. 400, because a refusal here is the policy, not the store.
		if err := a.SetPolicy(req.RequireVerification, req.RequireApproval); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	writeJSON(w, a.Policy())
}

// handleAdminApprove activates a pending account. Hub admins only.
func (s *Server) handleAdminApprove(w http.ResponseWriter, r *http.Request) {
	if !s.requestUser(r).Admin {
		http.Error(w, "hub admins only", http.StatusForbidden)
		return
	}
	a, ok := s.approver(w)
	if !ok {
		return
	}
	if err := a.Approve(r.PathValue("id")); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// handleAdminDeny removes a pending account. Hub admins only.
func (s *Server) handleAdminDeny(w http.ResponseWriter, r *http.Request) {
	if !s.requestUser(r).Admin {
		http.Error(w, "hub admins only", http.StatusForbidden)
		return
	}
	a, ok := s.approver(w)
	if !ok {
		return
	}
	if err := a.Deny(r.PathValue("id")); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}
