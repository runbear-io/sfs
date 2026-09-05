package webapp

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/runbear-io/beardrive/internal/journal"
)

// Phase 1 of docs/folder-permissions-prd.md: read-only folders, enforced on
// every write door.
//
// The principal under test throughout is the one an org-wall test never
// exercises and the one this feature exists to stop: BOB, a full member of the
// org, a member of the project, holding the project default (write) — and
// read-only on "locked/". Every case below is bob doing something the project
// level allows and the folder rule does not.

// folderHub is permHub with one read-only folder. bob and carol may write the
// project; only the rule narrows them.
func folderHub(t *testing.T) (http.Handler, *Server, map[string]*http.Cookie, Project) {
	t.Helper()
	h, srv, cookies, p := permHub(t)
	rule := map[string]any{"prefix": "locked", "default": PermRead}
	if rec := doAs(t, h, "PUT", "/api/p/"+p.ID+"/folders", rule, cookies["alice"]); rec.Code != 200 {
		t.Fatalf("set folder rule: %d %s", rec.Code, rec.Body)
	}
	return h, srv, cookies, p
}

// pushJournal writes one op as a device the account owns — the real sync door.
func pushJournal(t *testing.T, h http.Handler, projectID string, c *http.Cookie, dev, email, path string) *httptest.ResponseRecorder {
	t.Helper()
	ops, err := journal.Marshal([]journal.Op{{
		Seq: 1, Lamport: 1, Time: time.Now().UTC(), Device: dev, DeviceName: dev,
		User: email, UserName: email,
		Kind: journal.KindPut, Path: path, Blob: strings.Repeat("b", 64), Size: 1,
	}})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("PUT",
		"/api/p/"+projectID+"/store/object?key=journal/"+dev+".jsonl", bytes.NewReader(ops))
	req.AddCookie(c)
	req.Header.Set("X-Bdrive-Device", dev)
	req.Header.Set("X-Bdrive-Device-Name", dev)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestSec_Folder_ReadOnlyMemberCannotPushOpsForThatFolder.
//
// The sync wire is the write door a device actually uses, and a blob is inert
// until an op names it — so this is where "read-only folder" is decided. A
// member who can push to the project must not be able to push an op naming a
// path the folder rule makes read-only for them.
func TestSec_Folder_ReadOnlyMemberCannotPushOpsForThatFolder(t *testing.T) {
	h, _, c, p := folderHub(t)
	const dev = "bob-laptop"
	secRegisterDevice(t, h, p.ID, c["bob"], dev, dev, "darwin")

	// Control: the same device, the same journal door, a path outside the rule.
	if rec := pushJournal(t, h, p.ID, c["bob"], dev, "bob@x.io", "notes/a.md"); rec.Code != 200 {
		t.Fatalf("control: bob cannot push outside the rule at all: %d %s", rec.Code, rec.Body)
	}
	rec := pushJournal(t, h, p.ID, c["bob"], dev, "bob@x.io", "locked/secret.md")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("bob pushed an op into a read-only folder: %d %s", rec.Code, rec.Body)
	}
	// The refused path is named back: it is the caller's own text, and a device
	// whose push is refused with no way to tell which file did it cannot be
	// debugged by the person holding the laptop.
	if !strings.Contains(rec.Body.String(), "locked/secret.md") {
		t.Errorf("the refusal does not name the path that caused it: %s", rec.Body)
	}
}

// A journal carrying one bad op among good ones is refused WHOLE. A journal is
// append-only and its writer's own record, so the hub may not edit one — and
// storing the good ops while dropping the bad would be a silent partial sync.
func TestSec_Folder_OneBadOpRefusesTheWholeJournal(t *testing.T) {
	h, srv, c, p := folderHub(t)
	const dev = "bob-mixed"
	secRegisterDevice(t, h, p.ID, c["bob"], dev, dev, "darwin")

	ops, err := journal.Marshal([]journal.Op{
		{Seq: 1, Lamport: 1, Time: time.Now().UTC(), Device: dev, User: "bob@x.io",
			Kind: journal.KindPut, Path: "notes/ok.md", Blob: strings.Repeat("c", 64), Size: 1},
		{Seq: 2, Lamport: 2, Time: time.Now().UTC(), Device: dev, User: "bob@x.io",
			Kind: journal.KindPut, Path: "locked/bad.md", Blob: strings.Repeat("d", 64), Size: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("PUT",
		"/api/p/"+p.ID+"/store/object?key=journal/"+dev+".jsonl", bytes.NewReader(ops))
	req.AddCookie(c["bob"])
	req.Header.Set("X-Bdrive-Device", dev)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("a journal with one refused op: %d %s", rec.Code, rec.Body)
	}
	// Nothing was stored — not even the good op.
	_, v, err := srv.projectVolume(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if exists, _ := v.source.(*RemoteSource).Backend.Exists(req.Context(), "journal/"+dev+".jsonl"); exists {
		t.Fatal("a refused journal was stored anyway")
	}
}

// Every browser write door, driven by the same principal. These are the
// §Surfaces rows: a member who may write the project reaching a folder they
// may only read.
func TestSec_Folder_ReadOnlyMemberCannotWriteThroughAnyBrowserDoor(t *testing.T) {
	h, _, c, p := folderHub(t)
	base := "/api/p/" + p.ID + "/"
	sha := strings.Repeat("e", 64)

	// The file has to REALLY be there, put there by someone who may write it.
	// Without this, remove/restore/shares refuse a denied member with "no such
	// file" and the test passes against a hub with no gate at all — measuring
	// absence and reporting it as authorization.
	if rec := doAs(t, h, "PUT", base+"upload/content?path=locked/x.md",
		[]byte("secret"), c["alice"]); rec.Code != 200 {
		t.Fatalf("seed locked/x.md as the org owner: %d %s", rec.Code, rec.Body)
	}
	realSHA := shaOfSeeded(t, h, p.ID, c["alice"], "locked/x.md")

	for _, tc := range []struct {
		name, method, url string
		body              any
	}{
		{"upload/init", "POST", base + "upload/init",
			map[string]any{"path": "locked/x.md", "sha256": sha, "size": 1}},
		{"upload/content", "PUT", base + "upload/content?path=locked/x.md", []byte("hi")},
		{"upload/commit", "POST", base + "upload/commit",
			map[string]any{"path": "locked/x.md", "sha256": sha, "size": 1}},
		{"remove", "POST", base + "remove", map[string]any{"path": "locked/x.md"}},
		{"restore", "POST", base + "restore", map[string]any{"path": "locked/x.md", "sha": realSHA}},
		{"shares", "POST", base + "shares", map[string]any{"path": "locked/x.md"}},
	} {
		rec := doAs(t, h, tc.method, tc.url, tc.body, c["bob"])
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s into a read-only folder: %d %s", tc.name, rec.Code, rec.Body)
		}
	}
	// The file is still there and still its original content: nothing bob
	// tried half-applied before the refusal.
	rec := doAs(t, h, "GET", base+"file?path=locked/x.md", nil, c["alice"])
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "secret") {
		t.Fatalf("locked/x.md did not survive bob's attempts: %d %s", rec.Code, rec.Body)
	}
}

// shaOfSeeded reads a path's current content address out of history — the sha
// a restore has to name to be about a version that exists.
func shaOfSeeded(t *testing.T, h http.Handler, projectID string, c *http.Cookie, path string) string {
	t.Helper()
	rec := doAs(t, h, "GET", "/api/p/"+projectID+"/history?path="+path, nil, c)
	if rec.Code != 200 {
		t.Fatalf("history %s: %d %s", path, rec.Code, rec.Body)
	}
	var out struct {
		Entries []struct {
			Blob string `json:"blob"`
		} `json:"entries"`
	}
	json.Unmarshal(rec.Body.Bytes(), &out)
	if len(out.Entries) == 0 || out.Entries[0].Blob == "" {
		t.Fatalf("no history for %s: %s", path, rec.Body)
	}
	return out.Entries[0].Blob
}

// A share link is a permanent public read. Minting one is how a member with
// read on a restricted subtree would publish it to the internet, so it is
// gated on WRITE like every other change to that folder — not on read.
func TestSec_Folder_ReadOnlyMemberCannotShareOutOfTheFolder(t *testing.T) {
	h, _, c, p := folderHub(t)
	if rec := doAs(t, h, "PUT", "/api/p/"+p.ID+"/upload/content?path=locked/secret.md",
		[]byte("secret"), c["alice"]); rec.Code != 200 {
		t.Fatalf("seed: %d %s", rec.Code, rec.Body)
	}
	rec := doAs(t, h, "POST", "/api/p/"+p.ID+"/shares",
		map[string]any{"path": "locked/secret.md"}, c["bob"])
	if rec.Code != http.StatusForbidden {
		t.Fatalf("bob minted a public link for a folder he may only read: %d %s", rec.Code, rec.Body)
	}
}

// The control every one of the above needs: the rule narrows one subtree and
// changes nothing else. Without this, a test suite that only proves refusals
// passes just as well against a hub that refuses everything.
func TestFolderRuleLeavesTheRestOfTheProjectWritable(t *testing.T) {
	h, _, c, p := folderHub(t)
	base := "/api/p/" + p.ID + "/"
	for _, tc := range []struct {
		name, method, url string
		body              any
	}{
		{"upload/init", "POST", base + "upload/init",
			map[string]any{"path": "notes/x.md", "sha256": strings.Repeat("f", 64), "size": 1}},
		{"shares", "POST", base + "shares", map[string]any{"path": "notes/x.md"}},
		{"remove", "POST", base + "remove", map[string]any{"path": "notes/x.md"}},
	} {
		if rec := doAs(t, h, tc.method, tc.url, tc.body, c["bob"]); rec.Code == http.StatusForbidden {
			t.Errorf("%s outside the rule was refused: %d %s", tc.name, rec.Code, rec.Body)
		}
	}
}

// An org owner is admin everywhere, so no folder rule may stop them writing.
func TestFolderRuleNeverBlocksAnOrgOwner(t *testing.T) {
	h, _, c, p := folderHub(t)
	rec := doAs(t, h, "POST", "/api/p/"+p.ID+"/upload/init",
		map[string]any{"path": "locked/x.md", "sha256": strings.Repeat("a", 64), "size": 1}, c["alice"])
	if rec.Code == http.StatusForbidden {
		t.Fatalf("an org owner was locked out of a folder: %d %s", rec.Code, rec.Body)
	}
}

// /scope is what stops an honest client wedging itself: a member who edits a
// read-only folder should have the edit reverted next cycle, not have their
// whole push refused forever.
//
// Phase 1 of this feature reported read-only prefixes ONLY, on the reasoning
// that naming a denied one leaks the folder's name to every member. Phase 3
// reversed that deliberately, and this test was rewritten with it rather than
// deleted: a device syncs a real filesystem, so it has to know which paths it
// must never journal, or a member who creates a colliding local path has their
// whole journal refused and their sync wedges permanently. The name leaks; the
// contents, the file names inside, the history and the bytes do not. See
// handleProjectScope and docs/folder-permissions-prd.md §Known gaps 5.
func TestFolderScopeReportsWhatADeviceMustNotWrite(t *testing.T) {
	h, _, c, p := folderHub(t)
	hidden := map[string]any{"prefix": "vault", "default": PermNone}
	if rec := doAs(t, h, "PUT", "/api/p/"+p.ID+"/folders", hidden, c["alice"]); rec.Code != 200 {
		t.Fatalf("set hidden rule: %d %s", rec.Code, rec.Body)
	}
	rec := doAs(t, h, "GET", "/api/p/"+p.ID+"/scope", nil, c["bob"])
	if rec.Code != 200 {
		t.Fatalf("scope: %d %s", rec.Code, rec.Body)
	}
	var out struct {
		Scope    string   `json:"scope"`
		ReadOnly []string `json:"readonly"`
		Deny     []string `json:"deny"`
	}
	json.Unmarshal(rec.Body.Bytes(), &out)
	if len(out.ReadOnly) != 1 || out.ReadOnly[0] != "locked/" {
		t.Fatalf("readonly = %v, want [locked/]", out.ReadOnly)
	}
	if len(out.Deny) != 1 || out.Deny[0] != "vault/" {
		t.Fatalf("deny = %v, want [vault/] — a device that is not told cannot avoid wedging", out.Deny)
	}
	// The NAME is all that leaks. Nothing about what is inside it does.
	if strings.Contains(rec.Body.String(), "secret") {
		t.Fatalf("scope leaked a hidden folder's contents: %s", rec.Body)
	}
	if out.Scope == "" {
		t.Error("scope tag is empty on a project that has rules")
	}
}
