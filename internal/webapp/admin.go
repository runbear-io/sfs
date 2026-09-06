package webapp

import (
	"encoding/json"
	"io"
	"net/http"
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

// handleProjectDelete removes a project from the registry. Project admins
// only. Storage (blobs, journals) is intentionally left in place.
func (s *Server) handleProjectDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("project")
	if _, ok := s.project(w, r, id, PermAdmin); !ok {
		return
	}
	if err := s.Projects.Delete(id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
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
		// And project access is not access to every FOLDER in the project, for
		// the same reason one line up: the row carries the public token, so
		// listing a link into a folder this member cannot read hands them the
		// file the rule exists to withhold — and /s/ answers to the link's
		// CREATOR, not to whoever presents it, so the token works.
		//
		// This route is not behind proj(), so the filter is built from the
		// project in hand rather than from the request context.
		vis := s.visibilityFor(r, p)
		for _, sh := range s.Shares.List(p.ID) {
			if !vis.canRead(sh.Path) {
				continue
			}
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
