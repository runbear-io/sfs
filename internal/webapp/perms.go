package webapp

import (
	"encoding/json"
	"io"
	"net/http"
	"sort"
)

// Per-project permissions. Orgs wall projects off from outsiders; these four
// ordered levels say what an insider may do with one project. The default
// level for a project is "write" — today's behavior — expressed as the empty
// string on Project.Default, so an existing hub upgrades with no migration and
// no change in behavior until someone edits permissions.
//
// One resolver (projectPerm, and projectPermOf when the caller already holds
// the Project — same rules, one fewer registry read) and one choke point (the
// proj() wrapper in server.go): every per-project route declares the level it
// needs at registration, so no handler grows its own check and a missed
// handler cannot become a silent authorization hole.

const (
	PermNone  = "none"  // the project is hidden: absent from the list, 403 everywhere
	PermRead  = "read"  // browse, view, download, history, heat
	PermWrite = "write" // + upload, sync push, share links
	PermAdmin = "admin" // + rename, delete, edit this project's permissions
)

// permRank orders the levels. An unknown level ranks as none: fail closed.
func permRank(level string) int {
	switch level {
	case PermRead:
		return 1
	case PermWrite:
		return 2
	case PermAdmin:
		return 3
	default:
		return 0
	}
}

// atLeast reports whether have satisfies want.
func atLeast(have, want string) bool { return permRank(have) >= permRank(want) }

// validLevel says whether a level is one an API caller may name.
func validLevel(l string) bool {
	return l == PermNone || l == PermRead || l == PermWrite || l == PermAdmin
}

// projectPerm resolves the request account's effective level on a project:
//
//	org owner of the project's org  → admin (always; never lockable-out)
//	not a member of that org        → none (whatever grants the project holds)
//	explicit grant                  → that level ("none" = denied)
//	member of the project's org      → the project default (write unless changed)
//	otherwise                       → none
//
// Org membership is resolved before any grant is consulted: RemoveMember does
// not walk every project's grant map, so a grant written while an account was
// a member would otherwise outlive the membership and offboarding through the
// API would not offboard. grantable() already refuses to create a grant for a
// non-member; this is the same rule at read time.
//
// The one escape hatch left is single-volume mode (no directory, no auth),
// where there is no membership model to consult. An unknown project id and an
// org-less project both fail closed: no API path produces either on a
// configured hub, and "inherited from code that no longer exists" is not a
// reason to keep an escape that makes a project world-writable.
func (s *Server) projectPerm(r *http.Request, projectID string) string {
	if s.Desktop {
		return PermRead // the desktop viewer is read-only, for everyone
	}
	if s.Dir == nil || s.Auth == nil {
		return PermAdmin
	}
	p, ok := s.Projects.Get(projectID)
	if !ok {
		return PermNone
	}
	return s.projectPermOf(r, p)
}

// projectPermOf is projectPerm for a caller that has already resolved the
// project — every per-project route does, in proj(). Resolving it twice per
// request re-read the whole registry twice (see ProjectDB.refresh): 24 ms a
// request at 5k projects on the file backend, nine unfiltered SELECTs on
// Postgres. Same rules, same fail-closed defaults; the resolution just does
// not happen again.
func (s *Server) projectPermOf(r *http.Request, p Project) string {
	if s.Desktop {
		return PermRead // the desktop viewer is read-only, for everyone
	}
	if s.Dir == nil || s.Auth == nil {
		return PermAdmin
	}
	if p.Org == "" {
		return PermNone
	}
	return s.projectPermFor(p, normEmail(s.requestUser(r).Email))
}

// projectPermFor is projectPermOf's core, keyed on an account rather than a
// request. Split out for the one caller that has no request user to speak of:
// a public share link, which has to ask whether the account that MINTED it can
// still read what it publishes (shareStillReadable).
func (s *Server) projectPermFor(p Project, email string) string {
	role := s.Dir.Role(p.Org, email)
	if role == RoleOwner {
		return PermAdmin
	}
	if role == "" {
		return PermNone // not a member of the project's org
	}
	if l, ok := p.Perms[email]; ok {
		return l
	}
	return p.level()
}

// requirePerm answers the request itself when the caller is short of level.
func (s *Server) requirePerm(w http.ResponseWriter, r *http.Request, projectID, level string) bool {
	return s.permit(w, s.projectPerm(r, projectID), level)
}

// requirePermOn is requirePerm for a caller holding the resolved project.
func (s *Server) requirePermOn(w http.ResponseWriter, r *http.Request, p Project, level string) bool {
	return s.permit(w, s.projectPermOf(r, p), level)
}

func (s *Server) permit(w http.ResponseWriter, have, want string) bool {
	if atLeast(have, want) {
		return true
	}
	http.Error(w, permDenied(want), http.StatusForbidden)
	return false
}

// permDenied is the operator-voice 403 body. Deliberately one shape for every
// level so the frontend's errorFor keeps mapping it.
func permDenied(level string) string {
	switch level {
	case PermAdmin:
		return "you need admin permission on this project"
	case PermWrite:
		return "you have read-only access to this project"
	default:
		return "you do not have access to this project"
	}
}

// ---- HTTP ----

// handleProjectPerms returns the project's permission settings: the default
// level, the caller's own effective level, and the explicit grants. Any member
// with read may look; the grants are org-internal, not secrets.
//
// Re-confirmed by the owner in BEA-69 after a read-only member reported seeing
// the whole People matrix: the benchmark is Google Drive, where a viewer can
// see who has access, and hiding grants makes "why can't I edit this?"
// unanswerable. Same call for the project's public-links list
// (handleShareList, also PermRead). Raising either to PermWrite is a product
// decision, not a hardening fix — TestReadMemberSeesSharesAndGrants pins both.
func (s *Server) handleProjectPerms(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("project")
	p, ok := s.project(w, r, id, PermRead)
	if !ok {
		return
	}
	grants := make([]map[string]string, 0, len(p.Perms))
	for email, level := range p.Perms {
		grants = append(grants, map[string]string{"email": email, "level": level})
	}
	sort.Slice(grants, func(i, j int) bool { return grants[i]["email"] < grants[j]["email"] })
	writeJSON(w, map[string]any{
		"default": p.level(),
		"me":      s.projectPermOf(r, p),
		"creator": p.Creator,
		"grants":  grants,
	})
}

// handleProjectPermDefault sets the level every org member gets without an
// explicit grant. Admin only. "admin" is not a legal default: it would make
// the last-admin rule meaningless.
func (s *Server) handleProjectPermDefault(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("project")
	if _, ok := s.project(w, r, id, PermAdmin); !ok {
		return
	}
	var req struct {
		Default string `json:"default"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if !validLevel(req.Default) || req.Default == PermAdmin {
		http.Error(w, "default must be none, read, or write", http.StatusBadRequest)
		return
	}
	if err := s.Projects.SetDefault(id, req.Default); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// handleProjectPermSet grants one account an explicit level. Admin only.
func (s *Server) handleProjectPermSet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("project")
	p, ok := s.project(w, r, id, PermAdmin)
	if !ok {
		return
	}
	var req struct {
		Level string `json:"level"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if !validLevel(req.Level) {
		http.Error(w, "level must be none, read, write, or admin", http.StatusBadRequest)
		return
	}
	email := normEmail(r.PathValue("email"))
	if !s.grantable(w, p, email) {
		return
	}
	if err := s.Projects.SetPerm(id, email, req.Level); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// handleProjectPermClear drops an explicit grant, reverting that account to
// the project default. Admin only.
func (s *Server) handleProjectPermClear(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("project")
	if _, ok := s.project(w, r, id, PermAdmin); !ok {
		return
	}
	if err := s.Projects.ClearPerm(id, normEmail(r.PathValue("email"))); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// grantable checks the target of a grant: org members only, and never an org
// owner (they are implicitly admin everywhere, so a grant on one would be
// silently ignored — and a write that quietly does nothing is worse than a
// refusal).
func (s *Server) grantable(w http.ResponseWriter, p Project, email string) bool {
	if s.Dir == nil || p.Org == "" {
		return true
	}
	switch s.Dir.Role(p.Org, email) {
	case "":
		http.Error(w, "that account is not a member of this project's organization", http.StatusBadRequest)
		return false
	case RoleOwner:
		http.Error(w, "organization owners are always project admins", http.StatusBadRequest)
		return false
	}
	return true
}

// project resolves a project id and the caller's level in one step, answering
// the request itself when either fails. A missing project is 404; an existing
// one the caller may not touch is 403 — the same answer a non-member gets, so
// the two are indistinguishable from outside.
func (s *Server) project(w http.ResponseWriter, r *http.Request, id, level string) (Project, bool) {
	if s.Projects == nil {
		http.Error(w, "this server does not host projects", http.StatusNotFound)
		return Project{}, false
	}
	p, ok := s.Projects.Get(id)
	if !ok {
		http.Error(w, "no such project", http.StatusNotFound)
		return Project{}, false
	}
	if !s.requirePermOn(w, r, p, level) {
		return Project{}, false
	}
	return p, true
}
