package webapp

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// Phase 3 of docs/folder-permissions-prd.md: a hidden folder never reaches the
// device. Same principal — bob, a full member of the org and the project,
// denied read on "vault/" — now driving the sync wire rather than the browser.

// modern is a device that advertises it can track the scope tag — what every
// current bdrive sends. Tests that omit it are testing the refusal.
var modern = map[string]string{"X-Bdrive-Perms": "1"}

// storeList reads a project's store listing as one account.
func storeList(t *testing.T, h http.Handler, projectID, prefix string, c *http.Cookie) (map[string]int64, string) {
	t.Helper()
	rec := sec12agDo(t, h, "GET", "/api/p/"+projectID+"/store/list?prefix="+prefix, nil, c, modern)
	if rec.Code != 200 {
		t.Fatalf("store/list %s: %d %s", prefix, rec.Code, rec.Body)
	}
	var out struct {
		Objects []struct {
			Key  string `json:"key"`
			Size int64  `json:"size"`
		} `json:"objects"`
		Scope string `json:"scope"`
	}
	json.Unmarshal(rec.Body.Bytes(), &out)
	sizes := map[string]int64{}
	for _, o := range out.Objects {
		sizes[o.Key] = o.Size
	}
	return sizes, out.Scope
}

// TestSec_Folder_TheSyncWireNeverCarriesAHiddenOp.
//
// A device converges from the ops it is given, so this is the whole
// confidentiality claim: if the op never arrives, the file is never written to
// that machine's disk. Everything else in Phase 3 protects this one property.
func TestSec_Folder_TheSyncWireNeverCarriesAHiddenOp(t *testing.T) {
	h, _, c, p, sha := hiddenHub(t)
	key := "journal/webdev.jsonl" // the hub's own device, which the uploads journaled under

	sizes, _ := storeList(t, h, p.ID, "journal/", c["alice"])
	if len(sizes) == 0 {
		t.Fatalf("control: no journals listed at all")
	}
	for k := range sizes {
		key = k
	}

	body := sec12agDo(t, h, "GET", "/api/p/"+p.ID+"/store/object?key="+key, nil, c["bob"], modern)
	if body.Code != 200 {
		t.Fatalf("bob could not pull the journal at all: %d %s", body.Code, body.Body)
	}
	if strings.Contains(body.Body.String(), "vault/secret.md") {
		t.Fatalf("the sync wire carried an op for a hidden folder: %s", body.Body)
	}
	if !strings.Contains(body.Body.String(), "notes/open.md") {
		t.Fatalf("filtering removed the ops bob is entitled to: %s", body.Body)
	}
	// Carol is on the folder's list and gets the whole thing.
	carol := sec12agDo(t, h, "GET", "/api/p/"+p.ID+"/store/object?key="+key, nil, c["carol"], modern)
	if !strings.Contains(carol.Body.String(), "vault/secret.md") {
		t.Fatalf("carol, who is granted the folder, was filtered too: %s", carol.Body)
	}

	// Content addressing must not be the way around the rule.
	blob := sec12agDo(t, h, "GET", "/api/p/"+p.ID+"/store/object?key=blobs/"+sha, nil, c["bob"], modern)
	if blob.Code != http.StatusNotFound {
		t.Errorf("bob fetched a hidden folder's blob by sha: %d %s", blob.Code, blob.Body)
	}
	if got := sec12agDo(t, h, "GET", "/api/p/"+p.ID+"/store/object?key=blobs/"+sha, nil, c["carol"], modern); got.Code != 200 {
		t.Errorf("carol could not fetch a blob she may read: %d %s", got.Code, got.Body)
	}
	// ...and neither must existence probing.
	ex := sec12agDo(t, h, "GET", "/api/p/"+p.ID+"/store/exists?key=blobs/"+sha, nil, c["bob"], modern)
	if strings.Contains(ex.Body.String(), "true") {
		t.Errorf("store/exists confirmed a hidden blob to bob: %s", ex.Body)
	}
}

// The listed size and the served body must come from the same filter.
// syncer.pull skips a journal whose listed size did not grow and then resumes
// parsing at a BYTE OFFSET into what it already holds: a filtered body served
// against the stored size makes every device re-download every journal
// forever, and a stored size against a filtered body makes it resume
// mid-stream and mis-parse.
func TestSec_Folder_ListedJournalSizeMatchesTheFilteredBody(t *testing.T) {
	h, _, c, p, _ := hiddenHub(t)
	for _, who := range []string{"bob", "carol", "alice"} {
		sizes, _ := storeList(t, h, p.ID, "journal/", c[who])
		for key, size := range sizes {
			body := sec12agDo(t, h, "GET", "/api/p/"+p.ID+"/store/object?key="+key, nil, c[who], modern)
			if body.Code != 200 {
				t.Fatalf("%s GET %s: %d %s", who, key, body.Code, body.Body)
			}
			if int64(body.Body.Len()) != size {
				t.Errorf("%s: %s listed at %d bytes, served %d — every pull would loop or mis-resume",
					who, key, size, body.Body.Len())
			}
		}
	}
}

// The blob listing must not enumerate content this account cannot fetch: a
// listing of shas is a listing of how many files are in the folder and, with
// the sha, a key to try elsewhere.
func TestSec_Folder_BlobListingHidesUnreadableContent(t *testing.T) {
	h, _, c, p, sha := hiddenHub(t)
	bobSizes, _ := storeList(t, h, p.ID, "blobs/", c["bob"])
	if _, ok := bobSizes["blobs/"+sha]; ok {
		t.Errorf("the blob listing named a hidden folder's content to bob")
	}
	carolSizes, _ := storeList(t, h, p.ID, "blobs/", c["carol"])
	if _, ok := carolSizes["blobs/"+sha]; !ok {
		t.Errorf("the blob listing hid content carol may read: %v", carolSizes)
	}
}

// The listing carries the tag its sizes were computed under, so a device can
// notice its own view moved and re-pull from zero. Without it a revocation
// leaves every peer journal shorter than the copy on disk — and pull skips a
// journal that did not grow, so that peer's ops would never be read again.
func TestFolderScopeTagTravelsWithTheStoreListing(t *testing.T) {
	h, _, c, p, _ := hiddenHub(t)
	_, bobTag := storeList(t, h, p.ID, "journal/", c["bob"])
	_, carolTag := storeList(t, h, p.ID, "journal/", c["carol"])
	if bobTag == "" || carolTag == "" || bobTag == carolTag {
		t.Fatalf("scope tags are not per-account: bob=%q carol=%q", bobTag, carolTag)
	}
	// Alice is an org owner: admin everywhere, nothing filtered, so there is
	// nothing for her device to track.
	if _, aliceTag := storeList(t, h, p.ID, "journal/", c["alice"]); aliceTag != "" {
		t.Errorf("an unfiltered listing carried a scope tag: %q", aliceTag)
	}
	// Granting bob the folder moves HIS tag.
	rule := map[string]any{"prefix": "vault", "default": PermNone,
		"perms": map[string]string{"carol@x.io": PermRead, "bob@x.io": PermRead}}
	if rec := doAs(t, h, "PUT", "/api/p/"+p.ID+"/folders", rule, c["alice"]); rec.Code != 200 {
		t.Fatalf("regrant: %d %s", rec.Code, rec.Body)
	}
	_, bobAfter := storeList(t, h, p.ID, "journal/", c["bob"])
	if bobAfter == bobTag {
		t.Error("bob's scope tag did not move when his own access did")
	}
	_, carolAfter := storeList(t, h, p.ID, "journal/", c["carol"])
	if carolAfter != carolTag {
		t.Error("carol's scope tag moved when only bob's access changed — her device would re-sync for nothing")
	}
}

// /scope names denied prefixes, which is a deliberate disclosure: a device
// syncs a real filesystem and has to know which paths it must never journal,
// or a member who creates a colliding local path wedges their whole sync. The
// names leak; the contents do not.
func TestFolderScopeNamesDeniedPrefixes(t *testing.T) {
	h, _, c, p, _ := hiddenHub(t)
	rec := doAs(t, h, "GET", "/api/p/"+p.ID+"/scope", nil, c["bob"])
	if rec.Code != 200 {
		t.Fatalf("scope: %d %s", rec.Code, rec.Body)
	}
	var out struct {
		Deny     []string `json:"deny"`
		ReadOnly []string `json:"readonly"`
	}
	json.Unmarshal(rec.Body.Bytes(), &out)
	if len(out.Deny) != 1 || out.Deny[0] != "vault/" {
		t.Fatalf("deny = %v, want [vault/]", out.Deny)
	}
	// Carol may read it, so it is not denied to her.
	rec = doAs(t, h, "GET", "/api/p/"+p.ID+"/scope", nil, c["carol"])
	json.Unmarshal(rec.Body.Bytes(), &out)
	if len(out.Deny) != 0 {
		t.Fatalf("carol, who is granted the folder, was told it is denied: %v", out.Deny)
	}
}

// A device too old to track the scope tag resumes from a byte offset into a
// stream whose shape it cannot know has moved: after a revocation it stops
// reading that peer entirely, silently and forever. The hub refuses it instead,
// with something the person holding the laptop can act on.
//
// Confidentiality never depended on this — filtering happens on the hub, so an
// old client is served the same filtered bytes. Correctness does.
func TestOldClientIsRefusedOnAProjectWithFolderRules(t *testing.T) {
	h, _, c, p, _ := hiddenHub(t)
	// No X-Bdrive-Perms header: an older bdrive.
	rec := doAs(t, h, "GET", "/api/p/"+p.ID+"/store/list?prefix=journal/", nil, c["bob"])
	if rec.Code != http.StatusForbidden {
		t.Fatalf("an old client was served a filtered project: %d %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "upgrade bdrive") {
		t.Errorf("the refusal does not say what to do: %s", rec.Body)
	}

	// An org owner sees no filtering at all, so there is no offset to get
	// wrong and no reason to refuse them.
	if rec := doAs(t, h, "GET", "/api/p/"+p.ID+"/store/list?prefix=journal/", nil, c["alice"]); rec.Code != 200 {
		t.Errorf("an unfiltered account was refused: %d %s", rec.Code, rec.Body)
	}
}

// The refusal must not reach a project that has no folder rules: every hub in
// the world is running clients without this header today.
func TestOldClientKeepsWorkingWithoutFolderRules(t *testing.T) {
	h, _, c, p := permHub(t)
	for _, url := range []string{
		"/api/p/" + p.ID + "/store/list?prefix=journal/",
		"/api/p/" + p.ID + "/store/exists?key=blobs/" + strings.Repeat("a", 64),
	} {
		if rec := doAs(t, h, "GET", url, nil, c["bob"]); rec.Code == http.StatusForbidden {
			t.Errorf("%s refused a client on a project with no rules: %d %s", url, rec.Code, rec.Body)
		}
	}
}
