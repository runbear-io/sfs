package webapp

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A metaBackend is one MetaStore implementation under test. reset clears any
// shared durable state before the run; open returns a fresh store over the
// SAME underlying storage, so the suite can write, reopen, and prove the data
// persisted.
type metaBackend struct {
	name  string
	reset func(t *testing.T)
	open  func(t *testing.T) MetaStore
}

func metaBackends(t *testing.T) []metaBackend {
	dir := t.TempDir()
	sqlitePath := filepath.Join(t.TempDir(), "meta.db")
	backends := []metaBackend{
		{
			name:  "file",
			reset: func(t *testing.T) {},
			open: func(t *testing.T) MetaStore {
				s, err := OpenFileStore(dir)
				if err != nil {
					t.Fatal(err)
				}
				return s
			},
		},
		{
			name:  "sqlite",
			reset: func(t *testing.T) {},
			open: func(t *testing.T) MetaStore {
				s, err := OpenSQLStore("sqlite", sqlitePath)
				if err != nil {
					t.Fatal(err)
				}
				return s
			},
		},
	}
	// Postgres/Supabase is exercised only when a DSN is reachable.
	if dsn := os.Getenv("BDRIVE_TEST_POSTGRES"); dsn != "" {
		backends = append(backends, metaBackend{
			name: "postgres",
			reset: func(t *testing.T) {
				db, err := sql.Open("pgx", dsn)
				if err != nil {
					t.Fatalf("postgres reset: %v", err)
				}
				defer db.Close()
				// EVERY table migrate() creates, schema_meta included. A
				// partial reset is not a reset: dropping projects while
				// leaving schema_meta recording version 1 reproduces, exactly,
				// the half-applied migration addColumns refuses to touch — the
				// next open rebuilds projects WITHOUT the guarded
				// default_level column and then finds a version that says it
				// should be there. It also used to leave project_perms and
				// device_rows rows behind for the following test to inherit.
				db.Exec(`DROP TABLE IF EXISTS accounts, tokens, auth_policy, projects, project_perms,
					orgs, org_members, invites, shares, devices, device_rows, read_stats,
					read_sessions, meta_version, schema_meta`)
			},
			open: func(t *testing.T) MetaStore {
				s, err := OpenSQLStore("pgx", dsn)
				if err != nil {
					t.Fatal(err)
				}
				return s
			},
		})
	} else {
		t.Log("BDRIVE_TEST_POSTGRES not set — postgres backend UNTESTED in this run")
	}
	return backends
}

// TestMetaStoreConformance runs the same service-level operations against every
// backend, then reopens the store and asserts the data survived — covering
// accounts+tokens, the signup policy, pending/approve, projects (create-or-join,
// rename, delete), orgs (roles), invites (create/redeem/uses/validity), shares
// (create/revoke/expiry), and devices.
func TestMetaStoreConformance(t *testing.T) {
	for _, be := range metaBackends(t) {
		t.Run(be.name, func(t *testing.T) {
			be.reset(t)

			// ---- write everything through the services ----
			st := be.open(t)

			auth, err := NewBuiltinAuth(st.Accounts(), true, nil)
			if err != nil {
				t.Fatal(err)
			}
			// account + token
			u, err := auth.signup("dev@x.io", "Dev", "password1")
			if err != nil {
				t.Fatal(err)
			}
			tok, err := auth.issueToken(u.ID, "cli")
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := auth.userForToken(tok); !ok {
				t.Fatal("token should authenticate its account")
			}
			// pending + policy
			if err := auth.SetPolicy(false, true); err != nil { // require approval
				t.Fatal(err)
			}
			pend, err := auth.signup("pending@x.io", "Pend", "password1")
			if err != nil {
				t.Fatal(err)
			}
			if pend.Status != statusPending {
				t.Fatalf("new account status = %q, want pending", pend.Status)
			}

			projects, err := NewProjectDB(st.Projects())
			if err != nil {
				t.Fatal(err)
			}
			p1, created, err := projects.GetOrCreate("wiki", "o-1")
			if err != nil || !created {
				t.Fatalf("create wiki: created=%v err=%v", created, err)
			}
			if _, again, _ := projects.GetOrCreate("wiki", "o-1"); again {
				t.Fatal("same name+org must join, not create")
			}
			if _, other, _ := projects.GetOrCreate("wiki", "o-2"); !other {
				t.Fatal("same name in a different org must create")
			}
			if err := projects.Rename(p1.ID, "handbook"); err != nil {
				t.Fatal(err)
			}
			desc, icon := "everything support needs", "book-open"
			if err := projects.Update(p1.ID, nil, &desc, &icon); err != nil {
				t.Fatal(err)
			}
			p2, _, _ := projects.GetOrCreate("scratch", "o-1")
			if err := projects.SetPerm(p2.ID, "doomed@x.io", PermAdmin); err != nil {
				t.Fatal(err)
			}
			if err := projects.Delete(p2.ID, "boss@x.io"); err != nil {
				t.Fatal(err)
			}
			// per-project permissions ride along with the project record
			if err := projects.SetCreator(p1.ID, "Boss@X.io"); err != nil {
				t.Fatal(err)
			}
			if err := projects.SetDefault(p1.ID, PermNone); err != nil {
				t.Fatal(err)
			}
			if err := projects.SetTemplate(p1.ID, "para"); err != nil {
				t.Fatal(err)
			}
			for email, level := range map[string]string{
				"boss@x.io": PermAdmin, "reader@x.io": PermRead, "cutoff@x.io": PermNone,
			} {
				if err := projects.SetPerm(p1.ID, email, level); err != nil {
					t.Fatal(err)
				}
			}

			orgs, err := NewOrgDB(st.Orgs())
			if err != nil {
				t.Fatal(err)
			}
			org, err := orgs.Create("Acme", "boss@x.io")
			if err != nil {
				t.Fatal(err)
			}
			if err := orgs.AddMember(org.ID, "worker@x.io", RoleMember); err != nil {
				t.Fatal(err)
			}
			if err := orgs.SetRole(org.ID, "worker@x.io", RoleOwner); err != nil {
				t.Fatal(err)
			}
			inv, err := orgs.CreateInvite(org.ID, "boss@x.io", time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			orgs.RecordInviteUse(inv.Token)
			if !orgs.ValidInvite(inv.Token) {
				t.Fatal("fresh invite should be valid")
			}

			shares, err := NewShareDB(st.Shares())
			if err != nil {
				t.Fatal(err)
			}
			live, err := shares.Create(p1.ID, "handbook.md", "boss@x.io", 0)
			if err != nil {
				t.Fatal(err)
			}
			gone, _ := shares.Create(p1.ID, "temp.md", "boss@x.io", time.Hour)
			if !shares.Revoke(gone.Token) {
				t.Fatal("revoke should succeed")
			}
			dated, _ := shares.Create(p1.ID, "deck.md", "boss@x.io", 0)
			if _, ok, err := shares.SetExpiry(dated.Token, time.Hour); err != nil || !ok {
				t.Fatalf("set expiry: %v %v", ok, err)
			}

			devices, err := NewDeviceRegistry(st.Devices())
			if err != nil {
				t.Fatal(err)
			}
			devices.Observe(DeviceInfo{ID: "d1", Name: "laptop", OS: "mac", User: "dev@x.io", IP: "1.2.3.4"})

			reads, err := NewReadLedger(st.Reads(), 0)
			if err != nil {
				t.Fatal(err)
			}
			reads.Record(p1.ID, "handbook.md", ReadKindHuman, "dev@x.io")
			reads.Record(p1.ID, "handbook.md", ReadKindHuman, "boss@x.io")
			reads.Record(p1.ID, "wiki/deep.md", ReadKindAgent, "d1")
			// Per-session read detail rides its own repo, so it needs its own
			// pass on every backend: two sessions on one device must stay two
			// sets of rows, and one of them must prune away by date.
			reads.WithSessions(st.SessionReads(), 0)
			reads.RecordSession(p1.ID, "sess-a", "d1", "handbook.md")
			reads.RecordSession(p1.ID, "sess-a", "d1", "wiki/deep.md")
			reads.RecordSession(p1.ID, "sess-b", "d1", "handbook.md")
			if err := reads.Close(); err != nil {
				t.Fatal(err)
			}
			if err := st.SessionReads().PutBatch([]SessionRead{{
				Project: p1.ID, Session: "sess-old", Device: "d1", Path: "handbook.md",
				Last: time.Now().UTC().Add(-90 * 24 * time.Hour),
			}}); err != nil {
				t.Fatal(err)
			}

			if err := st.Close(); err != nil {
				t.Fatal(err)
			}

			// ---- reopen and verify everything persisted ----
			st2 := be.open(t)
			defer st2.Close()

			auth2, _ := NewBuiltinAuth(st2.Accounts(), true, nil)
			if _, ok := auth2.userForToken(tok); !ok {
				t.Fatal("token lost across reload")
			}
			if !auth2.RequireApproval || auth2.RequireVerification {
				t.Fatal("policy lost across reload")
			}
			if got := auth2.PendingUsers(); len(got) != 1 || got[0].Email != "pending@x.io" {
				t.Fatalf("pending users after reload = %+v", got)
			}
			if err := auth2.Approve(pend.ID); err != nil {
				t.Fatal(err)
			}
			if len(auth2.PendingUsers()) != 0 {
				t.Fatal("approve did not clear pending")
			}

			projects2, _ := NewProjectDB(st2.Projects())
			list := projects2.List()
			if len(list) != 2 { // handbook (o-1), wiki (o-2); scratch was deleted
				t.Fatalf("projects after reload = %+v", list)
			}
			hb, ok := projects2.Get(p1.ID)
			if !ok || hb.Name != "handbook" {
				t.Fatalf("rename lost across reload: %+v", hb)
			}
			// Description/icon and creator/default_level are all columns
			// migrate() has to ADD to an already-created projects table; the
			// reopen above already ran migrate() a second time, so surviving
			// here proves it's a no-op.
			if hb.Description != "everything support needs" || hb.Icon != "book-open" {
				t.Fatalf("description/icon lost across reload: %+v", hb)
			}
			if hb.Creator != "boss@x.io" || hb.Default != PermNone {
				t.Fatalf("creator/default lost across reload: %+v", hb)
			}
			if hb.Template != "para" {
				t.Fatalf("template lost across reload: %+v", hb)
			}
			if hb.Perms["boss@x.io"] != PermAdmin || hb.Perms["reader@x.io"] != PermRead ||
				hb.Perms["cutoff@x.io"] != PermNone || len(hb.Perms) != 3 {
				t.Fatalf("grants lost across reload: %+v", hb.Perms)
			}
			if _, ok := projects2.Get(p2.ID); ok {
				t.Fatal("deleted project (and its grants) came back after reload")
			}
			// The tombstone itself survives the reload: the audit record of
			// who deleted what, when.
			ts, ok := projects2.GetDeleted(p2.ID)
			if !ok || ts.DeletedBy != "boss@x.io" || ts.Deleted.IsZero() {
				t.Fatalf("tombstone lost across reload: %+v (ok=%v)", ts, ok)
			}

			orgs2, _ := NewOrgDB(st2.Orgs())
			ro, ok := orgs2.Get(org.ID)
			if !ok || ro.Members["boss@x.io"] != RoleOwner || ro.Members["worker@x.io"] != RoleOwner {
				t.Fatalf("org roles lost across reload: %+v", ro)
			}
			if !orgs2.ValidInvite(inv.Token) {
				t.Fatal("invite lost across reload")
			}
			if got := orgs2.ListInvites(org.ID); len(got) != 1 || got[0].Uses != 1 {
				t.Fatalf("invite uses after reload = %+v", got)
			}

			shares2, _ := NewShareDB(st2.Shares())
			if _, ok := shares2.Get(live.Token); !ok {
				t.Fatal("live share lost across reload")
			}
			if _, ok := shares2.Get(gone.Token); ok {
				t.Fatal("revoked share came back after reload")
			}
			if got, ok := shares2.Get(dated.Token); !ok || got.Expires.IsZero() {
				t.Fatalf("patched expiry lost across reload: %+v", got)
			}

			devices2, _ := NewDeviceRegistry(st2.Devices())
			d, ok := devices2.Get("d1")
			if !ok || d.Name != "laptop" || d.IP != "1.2.3.4" {
				t.Fatalf("device lost across reload: %+v", d)
			}

			reads2, err := NewReadLedger(st2.Reads(), 0)
			if err != nil {
				t.Fatal(err)
			}
			heat := reads2.Heat(p1.ID, "", time.Time{})
			if e := heat["handbook.md"]; e.Human != 2 || e.Readers != 2 {
				t.Fatalf("read buckets lost across reload: %+v", e)
			}
			if e := heat["wiki/deep.md"]; e.Agent != 1 || e.Readers != 0 {
				t.Fatalf("agent read bucket lost across reload: %+v", e)
			}
			if sub := reads2.Heat(p1.ID, "wiki", time.Time{}); len(sub) != 1 {
				t.Fatalf("prefix heat = %+v, want only wiki/deep.md", sub)
			}

			sessions := st2.SessionReads()
			got, err := sessions.ListBySession(p1.ID, "sess-a", "d1")
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 2 || got[0].Path != "handbook.md" || got[1].Path != "wiki/deep.md" {
				t.Fatalf("session-a rows lost across reload: %+v", got)
			}
			if other, _ := sessions.ListBySession(p1.ID, "sess-b", "d1"); len(other) != 1 {
				t.Fatalf("session-b rows = %+v, want its own single row", other)
			}
			// Wrong device, same session id: the query is keyed on both, which
			// is what keeps a forged report off somebody else's run card.
			if none, _ := sessions.ListBySession(p1.ID, "sess-a", "d2"); len(none) != 0 {
				t.Fatalf("session rows leaked across devices: %+v", none)
			}
			if err := sessions.PruneBefore(time.Now().UTC().AddDate(0, 0, -30)); err != nil {
				t.Fatal(err)
			}
			if old, _ := sessions.ListBySession(p1.ID, "sess-old", "d1"); len(old) != 0 {
				t.Fatalf("prune left expired session rows: %+v", old)
			}
			if kept, _ := sessions.ListBySession(p1.ID, "sess-a", "d1"); len(kept) != 2 {
				t.Fatalf("prune took recent session rows too: %+v", kept)
			}
			// The aggregate the run cards do NOT come from is untouched by any
			// of that — session rows never enter the bucket map.
			if e := reads2.Heat(p1.ID, "", time.Time{})["handbook.md"]; e.Human != 2 {
				t.Fatalf("bucket heat changed with session rows: %+v", e)
			}
		})
	}
}

// migrate() only ever created tables, so the columns permissions added need a
// real ALTER on a hub that is already running. Prove both halves: an old
// projects table gains them (with its rows intact), and migrating again is a
// no-op rather than an error.
func TestSQLMigrateAddsPermissionColumns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	// the pre-permissions schema, verbatim
	if _, err := old.Exec(`CREATE TABLE projects (
		id TEXT PRIMARY KEY, name TEXT NOT NULL, org TEXT NOT NULL DEFAULT '',
		created TEXT NOT NULL DEFAULT '')`); err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(`INSERT INTO projects (id,name,org,created) VALUES ('p-0000abcd','wiki','o-1','')`); err != nil {
		t.Fatal(err)
	}
	old.Close()

	for i := 0; i < 2; i++ { // opening twice re-runs migrate()
		st, err := OpenSQLStore("sqlite", path)
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		projects, err := NewProjectDB(st.Projects())
		if err != nil {
			t.Fatalf("load %d: %v", i, err)
		}
		p, ok := projects.Get("p-0000abcd")
		if !ok || p.Name != "wiki" {
			t.Fatalf("pre-existing row lost on upgrade: %+v", p)
		}
		// An upgraded row has no creator and an empty default, which reads as
		// write — the whole "no behavior change on upgrade" promise.
		if p.Creator != "" || p.Default != "" || p.level() != PermWrite {
			t.Fatalf("upgraded row = %+v, want empty creator/default reading as write", p)
		}
		if i == 0 {
			if err := projects.SetPerm(p.ID, "a@x.io", PermRead); err != nil {
				t.Fatal(err)
			}
		} else if p.Perms["a@x.io"] != PermRead {
			t.Fatalf("grant lost across reopen: %+v", p.Perms)
		}
		st.Close()
	}
}
