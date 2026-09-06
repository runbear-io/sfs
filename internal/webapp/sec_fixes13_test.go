package webapp

// Round 14 — the refresh family, one sibling further.
//
// Round 12 gave ProjectDB a read-path refresh(). Round 13 found four siblings
// with the identical defect and fixed them — and, in OrgDB's own words, put the
// re-read at the top of the MUTATORS as well as the reads:
//
//	"It runs at the top of the MUTATORS too, not only the reads: the last-owner
//	 guard counts owners out of this map, so a stale copy lets two processes
//	 each demote 'the other' owner ... The write-side reload in the repo cannot
//	 close that — the decision is made up here, before the repo is ever called."
//
// OrgDB has refresh() on all fourteen of its locking methods. ShareDB has it on
// all six. BuiltinAuth has it on every account/token method. **ProjectDB — the
// struct the whole class started in — still has it on exactly two: List and
// Get.** GetOrCreate, Update, Delete, SetCreator, SetTemplate, SetDefault,
// SetPerm, ClearPerm, dropPerm and SetOrg all take the lock and then read
// db.byID, the copy taken at open.
//
// That is round 13's own stated lesson landing on round 13's own commit: "a fix
// applied to ONE INSTANCE of a class is not a fix", where the instance left out
// is the one the class is named after.

import (
	"path/filepath"
	"testing"
)

// secfx13ProjectDB opens a file-backed registry at path, failing the test
// rather than returning an error, so the two-process fixtures below read as the
// scenario they describe.
func secfx13ProjectDB(t *testing.T, path string) *ProjectDB {
	t.Helper()
	db, err := OpenProjectDB(path)
	if err != nil {
		t.Fatal(err)
	}
	return db
}

// TestSec_Perms_ASecondHubProcessCannotResurrectADeletedProject.
//
// Deleting a project is a revocation, and round 13 recorded exactly what it is
// supposed to revoke: TestSec_Matrix_ProjectDeleteKillsItsPublicShareLinks —
// every /s/<token> minted from it stops serving, because shareCreatorStillBelongs
// resolves the project's org and "no org on the project — cleared, never set,
// or the project is GONE — means membership cannot be established, and on a
// public route that is a refusal".
//
// ProjectDB.Delete drops the row. Every OTHER ProjectDB mutator then writes
// from a map that still has it. `put` → `PutMeta` is an unconditional upsert on
// both backends (fileProjectRepo.PutMeta: `r.byID[p.ID] = p`; sqlProjectRepo:
// `INSERT ... ON CONFLICT(id) DO UPDATE`), unlike PutPerm which refuses a
// project it cannot find. So on a hub running two processes in front of one
// store — the deployment the SQL backend exists for, and the one round 13's
// sharpest correction was about — the second process's next ordinary metadata
// write puts the deleted project back, with its org, its grants and its
// creator, and every public link it ever minted starts serving again.
//
// Controls, in order: the second process sees the project before the delete
// (so the fixture is not measuring an empty registry), the delete takes on the
// process that served it, and the share link is live before and dead after.
// Only then is the stale write made.
func TestSec_Perms_ASecondHubProcessCannotResurrectADeletedProject(t *testing.T) {
	path := filepath.Join(t.TempDir(), "projects.json")
	orgs, err := OpenOrgDB(filepath.Join(t.TempDir(), "orgs.json"))
	if err != nil {
		t.Fatal(err)
	}
	org, err := orgs.Create("acme", "alice@x.io")
	if err != nil {
		t.Fatal(err)
	}

	a := secfx13ProjectDB(t, path)
	p, _, err := a.GetOrCreate("wiki", org.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.SetPerm(p.ID, "alice@x.io", PermAdmin); err != nil {
		t.Fatal(err)
	}

	// A second hub process, up and serving before the delete.
	b := secfx13ProjectDB(t, path)
	if _, ok := b.Get(p.ID); !ok {
		t.Fatal("fixture: the second process should see the project before the delete")
	}

	// The public link this project minted, and the rule that decides whether it
	// still serves. Same Server wiring the /s/ handler uses.
	sh := Share{Token: "tok", Project: p.ID, Path: "report.md", Creator: "alice@x.io"}
	live := func(db *ProjectDB) bool {
		return (&Server{Projects: db, Dir: LocalDirectory{OrgDB: orgs}}).shareCreatorStillBelongs(sh)
	}
	if !live(a) {
		t.Fatal("fixture: the share link should be live while the project exists")
	}

	// The operator deletes the project on process A. This is the whole
	// operation: "this project, and everything it published, is gone."
	if err := a.Delete(p.ID, "op@x.io"); err != nil {
		t.Fatal(err)
	}
	if _, ok := a.Get(p.ID); ok {
		t.Fatal("fixture: the delete did not take on the process that served it")
	}
	if live(a) {
		t.Fatal("fixture: the link should already be dead on process A — " +
			"TestSec_Matrix_ProjectDeleteKillsItsPublicShareLinks says so")
	}

	// Process B now does the most ordinary thing a hub does: renames a project.
	// Its caller is authorized for THIS operation on the project it thinks it
	// has; nothing about this request is hostile.
	renameErr := b.Rename(p.ID, "wiki-renamed")

	// A third process — or process A after any read — reads the store fresh.
	fresh := secfx13ProjectDB(t, path)
	got, back := fresh.Get(p.ID)
	if back {
		t.Fatalf("a project DELETED on one hub process was put back on disk by an unrelated "+
			"metadata write on a second one (Rename returned %v), and it came back carrying "+
			"the field the public-link rule reads: org %q (creator %q, grants %v).\n"+
			"ProjectDB.refresh() is on List and Get only. Every mutator — Update/Rename, "+
			"Delete, SetCreator, SetTemplate, SetDefault, SetPerm, ClearPerm, dropPerm, "+
			"SetOrg, GetOrCreate — takes db.mu and then reads db.byID, the map loaded at "+
			"open. Round 13 put refresh() at the top of OrgDB's, ShareDB's and "+
			"BuiltinAuth's mutators for exactly this reason and left the registry the "+
			"class is named after with a read-only fix.\n"+
			"PutMeta is an unconditional upsert on BOTH backends (fileProjectRepo: "+
			"`r.byID[p.ID] = p`; sqlProjectRepo: `ON CONFLICT(id) DO UPDATE`), so the "+
			"write-side reload round 11 added cannot refuse it — the row it is asked to "+
			"write is complete and self-consistent, it is just a row that was deleted.",
			renameErr, got.Org, got.Creator, got.Perms)
	}

	if live(fresh) {
		t.Fatal("the deleted project's public share link is serving again: " +
			"shareCreatorStillBelongs resolves the project's org, the project is back, " +
			"so /s/<token> hands the org's live content to anonymous strangers after an " +
			"operator deleted the project it came from.")
	}
}

// TestSec_Perms_TheLastProjectAdminGuardSurvivesASecondHubProcess.
//
// The same defect on the same struct, in the shape OrgDB's refresh comment
// names as the reason the fix had to reach the mutators at all:
//
//	"the last-owner guard counts owners out of this map, so a stale copy lets
//	 two processes each demote 'the other' owner and leave an org nobody can
//	 administer."
//
// ProjectDB has that guard twice — SetPerm ("cannot demote the last project
// admin") and ClearPerm ("cannot remove the last project admin") — and neither
// re-reads. Two processes, one store, and each one clears the admin the other
// still sees: both calls pass a guard that counts two admins, and the project
// ends with an explicit-admin list of zero, a state neither call was allowed to
// produce and the guard exists to make unreachable.
//
// Control: the same second call against a process that HAS seen the first is
// refused, so what fails below is the staleness and not the guard.
func TestSec_Perms_TheLastProjectAdminGuardSurvivesASecondHubProcess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "projects.json")
	a := secfx13ProjectDB(t, path)
	p, _, err := a.GetOrCreate("wiki", "org1")
	if err != nil {
		t.Fatal(err)
	}
	for _, who := range []string{"alice@x.io", "bob@x.io"} {
		if err := a.SetPerm(p.ID, who, PermAdmin); err != nil {
			t.Fatal(err)
		}
	}

	// Control: on ONE process the guard holds — the second removal is refused.
	solo := secfx13ProjectDB(t, path)
	if err := solo.ClearPerm(p.ID, "bob@x.io"); err != nil {
		t.Fatal(err)
	}
	if err := solo.ClearPerm(p.ID, "alice@x.io"); err == nil {
		t.Fatal("control: one process let the last project admin be removed — the guard " +
			"is not working at all, so nothing below would mean anything")
	}
	if err := solo.SetPerm(p.ID, "bob@x.io", PermAdmin); err != nil {
		t.Fatal(err) // put the fixture back for the real scenario
	}

	// The real scenario. Process B is up and serving before the first removal.
	b := secfx13ProjectDB(t, path)
	if got, _ := b.Get(p.ID); got.Perms["alice@x.io"] != PermAdmin || got.Perms["bob@x.io"] != PermAdmin {
		t.Fatalf("fixture: B should see both admins, got %v", got.Perms)
	}

	c := secfx13ProjectDB(t, path)
	if err := c.ClearPerm(p.ID, "bob@x.io"); err != nil {
		t.Fatal(err)
	}

	// B has not served a read since. Its guard counts out of the map it loaded.
	err = b.ClearPerm(p.ID, "alice@x.io")

	fresh := secfx13ProjectDB(t, path)
	got, _ := fresh.Get(p.ID)
	if adminCount(got) == 0 {
		t.Fatalf("two hub processes each removed the project admin the other still saw, and "+
			"the project now has NO explicit admin — a state ClearPerm's guard exists to "+
			"make unreachable and refuses on a single process (asserted as the control "+
			"above). The second call returned %v and the grants on disk are %v.\n"+
			"ClearPerm and SetPerm count admins out of db.byID, the map ProjectDB loaded "+
			"at open, and neither calls refresh(). This is the cross-process TOCTOU "+
			"OrgDB.refresh's own comment says the write-side re-read cannot close — the "+
			"decision is made before the repo is ever called — and round 13 fixed it for "+
			"OrgDB's last-OWNER guard while leaving ProjectDB's last-ADMIN guard, the one "+
			"it was modelled on, reading a copy taken at boot.", err, got.Perms)
	}
}
