package webapp

import (
	"bytes"
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
	"time"

	"github.com/runbear-io/beardrive/internal/config"
	"github.com/runbear-io/beardrive/internal/journal"
	"github.com/runbear-io/beardrive/internal/remote"
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
	// The project gate is the OUTER wall and a folder rule may never breach
	// it: someone who cannot see the project cannot see any folder in it. A
	// rule may otherwise raise as well as lower — a read-only project with one
	// writable drop-box folder is a real shape, and only a project admin can
	// create it, who could raise the project level outright anyway.
	//
	// Today proj() answers on the project level before any handler runs, so
	// this arm is unreachable over HTTP. It is here because Phase 2 and 3 add
	// callers that are NOT behind that gate (the per-reader fold filter, the
	// blob visibility set), and a resolver that is only safe because of where
	// it happens to be called from is one refactor away from a hole.
	if base == PermNone || base == "" {
		return PermNone
	}
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

// pathFilter is one account's read visibility inside one project, resolved
// once per request. Resolving it per path would re-read the org role for every
// file in a listing; resolving it once makes the per-path test a string
// compare against a usually-empty rule list.
//
// The deliberate NON-design here is a per-reader filtered fold cached on
// RemoteSource, which the PRD first proposed. A predicate at the use sites
// needs no cache, no scope-keyed eviction and no memory per distinct scope:
// the single-path routes pay one comparison, and the listing routes already
// iterate every entry. (Phase 3's filtered JOURNAL is a different problem — a
// byte stream, not a folded map — and does need that cache.)
type pathFilter struct {
	project Project
	email   string
	base    string
	tag     string // scopeTag: identifies this account's view, for cache keys
	on      bool   // false: nothing to filter, every test is a constant true
}

// visibility resolves the caller's read visibility for the project the proj()
// resolver already resolved. Off — allowing everything — in single-volume mode
// and on any project with no folder rules, which is every project on a hub
// that has not used the feature.
func (s *Server) visibility(r *http.Request) pathFilter {
	p, ok := projectFromCtx(r)
	if !ok {
		return pathFilter{}
	}
	return s.visibilityFor(r, p)
}

// visibilityFor is visibility for a caller holding the project itself — the
// routes that are NOT behind proj() and so have no stashed Project. There is
// one, and it is a real one: the org-wide share audit walks every project in
// the org, and a share row carries the public /s/ token.
func (s *Server) visibilityFor(r *http.Request, p Project) pathFilter {
	if len(p.Folders) == 0 {
		return pathFilter{}
	}
	base := s.projectPermOf(r, p)
	if base == PermAdmin {
		return pathFilter{} // admin everywhere; nothing to hide
	}
	email := normEmail(s.requestUser(r).Email)
	return pathFilter{
		project: p, email: email, base: base,
		tag: scopeTag(p, email, base), on: true,
	}
}

// scopeTagFor is the tag alone, for the store listing to hand a device so it
// can notice its own view moved. Empty when nothing is filtered, which is what
// tells a client there is nothing to track.
func (s *Server) scopeTagFor(r *http.Request) string { return s.visibility(r).tag }

// canRead reports whether this account may see a path at all.
func (f pathFilter) canRead(path string) bool {
	if !f.on {
		return true
	}
	return atLeast(folderLevel(f.project, f.email, path, f.base), PermRead)
}

// hides reports whether the filter conceals anything, so a caller can skip
// building a visibility set it would never consult.
func (f pathFilter) hides() bool { return f.on }

// visibleSHA reports whether this account may read the content stored under
// sha, by asking whether ANY op it can read names it.
//
// The union is the point, and it is not a weakness: content that also lives at
// a path the caller may read is content they can already fetch by that path.
// Restricting one copy of bytes that exist elsewhere unrestricted hides
// nothing, and pretending otherwise would be the lie. An admin restricting a
// folder whose contents are duplicated outside it needs to be told, in the UI
// — not have this door answer a different question from the tree.
//
// Costs one pass over the project's already-parsed ops, and only when the
// filter hides something at all.
func (f pathFilter) visibleSHA(ctx context.Context, rs *RemoteSource, sha string) bool {
	if !f.hides() {
		return true
	}
	ops, err := rs.loadSourcedOps(ctx)
	if err != nil {
		// The hub cannot tell whether this is visible. Refusing is the only
		// safe answer: serving it would make a storage hiccup the way past a
		// permission.
		return false
	}
	for _, so := range ops {
		if so.Op.Blob == sha && f.canRead(so.Op.Path) {
			return true
		}
	}
	return false
}

// visibleSHAs is the set of content addresses this account may fetch: every
// blob named by an op it may read. Built once, for the doors that answer about
// many shas (the store listing) rather than one.
func (f pathFilter) visibleSHAs(ctx context.Context, rs *RemoteSource) (map[string]bool, error) {
	ops, err := rs.loadSourcedOps(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(ops))
	for _, so := range ops {
		if f.canRead(so.Op.Path) {
			out[so.Op.Blob] = true
		}
	}
	return out, nil
}

// visibleChunks is every chunk named by a manifest this account may read.
//
// Chunks cannot be judged directly — a chunk's key is its own hash, never an
// Op.Blob — so the only complete-and-leak-free answer is to expand the
// manifests of the shas that ARE visible. Deliberately not folded into
// visibleSHAs, which runs on every blob request: this fetches one object per
// visible large file, and only the store LISTING (i.e. `bdrive export`) ever
// needs it.
//
// A manifest that will not fetch or parse contributes nothing rather than
// failing the listing: its file is already unreadable by other means, and an
// export that omits one file beats an export that errors.
func (f pathFilter) visibleChunks(ctx context.Context, rs *RemoteSource) (map[string]bool, error) {
	shas, err := f.visibleSHAs(ctx, rs)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for sha := range shas {
		rc, err := rs.Backend.Get(ctx, "manifests/"+sha)
		if err != nil {
			continue // not a chunked file, or gone
		}
		var man struct {
			Chunks []struct {
				H string `json:"h"`
			} `json:"chunks"`
		}
		derr := json.NewDecoder(io.LimitReader(rc, 8<<20)).Decode(&man)
		rc.Close()
		if derr != nil {
			continue
		}
		for _, c := range man.Chunks {
			out[c.H] = true
		}
	}
	return out, nil
}

// filterJournal drops the lines of a stored journal that name paths this
// account may not read. It is the whole confidentiality claim on the sync
// wire: a device converges from the ops it is given, so an op it never
// receives is a file it never writes to disk.
//
// Line-drop on the STORED BYTES, never a re-serialization of the parsed ops.
// Re-serializing would make every journal's bytes a function of this binary's
// Marshal, so adding a field to journal.Op someday would silently rewrite the
// byte offsets every client resumes from — see §The shape that works. Keeping
// the retained lines verbatim means an upgrade cannot move them.
//
// Filtering is safe here, and only here, because of the invariant the whole
// data model rests on: a device writes only its OWN journal and merely READS
// its peers'. A filtered copy on one device's disk can therefore never
// propagate back as the truth, and journalKeepsItsOps never sees one.
//
// A line that will not parse is DROPPED rather than passed through: the hub
// cannot tell which path it names, so it cannot tell whether it may be shown.
// The client's own Parse discards it too, so nothing converges differently.
func filterJournal(data []byte, f pathFilter) []byte {
	if !f.hides() {
		return data
	}
	out := make([]byte, 0, len(data))
	for _, line := range bytes.SplitAfter(data, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		ops, err := journal.Parse(line)
		if err != nil || len(ops) != 1 {
			continue
		}
		if f.canRead(ops[0].Path) {
			out = append(out, line...)
		}
	}
	return out
}

// ---- filtered-journal cache ----

// A filtered journal is fetched by the LIST (to size it) and again by the GET
// (to serve it) within one sync cycle, per device, per peer. This keeps the
// two from being two round trips to the object store, and — more importantly —
// guarantees they are the SAME bytes: a size computed from one fetch and a
// body from another would let a journal that grew in between desynchronize the
// client's byte-offset resume.
//
// Keyed by the object's identity AND the scope that filtered it, so two
// accounts with different views never read each other's entry.
//
// ponytail: one flat map with all-or-nothing eviction at a byte ceiling,
// exactly like jcache above it, and for the same reason — a real budget shared
// across scopes with LRU is a bigger change than this feature.
type filteredJournal struct {
	size int64
	mod  time.Time
	data []byte
}

const maxFilteredJournalBytes = 32 << 20

func filterKey(key, tag string) string { return tag + "\x00" + key }

func (r *RemoteSource) cachedFilter(o remote.Object, tag string) ([]byte, bool) {
	r.fmu.Lock()
	defer r.fmu.Unlock()
	c, ok := r.fcache[filterKey(o.Key, tag)]
	if !ok {
		return nil, false
	}
	// A zero-valued Object (the GET path, which knows only the key) cannot
	// prove freshness, so it takes whatever the LIST in the same cycle stored.
	// That is the point: the two must agree, and the LIST is the one that saw
	// the object.
	if o.Size != 0 && (c.size != o.Size || !c.mod.Equal(o.Modified)) {
		return nil, false
	}
	return c.data, true
}

func (r *RemoteSource) putFilter(o remote.Object, tag string, data []byte) {
	r.fmu.Lock()
	defer r.fmu.Unlock()
	if r.fcache == nil {
		r.fcache = map[string]filteredJournal{}
	}
	if r.fbytes+int64(len(data)) > maxFilteredJournalBytes {
		r.fcache, r.fbytes = map[string]filteredJournal{}, 0
	}
	r.fcache[filterKey(o.Key, tag)] = filteredJournal{size: o.Size, mod: o.Modified, data: data}
	r.fbytes += int64(len(data))
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
// It reports two lists, and the second one is a deliberate, documented
// disclosure: `deny` names the prefixes this account may not read at all.
//
// That leaks folder NAMES to every member of the project, and there is no way
// around it that leaves the client correct. A device syncs a real local
// filesystem, so it has to know which paths it must never journal — otherwise
// a member who happens to create "vault/notes.md" locally has their whole
// journal PUT refused and their sync wedges permanently. The alternative
// (teach the client on refusal) discloses the same name to the same person the
// moment they trip it, needs new client machinery for a rare case, and leaves
// them with a silently unsynced file until then.
//
// So: a restricted folder's NAME is visible to project members; its CONTENTS,
// its file names, its history and its bytes are not. An admin who needs the
// name secret too needs a separate project, and the docs say so.
func (s *Server) handleProjectScope(w http.ResponseWriter, r *http.Request) {
	p, ok := s.project(w, r, r.PathValue("project"), PermRead)
	if !ok {
		return
	}
	base := s.projectPermOf(r, p)
	email := normEmail(s.requestUser(r).Email)
	readonly, deny := []string{}, []string{}
	for _, rule := range p.Folders {
		switch l := folderLevel(p, email, rule.Prefix, base); {
		case !atLeast(l, PermRead):
			deny = append(deny, rule.Prefix)
		case l == PermRead && atLeast(base, PermWrite):
			readonly = append(readonly, rule.Prefix)
		}
	}
	sort.Strings(readonly)
	sort.Strings(deny)
	writeJSON(w, map[string]any{
		"scope":    scopeTag(p, email, base),
		"readonly": readonly,
		"deny":     deny,
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
