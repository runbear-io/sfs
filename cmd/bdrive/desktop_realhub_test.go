package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/runbear-io/beardrive/internal/config"
	"github.com/runbear-io/beardrive/internal/journal"
	"github.com/runbear-io/beardrive/internal/remote"
	"github.com/runbear-io/beardrive/internal/store"
	"github.com/runbear-io/beardrive/internal/webapp"
)

// TestDesktopAgainstARealHub drives the sidecar against a REAL webapp.Server —
// real ProjectDB, real OrgDB, real BuiltinAuth, real ReadLedger, real handlers
// — instead of the hand-written httptest fakes every other desktop test uses.
//
// Those fakes answer with JSON I wrote by hand, so they prove the sidecar
// forwards a request and nothing about whether the hub's own handlers produce
// what the app expects. A fixture that agrees with itself is exactly how the
// two halves of a wire pass their own tests and still disagree — which is the
// failure this whole area keeps having.
//
// What it pins, in one pass over the chain:
//   - a folder rule the hub really holds reaches the app through the sidecar
//   - /scope really denies it for a member who is not on the rule
//   - a file opened in the app really lands in the hub's ledger as a human read
func TestDesktopAgainstARealHub(t *testing.T) {
	hubHome := t.TempDir()
	const (
		token    = "tok-real-e2e"
		owner    = "alice@x.io"
		member   = "bob@x.io"
		devID    = "bob-mac"
		filePath = "notes/plan.md"
	)

	// A real BuiltinAuth, seeded on disk: signup and token minting are not
	// exported, and driving the whole device-code flow would be a test about
	// login rather than about this wire.
	sum := sha256.Sum256([]byte(token))
	now := time.Now().UTC()
	authJSON, _ := json.Marshal(map[string]any{
		"users": []map[string]any{
			{"id": "u-alice", "email": owner, "name": "Alice", "status": "active", "created": now},
			{"id": "u-bob", "email": member, "name": "Bob", "status": "active", "created": now},
		},
		"tokens": []map[string]any{
			{"hash": hex.EncodeToString(sum[:]), "user": "u-bob", "device": devID, "created": now},
		},
	})
	authPath := filepath.Join(hubHome, "auth.json")
	if err := os.WriteFile(authPath, authJSON, 0o600); err != nil {
		t.Fatal(err)
	}
	auth, err := webapp.OpenBuiltinAuth(authPath, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	orgs, err := webapp.OpenOrgDB(filepath.Join(hubHome, "orgs.json"))
	if err != nil {
		t.Fatal(err)
	}
	org, err := orgs.Create("acme", owner)
	if err != nil {
		t.Fatal(err)
	}
	if err := orgs.AddMember(org.ID, member, webapp.RoleMember); err != nil {
		t.Fatal(err)
	}
	projects, err := webapp.OpenProjectDB(filepath.Join(hubHome, "projects.json"))
	if err != nil {
		t.Fatal(err)
	}
	p, _, err := projects.GetOrCreate("wiki", org.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := projects.SetPerm(p.ID, member, webapp.PermWrite); err != nil {
		t.Fatal(err)
	}
	// The rule under test: bob is a full member of the project and NOT on it.
	if err := projects.SetFolder(p.ID, webapp.FolderRule{
		Prefix:  "vault/",
		Default: webapp.PermNone,
		Perms:   map[string]string{owner: webapp.PermRead},
	}); err != nil {
		t.Fatal(err)
	}
	ledger, err := webapp.OpenReadLedger(filepath.Join(hubHome, "reads.json"), 0)
	if err != nil {
		t.Fatal(err)
	}

	// The hub's storage: one real file, journaled, so the path exists in the
	// project's replayed state — handleReadReport refuses a read of nothing.
	hubStore := filepath.Join(hubHome, "storage")
	os.MkdirAll(filepath.Join(hubStore, p.ID, "journal"), 0o755)
	os.MkdirAll(filepath.Join(hubStore, p.ID, "blobs"), 0o755)
	body := []byte("# Plan\n")
	blobSum := sha256.Sum256(body)
	blob := hex.EncodeToString(blobSum[:])
	os.WriteFile(filepath.Join(hubStore, p.ID, "blobs", blob), body, 0o644)
	line, err := journal.Marshal([]journal.Op{{
		Seq: 1, Lamport: 1, Time: now, Device: "seed", Kind: journal.KindPut,
		Path: filePath, Blob: blob, Size: int64(len(body)), User: owner,
	}})
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(hubStore, p.ID, "journal", "seed.jsonl"), line, 0o644)

	backend, err := remote.Open(t.Context(), "file://"+hubStore)
	if err != nil {
		t.Fatal(err)
	}
	hubSrv := &webapp.Server{
		Root: backend, Projects: projects, Auth: auth,
		Dir: webapp.LocalDirectory{OrgDB: orgs}, Reads: ledger,
		Device: webapp.Identity{ID: "hub", Name: "hub"},
	}
	hub := httptest.NewServer(hubSrv.Handler())
	defer hub.Close()

	// Now the Mac: its own BDRIVE_HOME, one mount pointing at that real hub,
	// signed in as bob, with the same file in its local volume store.
	t.Setenv("BDRIVE_HOME", t.TempDir())
	const mountID = "m-realhub"
	folder := t.TempDir()
	remoteURL := hub.URL + "/p/" + p.ID
	if _, err := config.SaveProject(folder, config.Project{ID: mountID, Remote: remoteURL}); err != nil {
		t.Fatal(err)
	}
	if err := config.SaveMounts(map[string]config.MountInfo{mountID: {Path: folder, Remote: remoteURL}}); err != nil {
		t.Fatal(err)
	}
	if err := config.SaveSettings(config.Settings{Server: hub.URL, Token: token, Email: member, Name: "Bob"}); err != nil {
		t.Fatal(err)
	}
	volDir, err := config.VolumeDir(mountID)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(volDir)
	if err != nil {
		t.Fatal(err)
	}
	localSha, _, err := st.PutBlobBytes(body)
	if err != nil {
		t.Fatal(err)
	}
	err = st.AppendOps("seed", []journal.Op{{
		Seq: 1, Lamport: 1, Time: now, Device: "seed", Kind: journal.KindPut,
		Path: filePath, Blob: localSha, Size: int64(len(body)), User: owner,
	}})
	if err != nil {
		t.Fatal(err)
	}

	drainReads()
	app := httptest.NewServer(desktopHandler())
	defer app.Close()

	get := func(path string) (int, string) {
		t.Helper()
		resp, err := http.Get(app.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	// 1. The rule list, straight from the hub's own handler this time.
	code, out := get("/api/p/" + p.ID + "/folders")
	if code != 200 {
		t.Fatalf("folders through the sidecar: %d %s", code, out)
	}
	var folders struct {
		Folders []struct {
			Prefix string `json:"prefix"`
			Me     string `json:"me"`
		} `json:"folders"`
		Scope string `json:"scope"`
	}
	if err := json.Unmarshal([]byte(out), &folders); err != nil {
		t.Fatalf("the app cannot parse what the real hub returned: %v — %s", err, out)
	}
	// bob may not read vault/, so the hub hides the rule's very existence from
	// him. The app showing an empty list here is CORRECT — and it is the same
	// output the local registry produced for the wrong reason, which is why
	// the scope check below is the one that proves the wire.
	for _, f := range folders.Folders {
		if f.Prefix == "vault/" {
			t.Errorf("the hub disclosed a rule bob cannot read: %+v", f)
		}
	}

	// 2. Scope: the hub really tells bob's device not to write under vault/.
	code, out = get("/api/p/" + p.ID + "/scope")
	if code != 200 {
		t.Fatalf("scope through the sidecar: %d %s", code, out)
	}
	var scope struct {
		Scope    string   `json:"scope"`
		Deny     []string `json:"deny"`
		ReadOnly []string `json:"readonly"`
	}
	if err := json.Unmarshal([]byte(out), &scope); err != nil {
		t.Fatalf("the app cannot parse the real /scope: %v — %s", err, out)
	}
	if len(scope.Deny) != 1 || scope.Deny[0] != "vault/" {
		t.Errorf("deny = %v, want [vault/]: the real hub restricts it and the app must hear so", scope.Deny)
	}
	if scope.Scope == "" {
		t.Error("the scope tag is empty on a project that has a rule; a client cannot notice a change without it")
	}

	// 3. A person opens a file in the app, and it reaches the real ledger as a
	// human read under bob's account.
	if code, out := get("/api/p/" + p.ID + "/render?path=" + filePath); code != 200 {
		t.Fatalf("render: %d %s", code, out)
	}
	flushReads(context.Background())

	deadline := time.Now().Add(3 * time.Second)
	var entry webapp.HeatEntry
	for time.Now().Before(deadline) {
		entry = ledger.Heat(p.ID, "", time.Time{})[filePath]
		if entry.Human > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if entry.Human != 1 {
		t.Errorf("the hub's ledger has human=%d for %q, want 1: a file read in the Mac app "+
			"still reaches no ledger", entry.Human, filePath)
	}
	if entry.Agent != 0 {
		t.Errorf("agent=%d: a person browsing was filed as device traffic", entry.Agent)
	}
}
