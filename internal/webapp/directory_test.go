package webapp

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// Every Directory implementation has to behave the same way from the hub's
// side, whether it owns its organizations or mirrors someone else's. This is
// the contract, run against each implementation — the same shape as
// db_conformance_test does for MetaStore backends.
//
// A read-only directory (one whose orgs live in an external identity system)
// is a first-class case: it answers reads normally and refuses writes with
// ErrManagedElsewhere, which is what lets the hub answer 409 without knowing
// anything about the system on the other side.

// readOnlyDir wraps a directory and refuses every write, the way an
// implementation backed by an external identity system does.
type readOnlyDir struct{ Directory }

func (readOnlyDir) Create(string, string) (Org, error)     { return Org{}, ErrManagedElsewhere }
func (readOnlyDir) Rename(string, string) error            { return ErrManagedElsewhere }
func (readOnlyDir) Delete(string) error                    { return ErrManagedElsewhere }
func (readOnlyDir) AddMember(string, string, string) error { return ErrManagedElsewhere }
func (readOnlyDir) SetRole(string, string, string) error   { return ErrManagedElsewhere }
func (readOnlyDir) RemoveMember(string, string) error      { return ErrManagedElsewhere }
func (readOnlyDir) CreateInvite(string, string, time.Duration) (OrgInvite, error) {
	return OrgInvite{}, ErrManagedElsewhere
}
func (readOnlyDir) ManageURL(orgID string) string { return "https://elsewhere.example/" + orgID }

func TestDirectoryConformance(t *testing.T) {
	t.Run("LocalDirectory", func(t *testing.T) {
		db, err := OpenOrgDB(filepath.Join(t.TempDir(), "orgs.json"))
		if err != nil {
			t.Fatal(err)
		}
		dirReads(t, LocalDirectory{OrgDB: db}, true)
	})

	t.Run("read-only directory", func(t *testing.T) {
		db, err := OpenOrgDB(filepath.Join(t.TempDir(), "orgs.json"))
		if err != nil {
			t.Fatal(err)
		}
		// Seed through the underlying store: an external directory's contents
		// arrive by mirroring, not through the hub's write path.
		o, err := db.Create("acme", "alice@x.io")
		if err != nil {
			t.Fatal(err)
		}
		dir := readOnlyDir{Directory: LocalDirectory{OrgDB: db}}
		dirReads(t, dir, false)

		// Reads still work on the seeded org...
		if got := dir.Role(o.ID, "alice@x.io"); got != RoleOwner {
			t.Errorf("Role = %q, want owner", got)
		}
		// ...and every write is refused the same recognizable way.
		for name, err := range map[string]error{
			"Rename":       dir.Rename(o.ID, "nope"),
			"AddMember":    dir.AddMember(o.ID, "bob@x.io", RoleMember),
			"SetRole":      dir.SetRole(o.ID, "alice@x.io", RoleMember),
			"RemoveMember": dir.RemoveMember(o.ID, "alice@x.io"),
		} {
			if !errors.Is(err, ErrManagedElsewhere) {
				t.Errorf("%s err = %v, want ErrManagedElsewhere", name, err)
			}
		}
		if _, err := dir.Create("other", "alice@x.io"); !errors.Is(err, ErrManagedElsewhere) {
			t.Errorf("Create err = %v, want ErrManagedElsewhere", err)
		}
		if _, err := dir.CreateInvite(o.ID, "alice@x.io", 0); !errors.Is(err, ErrManagedElsewhere) {
			t.Errorf("CreateInvite err = %v, want ErrManagedElsewhere", err)
		}
	})
}

// dirReads exercises the read surface every implementation must answer, and
// the writes only an owning directory supports.
func dirReads(t *testing.T, dir Directory, writable bool) {
	t.Helper()

	if writable {
		o, err := dir.Create("acme", "alice@x.io")
		if err != nil {
			t.Fatal(err)
		}
		if got, ok := dir.Get(o.ID); !ok || got.Name != "acme" {
			t.Fatalf("Get = %+v ok=%v", got, ok)
		}
		if got := dir.Role(o.ID, "ALICE@x.io"); got != RoleOwner {
			t.Errorf("Role is not email-normalized: %q", got)
		}
		if err := dir.AddMember(o.ID, "bob@x.io", RoleMember); err != nil {
			t.Fatal(err)
		}
		// The invariants live in the implementation, not in the caller.
		if err := dir.RemoveMember(o.ID, "alice@x.io"); err == nil {
			t.Error("removing the last owner must be refused")
		}
		if err := dir.SetRole(o.ID, "alice@x.io", RoleMember); err == nil {
			t.Error("demoting the last owner must be refused")
		}
		inv, err := dir.CreateInvite(o.ID, "alice@x.io", time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		if !dir.ValidInvite(inv.Token) {
			t.Error("fresh invite is not valid")
		}
		if got := dir.ListInvites(o.ID); len(got) != 1 {
			t.Errorf("ListInvites = %d, want 1", len(got))
		}
		if _, ok := dir.Redeem(inv.Token); !ok {
			t.Error("Redeem failed")
		}
		dir.RecordInviteUse(inv.Token)
		if !dir.RevokeInvite(inv.Token) {
			t.Error("RevokeInvite failed")
		}
		if dir.ValidInvite(inv.Token) {
			t.Error("revoked invite is still valid")
		}
		if len(dir.OrgsFor("bob@x.io")) != 1 {
			t.Error("OrgsFor missed the org bob belongs to")
		}
	}

	// Unknown ids are answered, never panicked on — Role in particular runs on
	// every project request including device sync.
	if got := dir.Role("o-nope", "nobody@x.io"); got != "" {
		t.Errorf("Role of a non-member = %q, want \"\"", got)
	}
	if _, ok := dir.Get("o-nope"); ok {
		t.Error("Get of an unknown org reported ok")
	}
	if dir.ValidInvite("not-a-token") {
		t.Error("bogus invite reported valid")
	}
	if dir.ManageURL("o-1234") == "" {
		t.Error("ManageURL must always give the client somewhere to go")
	}
}
