package webapp

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOrgLifecycle(t *testing.T) {
	db, _ := OpenOrgDB(filepath.Join(t.TempDir(), "orgs.json"))
	o, _ := db.Create("acme", "alice@x.io")
	db.AddMember(o.ID, "bob@x.io", RoleMember)

	// rename
	if err := db.Rename(o.ID, "Acme Inc"); err != nil {
		t.Fatal(err)
	}
	if got, _ := db.Get(o.ID); got.Name != "Acme Inc" {
		t.Fatalf("rename failed: %q", got.Name)
	}
	// promote bob, then the last-owner guard
	if err := db.SetRole(o.ID, "bob@x.io", RoleOwner); err != nil {
		t.Fatal(err)
	}
	if err := db.SetRole(o.ID, "alice@x.io", RoleMember); err != nil {
		t.Fatal(err) // ok: bob is still an owner
	}
	if err := db.SetRole(o.ID, "bob@x.io", RoleMember); err == nil {
		t.Fatal("demoting the last owner must be refused")
	}
	// remove member (bob is the only owner now)
	if err := db.RemoveMember(o.ID, "alice@x.io"); err != nil {
		t.Fatal(err)
	}
	if err := db.RemoveMember(o.ID, "bob@x.io"); err == nil {
		t.Fatal("removing the last owner must be refused")
	}
	// invite revoke
	inv, _ := db.CreateInvite(o.ID, "bob@x.io", 0)
	if got := db.ListInvites(o.ID); len(got) != 1 {
		t.Fatalf("invite list = %d", len(got))
	}
	if !db.RevokeInvite(inv.Token) {
		t.Fatal("revoke returned false")
	}
	if _, ok := db.Redeem(inv.Token); ok {
		t.Fatal("revoked invite still redeems")
	}
}

func TestProjectLifecycle(t *testing.T) {
	db, _ := OpenProjectDB(filepath.Join(t.TempDir(), "projects.json"))
	p, _, _ := db.GetOrCreate("wiki", "o-1")
	db.GetOrCreate("docs", "o-1")

	if err := db.Rename(p.ID, "handbook"); err != nil {
		t.Fatal(err)
	}
	if got, _ := db.Get(p.ID); got.Name != "handbook" {
		t.Fatalf("rename: %q", got.Name)
	}
	// name collision within the org is refused
	if err := db.Rename(p.ID, "docs"); err == nil {
		t.Fatal("rename to an existing org-name must be refused")
	}

	// Partial update: only the fields you pass move.
	ptr := func(s string) *string { return &s }
	if err := db.Update(p.ID, nil, ptr("the team handbook"), ptr("book-open")); err != nil {
		t.Fatal(err)
	}
	got, _ := db.Get(p.ID)
	if got.Name != "handbook" || got.Description != "the team handbook" || got.Icon != "book-open" {
		t.Fatalf("update: %+v", got)
	}
	// icon-only update leaves name and description alone
	if err := db.Update(p.ID, nil, nil, ptr("users")); err != nil {
		t.Fatal(err)
	}
	if got, _ = db.Get(p.ID); got.Name != "handbook" || got.Description != "the team handbook" || got.Icon != "users" {
		t.Fatalf("icon-only update: %+v", got)
	}
	// present-and-empty clears; absent does not
	if err := db.Update(p.ID, nil, ptr(""), ptr("")); err != nil {
		t.Fatal(err)
	}
	if got, _ = db.Get(p.ID); got.Description != "" || got.Icon != "" || got.Name != "handbook" {
		t.Fatalf("clear: %+v", got)
	}

	for _, tc := range []struct {
		what             string
		name, desc, icon *string
	}{
		{"empty name", ptr("  "), nil, nil},
		{"name over 120", ptr(strings.Repeat("x", 121)), nil, nil},
		{"sibling collision", ptr("docs"), nil, nil},
		{"description over 280", nil, ptr(strings.Repeat("d", 281)), nil},
		{"icon uppercase", nil, nil, ptr("Folder")},
		{"icon with space", nil, nil, ptr("a b")},
		{"icon over 32", nil, nil, ptr(strings.Repeat("a", 33))},
	} {
		if err := db.Update(p.ID, tc.name, tc.desc, tc.icon); err == nil {
			t.Fatalf("%s: expected an error", tc.what)
		}
	}
	// a rejected update leaves the record untouched
	if got, _ = db.Get(p.ID); got.Name != "handbook" || got.Description != "" || got.Icon != "" {
		t.Fatalf("rejected updates mutated the project: %+v", got)
	}

	if err := db.Delete(p.ID, "boss@x.io"); err != nil {
		t.Fatal(err)
	}
	if _, ok := db.Get(p.ID); ok {
		t.Fatal("deleted project still present")
	}
	// The tombstone is queryable and says who deleted it, when.
	ts, ok := db.GetDeleted(p.ID)
	if !ok || ts.DeletedBy != "boss@x.io" || ts.Deleted.IsZero() {
		t.Fatalf("tombstone: %+v (ok=%v)", ts, ok)
	}
	if got := db.ListDeleted(); len(got) != 1 || got[0].ID != p.ID {
		t.Fatalf("ListDeleted = %+v", got)
	}
	// Deleting twice is refused; mutating a tombstone is refused.
	if err := db.Delete(p.ID, "boss@x.io"); err == nil {
		t.Fatal("double delete must be refused")
	}
	if err := db.Rename(p.ID, "revived"); err == nil {
		t.Fatal("renaming a tombstone must be refused")
	}
	// The name is free again: a new project by the old name gets a fresh id.
	again, created, err := db.GetOrCreate("handbook", "o-1")
	if err != nil || !created || again.ID == p.ID {
		t.Fatalf("name reuse after delete: %+v created=%v err=%v", again, created, err)
	}
}

// Owner-only guards on the HTTP surface: a plain member is refused, an owner
// succeeds.
func TestAdminEndpointsOwnerOnly(t *testing.T) {
	h, _, alice, bob, pa := orgHubSrv(t)

	// bob is not even a member of alice's org → 403 on rename
	if rec := doAs(t, h, "PATCH", "/api/orgs/"+pa.Org, map[string]string{"name": "x"}, bob); rec.Code != http.StatusForbidden {
		t.Fatalf("non-member org rename: %d", rec.Code)
	}
	// alice (owner) can rename her org
	if rec := doAs(t, h, "PATCH", "/api/orgs/"+pa.Org, map[string]string{"name": "Alice Co"}, alice); rec.Code != 200 {
		t.Fatalf("owner org rename: %d %s", rec.Code, rec.Body)
	}
	// alice can rename her project
	if rec := doAs(t, h, "PATCH", "/api/projects/"+pa.ID, map[string]string{"name": "notes"}, alice); rec.Code != 200 {
		t.Fatalf("owner project rename: %d %s", rec.Code, rec.Body)
	}
	// bob cannot delete alice's project (not a member → 404, doesn't leak)
	if rec := doAs(t, h, "DELETE", "/api/projects/"+pa.ID, nil, bob); rec.Code == 200 {
		t.Fatal("non-member deleted a project")
	}
	// alice can delete it
	if rec := doAs(t, h, "DELETE", "/api/projects/"+pa.ID, nil, alice); rec.Code != 200 {
		t.Fatalf("owner project delete: %d %s", rec.Code, rec.Body)
	}
}

// Deleting a project purges its storage prefix from the root; deleting an
// org cascades to its projects (registry and storage) and then the org row.
// Sibling projects and other orgs are untouched.
func TestDeletePurgesStorage(t *testing.T) {
	srv, _, root := newHub(t, true, nil)
	auth, err := OpenBuiltinAuth(filepath.Join(t.TempDir(), "auth.json"), true, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv.Auth = auth
	orgs, err := OpenOrgDB(filepath.Join(t.TempDir(), "orgs.json"))
	if err != nil {
		t.Fatal(err)
	}
	srv.Dir = LocalDirectory{OrgDB: orgs}
	h := srv.Handler()
	alice := signupAndSession(t, h, "alice@x.io", "Alice", "password1")
	bob := signupAndSession(t, h, "bob@x.io", "Bob", "password1")

	create := func(name string, c *http.Cookie) Project {
		t.Helper()
		rec := doAs(t, h, "POST", "/api/projects", map[string]string{"name": name}, c)
		if rec.Code != 200 {
			t.Fatalf("create %s: %d %s", name, rec.Code, rec.Body)
		}
		var out struct {
			Project Project `json:"project"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out.Project
	}
	seed := func(id string) string {
		t.Helper()
		blob := filepath.Join(root, id, "blobs", "aa")
		if err := os.MkdirAll(filepath.Dir(blob), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(blob, []byte("content"), 0o644); err != nil {
			t.Fatal(err)
		}
		return filepath.Join(root, id)
	}

	wiki, docs, bobs := create("wiki", alice), create("docs", alice), create("bobs", bob)
	wikiDir, docsDir, bobsDir := seed(wiki.ID), seed(docs.ID), seed(bobs.ID)

	// Project delete purges exactly that project's prefix.
	if rec := doAs(t, h, "DELETE", "/api/projects/"+wiki.ID, nil, alice); rec.Code != 200 {
		t.Fatalf("project delete: %d %s", rec.Code, rec.Body)
	}
	if _, err := os.Stat(wikiDir); !os.IsNotExist(err) {
		t.Fatalf("deleted project's storage still on disk: %v", err)
	}

	// The tombstone is queryable: gone from the live list, present in
	// ?deleted=1 for a member of its org, invisible to an outsider.
	deletedList := func(c *http.Cookie) []Project {
		t.Helper()
		rec := doAs(t, h, "GET", "/api/projects?deleted=1", nil, c)
		if rec.Code != 200 {
			t.Fatalf("deleted list: %d %s", rec.Code, rec.Body)
		}
		var out struct {
			Projects []Project `json:"projects"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out.Projects
	}
	rec := doAs(t, h, "GET", "/api/projects", nil, alice)
	if strings.Contains(rec.Body.String(), wiki.ID) {
		t.Fatalf("deleted project still in the live list: %s", rec.Body)
	}
	got := deletedList(alice)
	if len(got) != 1 || got[0].ID != wiki.ID || got[0].DeletedBy != "alice@x.io" || got[0].Deleted.IsZero() {
		t.Fatalf("alice's deleted list = %+v", got)
	}
	if got := deletedList(bob); len(got) != 0 {
		t.Fatalf("bob sees another org's tombstones: %+v", got)
	}
	for _, dir := range []string{docsDir, bobsDir} {
		if _, err := os.Stat(dir); err != nil {
			t.Fatalf("sibling storage touched: %v", err)
		}
	}

	// Org delete: not for non-owners; for the owner it takes projects,
	// storage, and the org row with it.
	if rec := doAs(t, h, "DELETE", "/api/orgs/"+docs.Org, nil, bob); rec.Code != http.StatusForbidden {
		t.Fatalf("non-owner org delete: %d", rec.Code)
	}
	if rec := doAs(t, h, "DELETE", "/api/orgs/"+docs.Org, nil, alice); rec.Code != 200 {
		t.Fatalf("org delete: %d %s", rec.Code, rec.Body)
	}
	if _, err := os.Stat(docsDir); !os.IsNotExist(err) {
		t.Fatalf("deleted org's project storage still on disk: %v", err)
	}
	if _, ok := srv.Projects.Get(docs.ID); ok {
		t.Fatal("deleted org's project still registered")
	}
	if _, ok := orgs.Get(docs.Org); ok {
		t.Fatal("deleted org still present")
	}
	if _, err := os.Stat(bobsDir); err != nil {
		t.Fatalf("another org's storage touched: %v", err)
	}
}

// PATCH /api/projects/{id} is a partial update: only the keys in the body
// move, present-and-empty clears, and every validation failure is a 400. The
// permission gate is unchanged — a non-member still gets what it got before.
func TestProjectUpdatePartial(t *testing.T) {
	h, srv, alice, bob, pa := orgHubSrv(t)

	patch := func(body string, c *http.Cookie) int {
		return doAs(t, h, "PATCH", "/api/projects/"+pa.ID, []byte(body), c).Code
	}
	get := func() Project {
		t.Helper()
		p, ok := srv.Projects.Get(pa.ID)
		if !ok {
			t.Fatal("project vanished")
		}
		return p
	}

	if code := patch(`{"name":"notes"}`, alice); code != 200 {
		t.Fatalf("rename: %d", code)
	}
	if p := get(); p.Name != "notes" || p.Description != "" || p.Icon != "" {
		t.Fatalf("name-only patch touched other fields: %+v", p)
	}
	if code := patch(`{"icon":"book-open"}`, alice); code != 200 {
		t.Fatalf("icon: %d", code)
	}
	if p := get(); p.Name != "notes" || p.Icon != "book-open" {
		t.Fatalf("icon-only patch: %+v", p)
	}
	if code := patch(`{"description":"what support reads"}`, alice); code != 200 {
		t.Fatalf("description: %d", code)
	}
	if p := get(); p.Description != "what support reads" || p.Icon != "book-open" {
		t.Fatalf("description-only patch: %+v", p)
	}
	if code := patch(`{"description":""}`, alice); code != 200 {
		t.Fatalf("clear description: %d", code)
	}
	if p := get(); p.Description != "" || p.Icon != "book-open" || p.Name != "notes" {
		t.Fatalf("clearing description touched other fields: %+v", p)
	}

	// alice's org gets a sibling so the collision rule has something to hit
	if rec := doAs(t, h, "POST", "/api/projects", map[string]string{"name": "docs"}, alice); rec.Code != 200 {
		t.Fatalf("create sibling: %d %s", rec.Code, rec.Body)
	}

	for _, tc := range []struct{ what, body string }{
		{"empty name", `{"name":""}`},
		{"long name", `{"name":"` + strings.Repeat("x", 121) + `"}`},
		{"sibling collision", `{"name":"docs"}`},
		{"long description", `{"description":"` + strings.Repeat("d", 281) + `"}`},
		{"bad icon", `{"icon":"BookOpen"}`},
	} {
		if code := patch(tc.body, alice); code != http.StatusBadRequest {
			t.Fatalf("%s: got %d, want 400", tc.what, code)
		}
	}
	// and none of those touched the record
	if p := get(); p.Name != "notes" || p.Description != "" || p.Icon != "book-open" {
		t.Fatalf("rejected patches mutated the project: %+v", p)
	}

	// gate unchanged: a non-member gets 404 (the project doesn't exist for him)
	if code := patch(`{"icon":"users"}`, bob); code == 200 {
		t.Fatal("non-member updated a project")
	}
}

// The invite→join→role→remove flow over HTTP, end to end.
func TestMemberManagementHTTP(t *testing.T) {
	h, _, alice, bob, pa := orgHubSrv(t)

	// invite bob and have him join
	rec := doAs(t, h, "POST", "/api/orgs/"+pa.Org+"/invites", nil, alice)
	var inv struct {
		Token string `json:"token"`
	}
	mustJSON(t, rec, &inv)
	if rec := doAs(t, h, "POST", "/api/invites/"+inv.Token, nil, bob); rec.Code != 200 {
		t.Fatalf("bob join: %d %s", rec.Code, rec.Body)
	}
	// alice promotes bob to owner
	if rec := doAs(t, h, "PATCH", "/api/orgs/"+pa.Org+"/members/bob@x.io", map[string]string{"role": "owner"}, alice); rec.Code != 200 {
		t.Fatalf("promote: %d %s", rec.Code, rec.Body)
	}
	// alice removes bob
	if rec := doAs(t, h, "DELETE", "/api/orgs/"+pa.Org+"/members/bob@x.io", nil, alice); rec.Code != 200 {
		t.Fatalf("remove: %d %s", rec.Code, rec.Body)
	}
	// bob is out: his project list no longer shows it
	rec = doAs(t, h, "GET", "/api/projects", nil, bob)
	var list struct {
		Projects []Project `json:"projects"`
	}
	json.Unmarshal(rec.Body.Bytes(), &list)
	for _, p := range list.Projects {
		if p.ID == pa.ID {
			t.Fatal("removed member still sees the project")
		}
	}
}

// A joined invite bumps its use counter, visible in the owner's invite list.
func TestInviteUseCounter(t *testing.T) {
	h, _, alice, bob, pa := orgHubSrv(t)
	rec := doAs(t, h, "POST", "/api/orgs/"+pa.Org+"/invites", nil, alice)
	var inv struct{ Token string }
	mustJSON(t, rec, &inv)
	doAs(t, h, "POST", "/api/invites/"+inv.Token, nil, bob)

	rec = doAs(t, h, "GET", "/api/orgs/"+pa.Org+"/invites", nil, alice)
	var out struct {
		Invites []struct {
			Token   string `json:"token"`
			Uses    int    `json:"uses"`
			Creator string `json:"creator"`
		} `json:"invites"`
	}
	mustJSON(t, rec, &out)
	if len(out.Invites) != 1 || out.Invites[0].Uses != 1 {
		t.Fatalf("invite uses = %+v, want 1 join recorded", out.Invites)
	}
	if out.Invites[0].Creator == "" {
		t.Fatal("invite list should carry the creator")
	}
}
