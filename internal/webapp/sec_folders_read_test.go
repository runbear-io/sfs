package webapp

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

// Phase 2 of docs/folder-permissions-prd.md: a hidden folder stops answering on
// every browser surface. Same principal as Phase 1 — BOB, a full member of the
// org and of the project, holding the project default — now denied read on
// "vault/".
//
// Note what Phase 2 does NOT do: the folder still syncs to bob's disk. That is
// Phase 3, and until it lands the hub says so in the UI and the docs.

// hiddenHub seeds a project with a real file inside a folder bob cannot read.
// Seeding as alice matters: a test where the file does not exist measures
// absence and reports it as authorization.
func hiddenHub(t *testing.T) (http.Handler, *Server, map[string]*http.Cookie, Project, string) {
	t.Helper()
	h, srv, cookies, p := permHub(t)
	if rec := doAs(t, h, "PUT", "/api/p/"+p.ID+"/upload/content?path=vault/secret.md",
		[]byte("# the secret\n"), cookies["alice"]); rec.Code != 200 {
		t.Fatalf("seed vault/secret.md: %d %s", rec.Code, rec.Body)
	}
	if rec := doAs(t, h, "PUT", "/api/p/"+p.ID+"/upload/content?path=notes/open.md",
		[]byte("# open\n"), cookies["alice"]); rec.Code != 200 {
		t.Fatalf("seed notes/open.md: %d %s", rec.Code, rec.Body)
	}
	sha := shaOfSeeded(t, h, p.ID, cookies["alice"], "vault/secret.md")
	rule := map[string]any{"prefix": "vault", "default": PermNone,
		"perms": map[string]string{"carol@x.io": PermRead}}
	if rec := doAs(t, h, "PUT", "/api/p/"+p.ID+"/folders", rule, cookies["alice"]); rec.Code != 200 {
		t.Fatalf("set hidden rule: %d %s", rec.Code, rec.Body)
	}
	return h, srv, cookies, p, sha
}

// TestSec_Folder_HiddenFolderIsNotReadableThroughAnyViewerDoor.
//
// 404, never 403: a 403 confirms the file is there, which is most of what a
// hidden folder is hiding. The same rule the project level already applies.
func TestSec_Folder_HiddenFolderIsNotReadableThroughAnyViewerDoor(t *testing.T) {
	h, _, c, p, sha := hiddenHub(t)
	base := "/api/p/" + p.ID + "/"

	for _, url := range []string{
		base + "file?path=vault/secret.md",
		base + "download?path=vault/secret.md",
		base + "render?path=vault/secret.md",
		base + "resolve?path=vault/secret.md",
		base + "blob?sha=" + sha,
		base + "render?sha=" + sha,
	} {
		rec := doAs(t, h, "GET", url, nil, c["bob"])
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: %d %s — want 404", url, rec.Code, rec.Body)
		}
		if strings.Contains(rec.Body.String(), "the secret") {
			t.Errorf("%s served the hidden content", url)
		}
	}

	// Carol is on the folder's list, so every one of those still works for
	// her. Without this the suite passes against a hub that hides everything.
	for _, url := range []string{
		base + "file?path=vault/secret.md",
		base + "blob?sha=" + sha,
	} {
		if rec := doAs(t, h, "GET", url, nil, c["carol"]); rec.Code != 200 {
			t.Errorf("carol, who is granted the folder, got %d on %s: %s", rec.Code, url, rec.Body)
		}
	}
	// And bob keeps the rest of the project.
	if rec := doAs(t, h, "GET", base+"file?path=notes/open.md", nil, c["bob"]); rec.Code != 200 {
		t.Errorf("bob lost a file outside the rule: %d %s", rec.Code, rec.Body)
	}
}

// The tree is the listing every other surface is navigated from: a hidden
// folder must not appear in it at all, not even as an empty directory.
func TestSec_Folder_HiddenFolderIsAbsentFromTheTree(t *testing.T) {
	h, _, c, p, _ := hiddenHub(t)
	rec := doAs(t, h, "GET", "/api/p/"+p.ID+"/tree", nil, c["bob"])
	if rec.Code != 200 {
		t.Fatalf("tree: %d %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "vault") || strings.Contains(rec.Body.String(), "secret.md") {
		t.Fatalf("the tree named a hidden folder: %s", rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "open.md") {
		t.Fatalf("the tree lost a visible file: %s", rec.Body)
	}
	if body := doAs(t, h, "GET", "/api/p/"+p.ID+"/tree", nil, c["carol"]).Body.String(); !strings.Contains(body, "secret.md") {
		t.Fatalf("carol, who is granted the folder, cannot see it in the tree: %s", body)
	}
}

// History is a per-path audit feed: an unfiltered row leaks the path, who
// changed it and when — the same three facts the tree would have leaked.
func TestSec_Folder_HistoryHidesTheFolder(t *testing.T) {
	h, _, c, p, _ := hiddenHub(t)
	for _, url := range []string{
		"/api/p/" + p.ID + "/history",
		"/api/p/" + p.ID + "/history?prefix=vault",
		"/api/p/" + p.ID + "/history?path=vault/secret.md",
	} {
		rec := doAs(t, h, "GET", url, nil, c["bob"])
		if rec.Code != 200 {
			t.Fatalf("%s: %d %s", url, rec.Code, rec.Body)
		}
		if strings.Contains(rec.Body.String(), "vault/secret.md") {
			t.Errorf("%s leaked a hidden path: %s", url, rec.Body)
		}
	}
	// The visible half of the feed is intact.
	if body := doAs(t, h, "GET", "/api/p/"+p.ID+"/history", nil, c["bob"]).Body.String(); !strings.Contains(body, "notes/open.md") {
		t.Fatalf("history lost the visible file: %s", body)
	}
}

// Read heat is per-path and drives the Dashboard quadrant. Hidden paths must
// not appear, and a member must not be able to CONFIRM a hidden path exists by
// reporting a read of it and watching the count come back.
func TestSec_Folder_HeatHidesTheFolderAndRefusesReportsAboutIt(t *testing.T) {
	h, srv, c, p, _ := hiddenHub(t)
	// permHub does not wire a ledger, and a skipped test is not coverage: the
	// heat surface is a §Surfaces row like any other, so give it one.
	ledger, err := OpenReadLedger(filepath.Join(t.TempDir(), "reads.json"), 0)
	if err != nil {
		t.Fatal(err)
	}
	srv.Reads = ledger

	// Alice, who may read the folder, opens the file: a real hidden-path read
	// in the ledger, so the filter has something to hide rather than an empty
	// map that would pass either way.
	if rec := doAs(t, h, "GET", "/api/p/"+p.ID+"/file?path=vault/secret.md", nil, c["alice"]); rec.Code != 200 {
		t.Fatalf("alice could not read the file: %d %s", rec.Code, rec.Body)
	}
	if rec := doAs(t, h, "GET", "/api/p/"+p.ID+"/file?path=notes/open.md", nil, c["alice"]); rec.Code != 200 {
		t.Fatalf("alice could not read the open file: %d %s", rec.Code, rec.Body)
	}
	// Control: the hidden path really is in the ledger, so a failure below is
	// the filter and not an empty ledger.
	if body := doAs(t, h, "GET", "/api/p/"+p.ID+"/heat", nil, c["alice"]).Body.String(); !strings.Contains(body, "vault/secret.md") {
		t.Fatalf("control: the hidden read was never recorded: %s", body)
	}

	heat := doAs(t, h, "GET", "/api/p/"+p.ID+"/heat", nil, c["bob"])
	if heat.Code != 200 {
		t.Fatalf("heat: %d %s", heat.Code, heat.Body)
	}
	if strings.Contains(heat.Body.String(), "vault") {
		t.Errorf("heat named a hidden folder to bob: %s", heat.Body)
	}
	if !strings.Contains(heat.Body.String(), "notes/open.md") {
		t.Errorf("heat lost the visible file: %s", heat.Body)
	}

	// A member must not be able to CONFIRM a hidden path exists by reporting a
	// read of it and watching the count come back.
	rec := doAs(t, h, "POST", "/api/p/"+p.ID+"/reads",
		map[string]any{"reads": []map[string]any{{"path": "vault/secret.md"}}}, c["bob"])
	if rec.Code == 200 && !strings.Contains(rec.Body.String(), `"accepted":0`) {
		t.Errorf("a read report about a hidden path was accepted: %s", rec.Body)
	}
}

// A share row carries the path it publishes, so listing one for a folder this
// account cannot read hands over the name AND the fact that it is public.
func TestSec_Folder_ShareListHidesLinksIntoTheFolder(t *testing.T) {
	h, srv, c, p, _ := hiddenHub(t)
	if srv.Shares == nil {
		t.Skip("sharing is off in this fixture")
	}
	if rec := doAs(t, h, "POST", "/api/p/"+p.ID+"/shares",
		map[string]any{"path": "vault/secret.md"}, c["alice"]); rec.Code != 200 {
		t.Fatalf("alice could not mint the link: %d %s", rec.Code, rec.Body)
	}
	rec := doAs(t, h, "GET", "/api/p/"+p.ID+"/shares", nil, c["bob"])
	if rec.Code == 200 && strings.Contains(rec.Body.String(), "vault") {
		t.Fatalf("the share list named a hidden folder: %s", rec.Body)
	}
}

// The loudest version of the leak: a link minted BEFORE the folder was
// restricted keeps serving the contents to the whole internet. Restricting a
// folder has to take its links with it.
func TestSec_Folder_ARestrictedFolderKillsItsOlderShareLinks(t *testing.T) {
	h, srv, c, p := permHub(t)
	if srv.Shares == nil {
		t.Skip("sharing is off in this fixture")
	}
	if rec := doAs(t, h, "PUT", "/api/p/"+p.ID+"/upload/content?path=vault/secret.md",
		[]byte("# the secret\n"), c["bob"]); rec.Code != 200 {
		t.Fatalf("seed: %d %s", rec.Code, rec.Body)
	}
	// bob mints it while the folder is still open to him.
	rec := doAs(t, h, "POST", "/api/p/"+p.ID+"/shares",
		map[string]any{"path": "vault/secret.md"}, c["bob"])
	if rec.Code != 200 {
		t.Fatalf("mint: %d %s", rec.Code, rec.Body)
	}
	var out struct {
		URL   string `json:"url"`
		Token string `json:"token"`
	}
	json.Unmarshal(rec.Body.Bytes(), &out)
	token := out.Token
	if token == "" {
		if i := strings.LastIndex(out.URL, "/s/"); i >= 0 {
			token = out.URL[i+3:]
		}
	}
	if token == "" {
		t.Fatalf("no share token in %s", rec.Body)
	}
	// Control: the link works.
	if got := doAs(t, h, "GET", "/s/"+token, nil, nil); got.Code != 200 {
		t.Fatalf("control: the fresh link does not serve: %d %s", got.Code, got.Body)
	}
	// Alice restricts the folder away from bob.
	rule := map[string]any{"prefix": "vault", "default": PermNone}
	if r2 := doAs(t, h, "PUT", "/api/p/"+p.ID+"/folders", rule, c["alice"]); r2.Code != 200 {
		t.Fatalf("restrict: %d %s", r2.Code, r2.Body)
	}
	got := doAs(t, h, "GET", "/s/"+token, nil, nil)
	if got.Code == 200 {
		t.Fatalf("a public link kept serving a folder that was restricted: %s", got.Body)
	}
	// Same 404 and the same words as a bogus token: a stranger holding a dead
	// link must not be distinguishable from a stranger guessing one.
	if got.Code != http.StatusNotFound {
		t.Errorf("dead link answered %d, want the ordinary 404", got.Code)
	}
}

// TestSec_Folder_OrgShareAuditHidesLinksIntoAHiddenFolder.
//
// The org-wide share audit walks every project the caller can read and was
// gated on the PROJECT level only. A share row carries the public /s/ token,
// and /s/ answers to the link's creator rather than to whoever presents it —
// so a member denied a folder could read the token out of this list and fetch
// the contents through a door that never asks who they are.
//
// The route is not behind proj(), so the per-request filter every other read
// surface uses was inert here.
func TestSec_Folder_OrgShareAuditHidesLinksIntoAHiddenFolder(t *testing.T) {
	h, srv, c, p, _ := hiddenHub(t)
	if srv.Shares == nil {
		t.Skip("sharing is off in this fixture")
	}
	// Alice, who may read the folder, publishes a file inside it.
	rec := doAs(t, h, "POST", "/api/p/"+p.ID+"/shares",
		map[string]any{"path": "vault/secret.md"}, c["alice"])
	if rec.Code != 200 {
		t.Fatalf("mint: %d %s", rec.Code, rec.Body)
	}
	// ...and one outside it, so the control is a filter and not an empty list.
	if r2 := doAs(t, h, "POST", "/api/p/"+p.ID+"/shares",
		map[string]any{"path": "notes/open.md"}, c["alice"]); r2.Code != 200 {
		t.Fatalf("mint open: %d %s", r2.Code, r2.Body)
	}

	audit := doAs(t, h, "GET", "/api/orgs/"+p.Org+"/shares", nil, c["bob"])
	if audit.Code != 200 {
		t.Fatalf("org share audit: %d %s", audit.Code, audit.Body)
	}
	if strings.Contains(audit.Body.String(), "vault") {
		t.Fatalf("the org share audit handed bob a link into a hidden folder: %s", audit.Body)
	}
	if !strings.Contains(audit.Body.String(), "notes/open.md") {
		t.Fatalf("the audit lost a link bob may see: %s", audit.Body)
	}
	// Carol is on the folder's list and still sees both.
	if body := doAs(t, h, "GET", "/api/orgs/"+p.Org+"/shares", nil, c["carol"]).Body.String(); !strings.Contains(body, "vault") {
		t.Fatalf("carol, who is granted the folder, lost her link: %s", body)
	}
}
