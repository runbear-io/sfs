package webapp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path"
	"sort"
	"strings"

	"github.com/runbear-io/beardrive/internal/config"
	"github.com/runbear-io/beardrive/internal/journal"
)

// Folder permissions narrow a project's level over one subtree. They are the
// same four levels as perms.go, resolved by the same rules one level down:
// orgs wall projects off from outsiders, per-project grants say what an
// insider may do with a project, and a folder rule says what they may do with
// a prefix inside it.
//
// The model is an EXCEPTION LIST on a default-open project, not an ACL tree:
// longest matching prefix wins outright, rules never union or merge, and a
// path with no matching rule gets the project level unchanged. A member
// granted on "a/" and denied on "a/b/" loses "a/b/" — that is the feature.
//
// A hub with no folder rules behaves exactly as it did before this file
// existed: ruleFor returns nothing and pathPermOf is projectPermOf.
//
// See docs/folder-permissions-prd.md. This file is Phase 0: the model, the
// resolver, and the admin API. Nothing here is enforced on a content route
// yet — that is Phase 1 (writes) and Phases 2-3 (reads).

// FolderRule restricts one prefix. It mirrors Project.Default + Project.Perms
// exactly one level down, including the empty-string sentinel: an empty
// Default means "say nothing, inherit the project level", which is how a rule
// can name a few accounts without changing what everyone else gets.
type FolderRule struct {
	Prefix  string            `json:"prefix"`            // normalized, always slash-terminated
	Default string            `json:"default,omitempty"` // "" = inherit the project level
	Perms   map[string]string `json:"perms,omitempty"`   // lowercase email → level
}

const (
	// maxFolderRules bounds one project's rule list. Every rule is consulted
	// on every path resolution (ruleFor is a linear scan) and the whole list
	// is hashed per reader (scopeTag), so this is a real cost, not a shape.
	//
	// ponytail: 64 rules, a bound rather than a measurement. A project that
	// wants more wants a prefix trie and a rules table that is not loaded
	// whole, which is a different change.
	maxFolderRules = 64
	maxPrefixLen   = 512
)

func (f FolderRule) clone() FolderRule {
	c := f
	if f.Perms != nil {
		c.Perms = make(map[string]string, len(f.Perms))
		for k, v := range f.Perms {
			c.Perms[k] = v
		}
	}
	return c
}

// normPrefix canonicalizes a folder prefix and refuses one this hub would not
// carry as a path. The stored form is always slash-terminated, which is what
// makes prefix matching unambiguous: a rule on "a/" covers "a/b.md" and does
// not accidentally cover a sibling file literally named "ab.md".
//
// The path itself is validated by journal.SafePath and config.ReservedPath —
// the same two clauses every other ingest applies (journalOps,
// cleanUploadPath). A rule may only name a path an op could name; a rule on
// something no file can ever be is a rule that silently does nothing.
func normPrefix(p string) (string, error) {
	p = strings.Trim(strings.TrimSpace(p), "/")
	if p == "" {
		return "", fmt.Errorf("a folder rule needs a prefix; to change what everyone gets on the whole project, set the project default instead")
	}
	if len(p) > maxPrefixLen {
		return "", fmt.Errorf("that prefix is too long")
	}
	if p != path.Clean(p) || !journal.SafePath(p) || config.ReservedPath(p) {
		return "", fmt.Errorf("invalid folder prefix %q", p)
	}
	return p + "/", nil
}

// ruleFor returns the rule governing a path: the longest prefix that matches.
// Unmatched paths return false and keep the project level.
//
// Longest-wins is the whole inheritance story. There is no walk up the tree
// and no merge, so "which rule applies here" has exactly one answer and a UI
// can show it without computing an effective set.
func (p Project) ruleFor(filePath string) (FolderRule, bool) {
	var best FolderRule
	found := false
	for _, r := range p.Folders {
		if !strings.HasPrefix(filePath, r.Prefix) {
			continue
		}
		if !found || len(r.Prefix) > len(best.Prefix) {
			best, found = r, true
		}
	}
	return best, found
}

// folderLevel is pathPermOf's pure core: the same rules with the request
// already resolved to an email and a base level, so it can be tested and
// reused (scopeTag) without an http.Request.
func folderLevel(p Project, email, filePath, base string) string {
	rule, ok := p.ruleFor(filePath)
	if !ok {
		return base
	}
	if l, ok := rule.Perms[email]; ok {
		return l
	}
	if rule.Default == "" {
		return base // the rule names other accounts and says nothing about this one
	}
	return rule.Default
}

// pathPermOf resolves the caller's level for one path inside a project.
//
// An admin — a project admin, and every org owner, who projectPermOf always
// answers admin for — keeps admin everywhere. That is deliberate and is the
// break-glass property: a folder rule can never lock out the people
// responsible for the org, so offboarding never orphans a subtree.
func (s *Server) pathPermOf(r *http.Request, p Project, filePath string) string {
	base := s.projectPermOf(r, p)
	if base == PermAdmin || len(p.Folders) == 0 {
		return base
	}
	return folderLevel(p, normEmail(s.requestUser(r).Email), filePath, base)
}

// requirePathPerm answers the request itself when the caller is short of level
// on a path. Same shape (and same 403 body) as requirePerm one level up.
func (s *Server) requirePathPerm(w http.ResponseWriter, r *http.Request, p Project, filePath, level string) bool {
	return s.permit(w, s.pathPermOf(r, p, filePath), level)
}

// scopeTag identifies what ONE account can currently see in a project. It
// replaces the stored permission epoch the PRD first proposed, for three
// reasons: it needs no column, no write path and no monotonicity argument; it
// cannot drift between two hub processes or survive a rollback wrong, because
// it is computed from the rules themselves; and it is per-reader, so changing
// Alice's grant does not force Liam's device to re-sync a project whose
// visible contents did not move.
//
// It is empty for a project with no rules — the overwhelming case, and the one
// where there is nothing to invalidate.
//
// Phase 3 uses this as both the client's re-sync trigger and the cache key for
// a filtered journal. Nothing reads it yet.
func scopeTag(p Project, email, base string) string {
	if len(p.Folders) == 0 {
		return ""
	}
	prefixes := make([]string, 0, len(p.Folders))
	for _, r := range p.Folders {
		prefixes = append(prefixes, r.Prefix)
	}
	sort.Strings(prefixes)
	h := sha256.New()
	fmt.Fprintf(h, "%s\n", base)
	for _, pre := range prefixes {
		fmt.Fprintf(h, "%s\t%s\n", pre, folderLevel(p, email, pre, base))
	}
	// Half a sha256 is a cache key and a change detector, not a secret: it
	// identifies a scope to the account that already holds it.
	return hex.EncodeToString(h.Sum(nil))[:32]
}

// visibleFolders is the rule list as one account may see it. A rule that hides
// a folder from the caller is omitted entirely rather than returned with a
// level: the folder's EXISTENCE is what a "none" rule conceals, so listing it
// would defeat the feature through its own admin API.
//
// This is the one place folder rules deliberately differ from project grants,
// which any member with read may list (BEA-69: a viewer seeing who has access
// is what makes "why can't I edit this?" answerable). Project grants describe
// a project the caller can already see. A folder rule can describe one they
// cannot.
func visibleFolders(p Project, email, base string) []FolderRule {
	out := make([]FolderRule, 0, len(p.Folders))
	for _, r := range p.Folders {
		if base != PermAdmin && !atLeast(folderLevel(p, email, r.Prefix, base), PermRead) {
			continue
		}
		out = append(out, r.clone())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Prefix < out[j].Prefix })
	return out
}

// ---- request plumbing ----

// ctxProjectValKey carries the Project the proj() resolver already resolved,
// for handlers that learn their path from their own body and so cannot be
// gated at registration the way a project level is.
type ctxProjectValKey struct{}

func withProject(r *http.Request, p Project) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), ctxProjectValKey{}, p))
}

func projectFromCtx(r *http.Request) (Project, bool) {
	p, ok := r.Context().Value(ctxProjectValKey{}).(Project)
	return p, ok
}

// writablePath is the folder gate for a write handler. It answers the request
// itself and reports false when the caller may not write path.
//
// True — permissively — outside hub mode and on a project with no folder
// rules: there is no rule to apply, and proj() has already enforced the
// project level. That is the "a hub with no folder rules behaves exactly as
// today" promise, expressed once instead of at every call site.
func (s *Server) writablePath(w http.ResponseWriter, r *http.Request, path string) bool {
	p, ok := projectFromCtx(r)
	if !ok || len(p.Folders) == 0 {
		return true
	}
	return s.requirePathPerm(w, r, p, path, PermWrite)
}

// writablePaths is writablePath for an operation that touches several paths at
// once (undo run). All or nothing: a partial write would leave the caller with
// half a reverted run and no way to name what was skipped.
func (s *Server) writablePaths(w http.ResponseWriter, r *http.Request, paths []string) bool {
	p, ok := projectFromCtx(r)
	if !ok || len(p.Folders) == 0 {
		return true
	}
	base := s.projectPermOf(r, p)
	if base == PermAdmin {
		return true
	}
	email := normEmail(s.requestUser(r).Email)
	for _, path := range paths {
		if !atLeast(folderLevel(p, email, path, base), PermWrite) {
			http.Error(w, permDenied(PermWrite), http.StatusForbidden)
			return false
		}
	}
	return true
}

// ---- HTTP ----

// handleProjectFolders lists the folder rules the caller may know about, plus
// their own effective level on each. Any member with read may look; the list
// is already filtered to what they can see.
func (s *Server) handleProjectFolders(w http.ResponseWriter, r *http.Request) {
	p, ok := s.project(w, r, r.PathValue("project"), PermRead)
	if !ok {
		return
	}
	base := s.projectPermOf(r, p)
	email := normEmail(s.requestUser(r).Email)
	rules := visibleFolders(p, email, base)
	out := make([]map[string]any, 0, len(rules))
	for _, rule := range rules {
		grants := make([]map[string]string, 0, len(rule.Perms))
		for e, l := range rule.Perms {
			grants = append(grants, map[string]string{"email": e, "level": l})
		}
		sort.Slice(grants, func(i, j int) bool { return grants[i]["email"] < grants[j]["email"] })
		out = append(out, map[string]any{
			"prefix":  rule.Prefix,
			"default": rule.Default,
			"grants":  grants,
			"me":      folderLevel(p, email, rule.Prefix, base),
		})
	}
	writeJSON(w, map[string]any{"folders": out, "scope": scopeTag(p, email, base)})
}

// handleProjectScope tells a device what IT may do in this project, so an
// honest client never journals an op the hub would refuse. A member who edits
// a read-only folder should have that edit reverted on the next cycle — not
// have their whole push refused forever, which is what happens to a client
// that does not ask.
//
// It reports `readonly` only. A prefix the caller may not READ is deliberately
// absent: naming it here would hand every member of the project the NAME of
// every hidden folder, which is most of what "invisible" is supposed to mean,
// and nothing in Phase 1 consumes it. How denial reaches a client without
// publishing that list is Phase 3's problem — see the PRD.
func (s *Server) handleProjectScope(w http.ResponseWriter, r *http.Request) {
	p, ok := s.project(w, r, r.PathValue("project"), PermRead)
	if !ok {
		return
	}
	base := s.projectPermOf(r, p)
	email := normEmail(s.requestUser(r).Email)
	readonly := []string{}
	for _, rule := range p.Folders {
		l := folderLevel(p, email, rule.Prefix, base)
		// Exactly read: visible, so naming it leaks nothing the caller cannot
		// already list, and writable-by-nobody is what the client must know.
		if l == PermRead && atLeast(base, PermWrite) {
			readonly = append(readonly, rule.Prefix)
		}
	}
	sort.Strings(readonly)
	writeJSON(w, map[string]any{
		"scope":    scopeTag(p, email, base),
		"readonly": readonly,
	})
}

// handleProjectFolderSet upserts one rule. Admin only.
func (s *Server) handleProjectFolderSet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("project")
	p, ok := s.project(w, r, id, PermAdmin)
	if !ok {
		return
	}
	var req struct {
		Prefix  string            `json:"prefix"`
		Default string            `json:"default"`
		Perms   map[string]string `json:"perms"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	prefix, err := normPrefix(req.Prefix)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// PermAdmin is a project-scoped capability — rename, delete, edit
	// permissions — so it has no meaning over a subtree. Refusing it here
	// keeps "who administers this project" answerable from one list, and
	// stops a folder rule from minting an administrator.
	if req.Default != "" && (!validLevel(req.Default) || req.Default == PermAdmin) {
		http.Error(w, "a folder default must be none, read, or write", http.StatusBadRequest)
		return
	}
	rule := FolderRule{Prefix: prefix, Default: req.Default}
	if len(req.Perms) > 0 {
		rule.Perms = make(map[string]string, len(req.Perms))
		for email, level := range req.Perms {
			e := normEmail(email)
			if e == "" {
				http.Error(w, "a folder grant needs an email", http.StatusBadRequest)
				return
			}
			if !validLevel(level) || level == PermAdmin {
				http.Error(w, "a folder grant must be none, read, or write", http.StatusBadRequest)
				return
			}
			if !s.grantable(w, p, e) {
				return
			}
			rule.Perms[e] = level
		}
	}
	if err := s.Projects.SetFolder(id, rule); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "prefix": prefix})
}

// handleProjectFolderClear removes a rule, reverting the subtree to the
// project level. Admin only.
func (s *Server) handleProjectFolderClear(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("project")
	if _, ok := s.project(w, r, id, PermAdmin); !ok {
		return
	}
	prefix, err := normPrefix(r.URL.Query().Get("prefix"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.Projects.ClearFolder(id, prefix); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}
