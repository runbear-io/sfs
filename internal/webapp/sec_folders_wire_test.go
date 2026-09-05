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

// TestRevokingAFolderDoesNotBrickTheDevicesJournal.
//
// push sends the WHOLE local journal file every cycle — it is one append-only
// object — so the body repeats every op the device has ever written. Judging
// that history against today's folder rules means a member who had access to a
// folder, wrote there, and then lost it has every later push refused over ops
// the hub itself already holds: their sync is dead for the entire project,
// permanently, with no recovery but editing their journal by hand.
//
// The rule is that only what a push ADDS is judged. Everything the hub already
// stored was authorized when it was written and is already being served
// (filtered) to everyone.
func TestRevokingAFolderDoesNotBrickTheDevicesJournal(t *testing.T) {
	h, _, c, p := permHub(t)
	const dev = "bob-laptop"
	secRegisterDevice(t, h, p.ID, c["bob"], dev, dev, "darwin")

	push := func(ops []journal.Op) *httptest.ResponseRecorder {
		t.Helper()
		body, err := journal.Marshal(ops)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest("PUT",
			"/api/p/"+p.ID+"/store/object?key=journal/"+dev+".jsonl", bytes.NewReader(body))
		req.AddCookie(c["bob"])
		req.Header.Set("X-Bdrive-Device", dev)
		req.Header.Set("X-Bdrive-Perms", "1")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	op := func(seq int64, path string) journal.Op {
		return journal.Op{
			Seq: seq, Lamport: seq, Time: time.Now().UTC(), Device: dev, User: "bob@x.io",
			Kind: journal.KindPut, Path: path,
			Blob: strings.Repeat(string(rune('a'+seq)), 64), Size: 1,
		}
	}

	// While bob may write it, he journals a file inside what will become a
	// restricted folder — plus ordinary work outside it.
	history := []journal.Op{op(1, "notes/a.md"), op(2, "vault/mine.md")}
	if rec := push(history); rec.Code != 200 {
		t.Fatalf("control: bob's original push failed: %d %s", rec.Code, rec.Body)
	}

	// Alice restricts the folder away from him.
	rule := map[string]any{"prefix": "vault", "default": PermNone}
	if rec := doAs(t, h, "PUT", "/api/p/"+p.ID+"/folders", rule, c["alice"]); rec.Code != 200 {
		t.Fatalf("restrict: %d %s", rec.Code, rec.Body)
	}

	// Bob's next cycle sends the whole journal again — the two old ops plus one
	// new one, entirely outside the restricted folder.
	next := append(append([]journal.Op{}, history...), op(3, "notes/b.md"))
	if rec := push(next); rec.Code != 200 {
		t.Fatalf("a revocation bricked bob's journal: %d %s — every later push is refused "+
			"over ops the hub already holds", rec.Code, rec.Body)
	}

	// The rule still bites on anything NEW inside the folder.
	bad := append(append([]journal.Op{}, next...), op(4, "vault/after.md"))
	if rec := push(bad); rec.Code != http.StatusForbidden {
		t.Fatalf("a new op inside the restricted folder was accepted: %d %s", rec.Code, rec.Body)
	}
}

// TestSec_Folder_ARewrittenSeqCannotSmuggleAHiddenPath.
//
// "Already stored, do not re-judge" has to mean the same OP, not the same
// number. journalKeepsItsOps deliberately refuses truncation but not
// rewriting-in-place, so a device may replace what Seq 1 says — and if the
// folder gate skipped every Seq the hub already holds, re-pointing an old Seq
// at a restricted path would walk straight past it.
//
// This is the hole the first version of the brick fix opened, caught by the
// phase-1 test it broke.
func TestSec_Folder_ARewrittenSeqCannotSmuggleAHiddenPath(t *testing.T) {
	h, _, c, p := folderHub(t) // "locked/" is read-only for bob
	const dev = "bob-rewriter"
	secRegisterDevice(t, h, p.ID, c["bob"], dev, dev, "darwin")

	push := func(ops []journal.Op) *httptest.ResponseRecorder {
		t.Helper()
		body, err := journal.Marshal(ops)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest("PUT",
			"/api/p/"+p.ID+"/store/object?key=journal/"+dev+".jsonl", bytes.NewReader(body))
		req.AddCookie(c["bob"])
		req.Header.Set("X-Bdrive-Device", dev)
		req.Header.Set("X-Bdrive-Perms", "1")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	at := func(seq int64, path string) journal.Op {
		return journal.Op{
			Seq: seq, Lamport: seq, Time: time.Now().UTC(), Device: dev, User: "bob@x.io",
			Kind: journal.KindPut, Path: path, Blob: strings.Repeat("f", 64), Size: 1,
		}
	}

	if rec := push([]journal.Op{at(1, "notes/ok.md")}); rec.Code != 200 {
		t.Fatalf("control: the first push failed: %d %s", rec.Code, rec.Body)
	}
	// Same Seq, different path — into the folder bob may only read.
	if rec := push([]journal.Op{at(1, "locked/smuggled.md")}); rec.Code != http.StatusForbidden {
		t.Fatalf("re-pointing Seq 1 at a read-only folder was accepted: %d %s", rec.Code, rec.Body)
	}
	// And re-sending the genuine op is still fine — that is the protocol.
	if rec := push([]journal.Op{at(1, "notes/ok.md"), at(2, "notes/two.md")}); rec.Code != 200 {
		t.Fatalf("re-sending stored history was refused: %d %s", rec.Code, rec.Body)
	}
}

// TestSec_Folder_ManifestIsGatedLikeTheBlobItStandsFor.
//
// Delta sync stores a large file as chunks plus a manifest, and the manifest's
// key is the FILE's sha — the same key space as blobs/. Gating blobs/ and not
// manifests/ was a full disclosure of any hidden file over the chunking
// threshold: the manifest names its chunks and the chunks are the content.
//
// chunks/ is deliberately NOT gated — a chunk's key is its own hash, never an
// Op.Blob, so it is in no visible-sha set and gating it would make every large
// file unfetchable for a filtered account. It is safe because a chunk sha is
// reachable only through a manifest, which is gated, and chunks are kept out
// of the listing.
func TestSec_Folder_ManifestIsGatedLikeTheBlobItStandsFor(t *testing.T) {
	h, _, c, p, sha := hiddenHub(t)
	for _, key := range []string{"blobs/" + sha, "manifests/" + sha} {
		rec := sec12agDo(t, h, "GET", "/api/p/"+p.ID+"/store/object?key="+key, nil, c["bob"], modern)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s answered a denied member: %d %s", key, rec.Code, rec.Body)
		}
		ex := sec12agDo(t, h, "GET", "/api/p/"+p.ID+"/store/exists?key="+key, nil, c["bob"], modern)
		if strings.Contains(ex.Body.String(), "true") {
			t.Errorf("store/exists confirmed %s to a denied member: %s", key, ex.Body)
		}
	}
	// Carol may read the folder, so the same doors answer for her — a manifest
	// that does not exist yet is a 404 about absence, not about permission,
	// which is why only the blob is asserted to succeed here.
	if rec := sec12agDo(t, h, "GET", "/api/p/"+p.ID+"/store/object?key=blobs/"+sha, nil, c["carol"], modern); rec.Code != 200 {
		t.Errorf("carol was refused content she may read: %d %s", rec.Code, rec.Body)
	}
	// Chunks are listed only when a manifest this account may read names them.
	// These fixtures are all far under the chunking threshold, so nothing here
	// is chunked and the correct answer is an empty list — the assertion that
	// matters is that it does not error, since the path it takes now fetches
	// manifests. chunks_test.go covers real chunked content.
	sizes, _ := storeList(t, h, p.ID, "chunks/", c["bob"])
	if len(sizes) != 0 {
		t.Errorf("chunks were listed for content nothing chunked: %v", sizes)
	}
}
