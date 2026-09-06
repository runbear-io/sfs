package webapp

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// MetaStore is the hub's metadata persistence, split into one typed repository
// per entity. It holds ONLY the control plane — accounts, tokens, projects,
// orgs, invites, shares, devices. File content and the append-only journals
// live in the object store and never touch this; ephemeral state (one-time
// login and device codes, rate-limit buckets) stays in memory.
//
// A deployment chooses the backend: `file` (JSON on disk, the zero-dependency
// default) or `sql` (SQLite locally, Postgres/Supabase in production). The
// service structs (BuiltinAuth, OrgDB, …) keep their in-memory maps, mutexes,
// and business logic and persist each change through these repos — so reads
// stay in memory and writes are a single record apiece, which every backend
// implements as one real row.
type MetaStore interface {
	Accounts() AccountRepo
	Projects() ProjectRepo
	Orgs() OrgRepo
	Shares() ShareRepo
	Devices() DeviceRepo
	Reads() ReadRepo
	SessionReads() SessionReadRepo
	Close() error
}

// AccountRepo persists accounts, device tokens, and the (singleton) signup
// policy. Load returns everything at open; every other method is one record.
type AccountRepo interface {
	Load() (users []*authUser, tokens []authToken, policy *authPolicy, err error)
	PutAccount(u *authUser) error
	DeleteAccount(id string) error
	PutToken(t authToken) error
	DeleteToken(hash string) error
	PutPolicy(p authPolicy) error
}

type ProjectRepo interface {
	Load() ([]Project, error)
	Put(p Project) error
	Delete(id string) error
}

type OrgRepo interface {
	Load() (orgs []Org, invites []OrgInvite, err error)
	PutOrg(o Org) error
	DeleteOrg(id string) error
	PutInvite(i OrgInvite) error
	DeleteInvite(token string) error
}

type ShareRepo interface {
	Load() ([]Share, error)
	Put(s Share) error
	Delete(token string) error
}

type DeviceRepo interface {
	Load() ([]DeviceInfo, error)
	Put(d DeviceInfo) error
	// Delete removes one account's row for one device id. Rows are keyed by
	// (account, id), and this is how an offboarded account's hub-wide claim on
	// a machine is released — see DeviceRegistry.Release.
	Delete(user, id string) error
}

// ReadRepo persists read-telemetry buckets (see ReadStat). Unlike the other
// repos it is batch-oriented: reads are telemetry, and the ledger flushes many
// dirty buckets at once — one file rewrite / one SQL transaction per flush,
// not one write per bucket.
type ReadRepo interface {
	Load() ([]ReadStat, error)
	PutBatch(stats []ReadStat) error // upsert by (project, path, day, kind, actor)
	DeleteBatch(keys []ReadStatKey) error
}

// SessionReadRepo persists which paths one agent session read (see
// SessionRead). Deliberately its OWN repo rather than a session column on
// read_stats: ReadLedger loads every read_stats row into one map at boot and
// ReadLedger.Heat linearly scans that whole map on every heat request,
// hub-wide — so multiplying its row count by session cardinality would slow
// the Dashboard for projects that never ran an agent. These rows never enter
// that map; they are queried by primary key and pruned by date.
type SessionReadRepo interface {
	PutBatch(reads []SessionRead) error // upsert by (project, session, device, path)
	ListBySession(project, session, device string) ([]SessionRead, error)
	PruneBefore(t time.Time) error
}

// ---- cheap change detection ---------------------------------------------

// Versioned is the optional "has anything moved?" check on a repository: a
// token that changes whenever anything the repo stores changes.
//
// Every registry re-reads its whole store on every authorization read (see
// ProjectDB.refresh) — that is a correctness floor, not a cache, and it stays.
// This is the same read made cheap: one os.Stat, or one primary-key lookup,
// instead of a full JSON parse or nine unfiltered SELECTs on every
// authenticated request. It is NOT a TTL: a token that moved is always
// followed by the full re-read, so the staleness window rounds 12-14 closed
// stays closed.
//
// A repo that cannot answer — no implementation, or an error — is treated as
// changed, so the fallback is exactly the unconditional re-read that was
// always there. An implementation must never return an empty token.
type Versioned interface {
	Version() (string, error)
}

// versionGate is the per-registry half of that check. It remembers the token
// of the last SUCCESSFUL load, so a load that failed never leaves the registry
// marked fresh. Callers hold the registry's own mutex — refresh() already does.
type versionGate struct {
	token string
	valid bool
}

// stale reports whether repo may have changed since the last successful load,
// and returns the token to record once that load succeeds.
func (g *versionGate) stale(repo any) (token string, stale bool) {
	v, ok := repo.(Versioned)
	if !ok {
		return "", true
	}
	cur, err := v.Version()
	if err != nil || cur == "" {
		return "", true // can't tell → re-read, and record nothing
	}
	if g.valid && cur == g.token {
		return cur, false
	}
	return cur, true
}

// fresh records the token of a load that succeeded.
func (g *versionGate) fresh(token string) {
	if token != "" {
		g.token, g.valid = token, true
	}
}

// ---- what a metadata store will hold ------------------------------------

// storable refuses text that no metadata backend can hold faithfully, so all
// three agree on which requests succeed.
//
// The three disagreed on eighteen inputs. A NUL cannot go in a Postgres text
// column at all (SQLSTATE 22021), while sqlite keeps it and the file backend
// keeps it. Invalid UTF-8 is worse than a disagreement on the file backend —
// the default: encoding/json substitutes U+FFFD per bad byte and reports
// SUCCESS, so the running hub and its database hold different records, nothing
// is logged, and two inputs that differ in memory fold onto one key on disk.
//
// Refusing is the decision rather than storing verbatim, for two reasons. It
// is what the doors already enforce (printableOnly, hasControlChars,
// journal.SafePath), so there is one rule instead of three. And it means a hub
// cannot change what it accepts by changing its database — the property that
// let row 14 look clean for seven rounds while the backends diverged.
//
// It replaces the assertion in the retired TestSec_DB_NULBytesDoNotTruncateRecords
// ("a NUL must round-trip verbatim"), which Postgres cannot implement without
// moving this whole layer to bytea. See TestSec_DB_EveryBackendAgreesWhichTextIsStorable.
func storable(vals ...string) error {
	for _, v := range vals {
		if strings.IndexByte(v, 0) >= 0 {
			return fmt.Errorf("metadata text may not contain a NUL byte: %q", v)
		}
		if !utf8.ValidString(v) {
			return fmt.Errorf("metadata text must be valid UTF-8: %q", v)
		}
	}
	return nil
}

// storableMap checks a map's keys and values — the grant and membership maps,
// where the KEY is the account an authorization decision keys on.
func storableMap(m map[string]string) error {
	for k, v := range m {
		if err := storable(k, v); err != nil {
			return err
		}
	}
	return nil
}

func checkAccount(u *authUser) error {
	return storable(u.ID, u.Email, u.Name, u.Pass, u.Status)
}

func checkToken(t authToken) error { return storable(t.Hash, t.User, t.Device) }

func checkProject(p Project) error {
	if err := storable(p.ID, p.Name, p.Org, p.Description, p.Icon,
		p.Creator, p.Template, p.Default, p.DeletedBy); err != nil {
		return err
	}
	return storableMap(p.Perms)
}

func checkOrg(o Org) error {
	if err := storable(o.ID, o.Name); err != nil {
		return err
	}
	return storableMap(o.Members)
}

func checkInvite(i OrgInvite) error { return storable(i.Token, i.Org, i.Creator) }

func checkShare(s Share) error { return storable(s.Token, s.Project, s.Path, s.Creator) }

func checkDevice(d DeviceInfo) error {
	return storable(d.ID, d.Name, d.OS, d.User, d.IP)
}

func checkReadStat(s ReadStat) error {
	return storable(s.Project, s.Path, s.Day, s.Kind, s.Actor)
}

func checkSessionRead(s SessionRead) error {
	return storable(s.Project, s.Session, s.Path, s.Device)
}
