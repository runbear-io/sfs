package webapp

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// ---- the resolver, without an HTTP request ----

// restricted is Snow's example from docs/folder-permissions-prd.md: a
// default-open project with one closed subtree.
func restricted() Project {
	return Project{
		ID: "p1",
		Folders: []FolderRule{{
			Prefix:  "a/",
			Default: PermNone,
			Perms:   map[string]string{"liam@x.io": PermWrite, "snow@x.io": PermWrite},
		}},
	}
}

func TestFolderLevelResolvesLongestPrefix(t *testing.T) {
	p := Project{Folders: []FolderRule{
		{Prefix: "a/", Default: PermRead},
		{Prefix: "a/b/", Default: PermNone},
		{Prefix: "a/b/c/", Default: PermWrite, Perms: map[string]string{"bob@x.io": PermRead}},
	}}
	for _, tc := range []struct {
		path, email, want string
	}{
		// No rule matches: the project level passes through untouched. This is
		// the case every path on every existing hub takes.
		{"README.md", "bob@x.io", PermWrite},
		{"ab.md", "bob@x.io", PermWrite}, // "a/" must not match a sibling by string prefix
		{"a", "bob@x.io", PermWrite},     // a file named exactly "a" is not inside "a/"

		{"a/x.md", "bob@x.io", PermRead},
		{"a/b/x.md", "bob@x.io", PermNone},   // deeper rule wins over "a/"
		{"a/b/c/x.md", "bob@x.io", PermRead}, // deepest rule, and its grant beats its default
		{"a/b/c/x.md", "eve@x.io", PermWrite},
	} {
		if got := folderLevel(p, tc.email, tc.path, PermWrite); got != tc.want {
			t.Errorf("folderLevel(%q, %q) = %q, want %q", tc.path, tc.email, got, tc.want)
		}
	}
}

// A rule with no Default names some accounts and says nothing about the rest,
// so everyone else keeps the project level. Losing this makes every rule a
// closed rule and silently hides folders an admin only meant to share.
func TestFolderRuleWithoutDefaultInherits(t *testing.T) {
	p := Project{Folders: []FolderRule{
		{Prefix: "a/", Perms: map[string]string{"bob@x.io": PermRead}},
	}}
	if got := folderLevel(p, "bob@x.io", "a/x.md", PermWrite); got != PermRead {
		t.Errorf("granted account = %q, want read", got)
	}
	if got := folderLevel(p, "eve@x.io", "a/x.md", PermWrite); got != PermWrite {
		t.Errorf("unnamed account = %q, want the project level (write)", got)
	}
	// ...and it narrows a project the reader only has read on, rather than
	// widening it: a folder rule is resolved against the caller's base level.
	if got := folderLevel(p, "eve@x.io", "a/x.md", PermRead); got != PermRead {
		t.Errorf("unnamed account on a read-only project = %q, want read", got)
	}
}

func TestFolderLevelFailsClosedOnAJunkGrant(t *testing.T) {
	p := Project{Folders: []FolderRule{
		{Prefix: "a/", Default: PermNone, Perms: map[string]string{"bob@x.io": "bogus"}},
	}}
	got := folderLevel(p, "bob@x.io", "a/x.md", PermWrite)
	if atLeast(got, PermRead) {
		t.Fatalf("a corrupt folder grant resolved to %q, which passes a read check", got)
	}
}

func TestNormPrefix(t *testing.T) {
	for in, want := range map[string]string{
		"a":        "a/",
		"a/":       "a/",
		"/a/b":     "a/b/",
		"  a/b/  ": "a/b/",
		"a/b//":    "a/b/",
	} {
		got, err := normPrefix(in)
		if err != nil || got != want {
			t.Errorf("normPrefix(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	// A prefix no op could ever name is a rule that silently governs nothing.
	for _, bad := range []string{"", "/", "..", "../etc", "a/../b", "a/./b", ".bdrive", ".bdrive/x", "a\x00b"} {
		if got, err := normPrefix(bad); err == nil {
			t.Errorf("normPrefix(%q) = %q, want an error", bad, got)
		}
	}
}

// ---- over HTTP ----

func folderRules(t *testing.T, h http.Handler, id string, c *http.Cookie) []map[string]any {
	t.Helper()
	rec := doAs(t, h, "GET", "/api/p/"+id+"/folders", nil, c)
	if rec.Code != 200 {
		t.Fatalf("GET folders: %d %s", rec.Code, rec.Body)
	}
	var out struct {
		Folders []map[string]any `json:"folders"`
		Scope   string           `json:"scope"`
	}
	json.Unmarshal(rec.Body.Bytes(), &out)
	return out.Folders
}

func scopeOf(t *testing.T, h http.Handler, id string, c *http.Cookie) string {
	t.Helper()
	rec := doAs(t, h, "GET", "/api/p/"+id+"/folders", nil, c)
	var out struct {
		Scope string `json:"scope"`
	}
	json.Unmarshal(rec.Body.Bytes(), &out)
	return out.Scope
}

// Only a project admin edits folder rules. bob has write on the project — the
// default — and write is not enough: a member who can add files must not be
// able to decide who sees them.
func TestFolderRulesAreAdminOnly(t *testing.T) {
	h, _, cookies, p := permHub(t)
	rule := map[string]any{"prefix": "a", "default": PermNone}

	if rec := doAs(t, h, "PUT", "/api/p/"+p.ID+"/folders", rule, cookies["bob"]); rec.Code != 403 {
		t.Fatalf("bob (write) set a folder rule: %d %s", rec.Code, rec.Body)
	}
	if rec := doAs(t, h, "PUT", "/api/p/"+p.ID+"/folders", rule, cookies["alice"]); rec.Code != 200 {
		t.Fatalf("alice (org owner) could not set a folder rule: %d %s", rec.Code, rec.Body)
	}
	if rec := doAs(t, h, "DELETE", "/api/p/"+p.ID+"/folders?prefix=a", nil, cookies["bob"]); rec.Code != 403 {
		t.Fatalf("bob cleared a folder rule: %d %s", rec.Code, rec.Body)
	}
	// dave is in another org: the project does not exist as far as he is
	// concerned, on this route as on every other.
	if rec := doAs(t, h, "GET", "/api/p/"+p.ID+"/folders", nil, cookies["dave"]); rec.Code != 403 && rec.Code != 404 {
		t.Fatalf("an outsider read folder rules: %d %s", rec.Code, rec.Body)
	}
}

// A folder rule may not carry admin: admin is rename/delete/edit-permissions,
// which are project-scoped, so a folder-scoped one would mean nothing while
// looking like it meant something.
func TestFolderRuleRefusesAdminLevel(t *testing.T) {
	h, _, cookies, p := permHub(t)
	for _, body := range []map[string]any{
		{"prefix": "a", "default": PermAdmin},
		{"prefix": "a", "perms": map[string]string{"bob@x.io": PermAdmin}},
	} {
		if rec := doAs(t, h, "PUT", "/api/p/"+p.ID+"/folders", body, cookies["alice"]); rec.Code != 400 {
			t.Fatalf("folder rule %v accepted admin: %d %s", body, rec.Code, rec.Body)
		}
	}
}

// The existence of a hidden folder is the thing a "none" rule conceals, so the
// rule must not appear in its subject's own listing. This is where folder
// rules deliberately differ from project grants, which every member may read.
func TestHiddenFolderRuleIsNotListedToItsSubject(t *testing.T) {
	h, _, cookies, p := permHub(t)
	rule := map[string]any{
		"prefix": "a", "default": PermNone,
		"perms": map[string]string{"carol@x.io": PermWrite},
	}
	if rec := doAs(t, h, "PUT", "/api/p/"+p.ID+"/folders", rule, cookies["alice"]); rec.Code != 200 {
		t.Fatalf("set rule: %d %s", rec.Code, rec.Body)
	}
	if got := len(folderRules(t, h, p.ID, cookies["bob"])); got != 0 {
		t.Fatalf("bob, who is denied a/, sees %d rule(s) — the folder's existence leaked", got)
	}
	if got := len(folderRules(t, h, p.ID, cookies["carol"])); got != 1 {
		t.Fatalf("carol, who is granted a/, sees %d rules, want 1", got)
	}
	if got := len(folderRules(t, h, p.ID, cookies["alice"])); got != 1 {
		t.Fatalf("alice (admin) sees %d rules, want 1", got)
	}
}

// An org owner is admin everywhere and no folder rule may change that: it is
// the break-glass property that stops an offboarding orphaning a subtree.
func TestOrgOwnerKeepsAdminUnderAClosedRule(t *testing.T) {
	h, srv, cookies, p := permHub(t)
	// A rule may not even name an owner: grantable() refuses it at the folder
	// level exactly as it does at the project level, so a write that would
	// quietly do nothing is a refusal instead.
	deny := map[string]any{"prefix": "a", "default": PermNone, "perms": map[string]string{"alice@x.io": PermNone}}
	if rec := doAs(t, h, "PUT", "/api/p/"+p.ID+"/folders", deny, cookies["alice"]); rec.Code != 400 {
		t.Fatalf("a folder rule named an org owner: %d %s", rec.Code, rec.Body)
	}
	rule := map[string]any{"prefix": "a", "default": PermNone}
	if rec := doAs(t, h, "PUT", "/api/p/"+p.ID+"/folders", rule, cookies["alice"]); rec.Code != 200 {
		t.Fatalf("set rule: %d %s", rec.Code, rec.Body)
	}
	fresh, _ := srv.Projects.Get(p.ID)
	r, _ := http.NewRequest("GET", "/", nil)
	r.AddCookie(cookies["alice"])
	if got := srv.pathPermOf(r, fresh, "a/secret.md"); got != PermAdmin {
		t.Fatalf("org owner resolved to %q inside a closed folder, want admin", got)
	}
	if got := len(folderRules(t, h, p.ID, cookies["alice"])); got != 1 {
		t.Fatal("an admin must still see a closed rule")
	}
}

// scopeTag is what Phase 3 will hand a device to notice its OWN view moved.
// Changing carol's grant must not invalidate bob's: a stored project-wide
// epoch would have re-synced every device on every edit.
func TestScopeTagIsPerReader(t *testing.T) {
	h, _, cookies, p := permHub(t)
	if got := scopeOf(t, h, p.ID, cookies["bob"]); got != "" {
		t.Fatalf("a project with no rules has scope %q, want empty", got)
	}
	set := func(perms map[string]string) {
		t.Helper()
		body := map[string]any{"prefix": "a", "default": PermNone, "perms": perms}
		if rec := doAs(t, h, "PUT", "/api/p/"+p.ID+"/folders", body, cookies["alice"]); rec.Code != 200 {
			t.Fatalf("set rule: %d %s", rec.Code, rec.Body)
		}
	}
	set(map[string]string{"carol@x.io": PermRead})
	bob1, carol1 := scopeOf(t, h, p.ID, cookies["bob"]), scopeOf(t, h, p.ID, cookies["carol"])
	if bob1 == "" || carol1 == "" || bob1 == carol1 {
		t.Fatalf("scopes are not per-reader: bob=%q carol=%q", bob1, carol1)
	}
	set(map[string]string{"carol@x.io": PermWrite})
	bob2, carol2 := scopeOf(t, h, p.ID, cookies["bob"]), scopeOf(t, h, p.ID, cookies["carol"])
	if carol2 == carol1 {
		t.Error("carol's scope did not change when her own grant did")
	}
	if bob2 != bob1 {
		t.Error("bob's scope changed when only carol's grant did — every device would re-sync")
	}
}

// A rule is upserted by prefix, not appended, and clearing one leaves the rest.
func TestFolderRulesUpsertAndClear(t *testing.T) {
	h, srv, cookies, p := permHub(t)
	put := func(prefix, def string) {
		t.Helper()
		body := map[string]any{"prefix": prefix, "default": def}
		if rec := doAs(t, h, "PUT", "/api/p/"+p.ID+"/folders", body, cookies["alice"]); rec.Code != 200 {
			t.Fatalf("put %s: %d %s", prefix, rec.Code, rec.Body)
		}
	}
	put("a", PermRead)
	put("a", PermNone) // same prefix again: replace, not duplicate
	put("b", PermRead)
	if got := len(folderRules(t, h, p.ID, cookies["alice"])); got != 2 {
		t.Fatalf("%d rules after two prefixes and one replacement, want 2", got)
	}
	fresh, _ := srv.Projects.Get(p.ID)
	if rule, ok := fresh.ruleFor("a/x.md"); !ok || rule.Default != PermNone {
		t.Fatalf("the second put did not replace the first: %+v", rule)
	}
	if rec := doAs(t, h, "DELETE", "/api/p/"+p.ID+"/folders?prefix=a", nil, cookies["alice"]); rec.Code != 200 {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}
	fresh, _ = srv.Projects.Get(p.ID)
	if _, ok := fresh.ruleFor("a/x.md"); ok {
		t.Error("the rule survived its delete")
	}
	if _, ok := fresh.ruleFor("b/x.md"); !ok {
		t.Error("deleting one rule removed another")
	}
	// Clearing a prefix that carries no rule is an error, not a silent no-op:
	// it is almost always a typo'd prefix, and reporting ok would tell an
	// admin a folder is open when the real rule is still closed.
	if rec := doAs(t, h, "DELETE", "/api/p/"+p.ID+"/folders?prefix=zzz", nil, cookies["alice"]); rec.Code != 400 {
		t.Fatalf("clearing an absent rule: %d, want 400", rec.Code)
	}
}

// A grant may only name a member of the project's org — the same rule
// grantable() applies to project-level grants, for the same reason: a grant on
// a non-member outlives nothing and offboarding must not leave one behind.
func TestFolderGrantRefusesANonMember(t *testing.T) {
	h, _, cookies, p := permHub(t)
	body := map[string]any{"prefix": "a", "default": PermNone,
		"perms": map[string]string{"dave@x.io": PermWrite}}
	if rec := doAs(t, h, "PUT", "/api/p/"+p.ID+"/folders", body, cookies["alice"]); rec.Code != 400 {
		t.Fatalf("granted a folder to a non-member: %d %s", rec.Code, rec.Body)
	}
}

// Phase 0 is the model only. A rule must not yet change what any content route
// answers — the phases land in order so "invisible" is never advertised while
// the bytes are still on every member's disk.
func TestFolderRulesDoNotYetGateContent(t *testing.T) {
	h, _, cookies, p := permHub(t)
	body := map[string]any{"prefix": "a", "default": PermNone}
	if rec := doAs(t, h, "PUT", "/api/p/"+p.ID+"/folders", body, cookies["alice"]); rec.Code != 200 {
		t.Fatalf("set rule: %d %s", rec.Code, rec.Body)
	}
	rec := doAs(t, h, "GET", "/api/p/"+p.ID+"/tree", nil, cookies["bob"])
	if rec.Code != 200 {
		t.Fatalf("Phase 0 changed a content route for a denied member: %d %s", rec.Code, rec.Body)
	}
}

// projectView embeds Project, so every field added to Project reaches the
// client unless projectJSON clears it. A folder rule can name a subtree the
// caller may not know exists — the one thing /api/p/{id}/folders filters its
// own output to prevent — so it must never ride along on a project list row.
func TestProjectListDoesNotLeakFolderRules(t *testing.T) {
	h, _, cookies, p := permHub(t)
	rule := map[string]any{"prefix": "secret-plans", "default": PermNone}
	if rec := doAs(t, h, "PUT", "/api/p/"+p.ID+"/folders", rule, cookies["alice"]); rec.Code != 200 {
		t.Fatalf("set rule: %d %s", rec.Code, rec.Body)
	}
	for _, url := range []string{"/api/projects", "/api/projects/" + p.ID} {
		rec := doAs(t, h, "GET", url, nil, cookies["bob"])
		if rec.Code != 200 {
			t.Fatalf("%s: %d %s", url, rec.Code, rec.Body)
		}
		if strings.Contains(rec.Body.String(), "secret-plans") {
			t.Fatalf("%s leaked a hidden folder's name to a denied member: %s", url, rec.Body)
		}
	}
}

// A folder rule is resolved against the caller's PROJECT level, so it narrows
// or widens a project they can already reach — it never grants entry to one
// they cannot. proj() answers first, and it answers on the project level.
//
// Worth pinning because it is the surprising half: an admin who sets a project
// to invite-only and then grants someone a folder inside it has granted them
// nothing, and should be told rather than left wondering.
func TestFolderGrantDoesNotOpenAProjectYouCannotSee(t *testing.T) {
	h, srv, cookies, p := permHub(t)
	if rec := doAs(t, h, "PUT", "/api/p/"+p.ID+"/permissions",
		map[string]any{"default": PermNone}, cookies["alice"]); rec.Code != 200 {
		t.Fatalf("make invite-only: %d %s", rec.Code, rec.Body)
	}
	rule := map[string]any{"prefix": "shared", "default": PermNone,
		"perms": map[string]string{"bob@x.io": PermWrite}}
	if rec := doAs(t, h, "PUT", "/api/p/"+p.ID+"/folders", rule, cookies["alice"]); rec.Code != 200 {
		t.Fatalf("set rule: %d %s", rec.Code, rec.Body)
	}
	// bob is denied the project, so the folder grant is unreachable.
	if rec := doAs(t, h, "GET", "/api/p/"+p.ID+"/tree", nil, cookies["bob"]); rec.Code != 403 {
		t.Fatalf("a folder grant let bob into an invite-only project: %d %s", rec.Code, rec.Body)
	}
	fresh, _ := srv.Projects.Get(p.ID)
	r, _ := http.NewRequest("GET", "/", nil)
	r.AddCookie(cookies["bob"])
	if got := srv.pathPermOf(r, fresh, "shared/x.md"); got != PermNone {
		t.Fatalf("pathPermOf = %q inside a project bob may not see, want none", got)
	}
}

// The mirror image, and the reason folderLevel starts from the caller's base
// rather than from the project default: a rule may RAISE the level on its
// subtree. A read-only project with one writable drop-box folder is a real
// shape, and only a project admin can create it — the same person who could
// raise the project level outright.
func TestFolderRuleCanWidenWithinAProjectYouCanSee(t *testing.T) {
	h, srv, cookies, p := permHub(t)
	if rec := doAs(t, h, "PUT", "/api/p/"+p.ID+"/permissions",
		map[string]any{"default": PermRead}, cookies["alice"]); rec.Code != 200 {
		t.Fatalf("make read-only: %d %s", rec.Code, rec.Body)
	}
	rule := map[string]any{"prefix": "dropbox", "perms": map[string]string{"bob@x.io": PermWrite}}
	if rec := doAs(t, h, "PUT", "/api/p/"+p.ID+"/folders", rule, cookies["alice"]); rec.Code != 200 {
		t.Fatalf("set rule: %d %s", rec.Code, rec.Body)
	}
	fresh, _ := srv.Projects.Get(p.ID)
	r, _ := http.NewRequest("GET", "/", nil)
	r.AddCookie(cookies["bob"])
	if got := srv.pathPermOf(r, fresh, "dropbox/x.md"); got != PermWrite {
		t.Errorf("pathPermOf in the drop-box = %q, want write", got)
	}
	if got := srv.pathPermOf(r, fresh, "notes/x.md"); got != PermRead {
		t.Errorf("pathPermOf outside it = %q, want the project level (read)", got)
	}
}
