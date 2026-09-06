package webapp

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// Organizations group accounts and own projects: every project belongs to
// exactly one org, and only that org's members can see or sync it. The OSS
// server supports any number of orgs on one hub (a self-hosted deployment is
// typically just one); membership is by account email with two roles. Share
// links (/s/) deliberately stay outside this wall — public is their point.
//
// Same file-backed discipline as the other registries: orgs.json is loaded
// at open and rewritten atomically (temp + rename) on every change.

const (
	RoleOwner  = "owner"
	RoleMember = "member"
)

// Org is one organization.
type Org struct {
	ID      string            `json:"id"`
	Name    string            `json:"name"`
	Members map[string]string `json:"members"` // lowercase email → role
	Created time.Time         `json:"created"`
	// Joined records when each member row was first created. It exists for one
	// decision — who inherits an org whose sole owner is offboarded — and that
	// decision must not be readable off the address a member typed at signup.
	// Rows written before this field existed carry a zero time and lose to any
	// dated row, which is right: they are the oldest members there are.
	Joined map[string]time.Time `json:"joined,omitempty"`
}

// OrgInvite is a mint-once join link. Redeeming it while signed in adds the
// account to the org as a member.
type OrgInvite struct {
	Token   string    `json:"token"`
	Org     string    `json:"org"`
	Creator string    `json:"creator,omitempty"` // account email
	Created time.Time `json:"created"`
	Expires time.Time `json:"expires"`
	Uses    int       `json:"uses"` // how many accounts have joined via this link
}

// RecordInviteUse bumps the join counter for an invite (best effort).
func (db *OrgDB) RecordInviteUse(token string) {
	db.mu.Lock()
	defer db.mu.Unlock()
	db.refresh()
	if inv, ok := db.invites[token]; ok {
		inv.Uses++
		db.invites[token] = inv
		db.repo.PutInvite(inv)
	}
}

func (i OrgInvite) expired() bool { return time.Now().After(i.Expires) }

// DefaultInviteTTL bounds invite links that don't ask for an expiry.
const DefaultInviteTTL = 7 * 24 * time.Hour

// OrgDB is the in-memory org registry over a MetaStore OrgRepo (orgs + invites).
type OrgDB struct {
	repo OrgRepo

	mu      sync.Mutex
	ver     versionGate // skips the re-read when the store has not moved
	warned  bool        // "re-read failed" logged once (see refresh)
	byID    map[string]Org
	invites map[string]OrgInvite
	// seniority, when set, lists accounts oldest first. It resolves the ONE
	// decision the org rows themselves cannot: which member inherits an org
	// when every Joined time ties, which is the state of every member row
	// written before Joined existed — i.e. of the hubs that have been running
	// longest. See heir.
	seniority func() []string
}

// SetSeniority installs the oldest-first account order (AuthProvider.Accounts).
// It is set per call rather than at construction because a directory can be
// rebuilt from its repo at any time, and a tie-break that silently reverts to
// the address a member typed is round 8's escalation back.
func (db *OrgDB) SetSeniority(f func() []string) {
	db.mu.Lock()
	defer db.mu.Unlock()
	db.seniority = f
}

// NewOrgDB builds the registry over a repo, loading orgs and invites.
func NewOrgDB(repo OrgRepo) (*OrgDB, error) {
	db := &OrgDB{repo: repo, byID: make(map[string]Org), invites: make(map[string]OrgInvite)}
	orgs, invites, err := repo.Load()
	if err != nil {
		return nil, err
	}
	for _, o := range orgs {
		db.byID[o.ID] = o
	}
	for _, i := range invites {
		db.invites[i.Token] = i
	}
	return db, nil
}

// OpenOrgDB loads the file-backed registry at path.
func OpenOrgDB(path string) (*OrgDB, error) {
	return NewOrgDB(newFileOrgRepo(path))
}

func normEmail(email string) string { return strings.ToLower(strings.TrimSpace(email)) }

// clone copies an Org so its Members map never leaves the registry: Org is a
// value but the map inside it is a pointer, and a caller holding the live map
// can write roles straight past SetRole, the last-owner guard and the store —
// or race a concurrent mutator while handleOrgList ranges over it.
func (o Org) clone() Org {
	c := o
	c.Members = make(map[string]string, len(o.Members))
	for k, v := range o.Members {
		c.Members[k] = v
	}
	c.Joined = make(map[string]time.Time, len(o.Joined))
	for k, v := range o.Joined {
		c.Joined[k] = v
	}
	return c
}

// rowScopedOrgRepo is the part of an OrgRepo that can write an org's own
// metadata and its individual MEMBERSHIP rows as separate records.
//
// Same seam, same reason, same shape as rowScopedProjectRepo (see its comment):
// a whole-record write replaces the entire member set from the writer's
// in-memory copy — fileOrgRepo rewrote orgs.json from a map loaded at open, and
// sqlOrgRepo DELETEd every org_members row for the org and re-inserted from the
// same stale map — so on a hub running two processes in front of one database
// (the entire reason to configure Postgres) any unrelated write by the second
// one resurrected every membership it had not seen. On orgs that is the outer
// authorization wall, not a per-project grant.
//
// Optional rather than part of OrgRepo so a repo that cannot do it — or a test
// wrapper that intercepts PutOrg — keeps the old whole-record path.
type rowScopedOrgRepo interface {
	// PutOrgMeta writes an org's own fields and leaves its members alone.
	PutOrgMeta(o Org) error
	// PutMember writes one membership row. An empty role deletes it.
	PutMember(org, email, role string, joined time.Time) error
}

// putOrg persists a mutated org's own METADATA, restoring the previous value if
// the store refuses. Callers hold mu. Without the rollback a refused write still
// reads as applied until the hub restarts, when it silently reverts — a demotion
// that un-demotes itself.
func (db *OrgDB) putOrg(prev, o Org) error {
	db.byID[o.ID] = o
	var err error
	if rs, ok := db.repo.(rowScopedOrgRepo); ok {
		err = rs.PutOrgMeta(o)
	} else {
		err = db.repo.PutOrg(o)
	}
	if err != nil {
		db.byID[o.ID] = prev
		return err
	}
	return nil
}

// putMember persists ONE membership change, with the same rollback. role == ""
// means the row is being removed.
func (db *OrgDB) putMember(prev, next Org, email, role string) error {
	db.byID[next.ID] = next
	var err error
	if rs, ok := db.repo.(rowScopedOrgRepo); ok {
		err = rs.PutMember(next.ID, email, role, next.Joined[email])
	} else {
		err = db.repo.PutOrg(next)
	}
	if err != nil {
		db.byID[next.ID] = prev
		return err
	}
	return nil
}

// refresh re-reads orgs and invites from the store. Callers hold mu.
//
// Round 12 gave ProjectDB this on its read path and stopped there. The org
// registry is the wall IN FRONT of project permissions — projectPerm resolves
// s.Projects.Get() (refreshed) and then s.Dir.Role(p.Org, email) out of a map
// loaded at boot — so on a hub running two processes an owner's "remove this
// person from the org" took effect on whichever process served it and on no
// other, and the removed member kept reading every project in the org. Same for
// a revoked invite, which on the default invite-only posture also bootstraps an
// account.
//
// It runs at the top of the MUTATORS too, not only the reads: the last-owner
// guard counts owners out of this map, so a stale copy lets two processes each
// demote "the other" owner and leave an org nobody can administer. The
// write-side reload in the repo cannot close that — the decision is made up
// here, before the repo is ever called.
//
// A store that cannot answer leaves the current maps in place: the previous
// answer is the last one the store agreed to, and dropping every org on a
// transient read error would 403 the whole hub. Same trade, same reasons as
// ProjectDB.refresh.
//
// Gated on the store's change token (Versioned) like ProjectDB.refresh: still
// a re-read on every authorization read, but only when something moved.
func (db *OrgDB) refresh() {
	token, stale := db.ver.stale(db.repo)
	if !stale {
		return
	}
	orgs, invites, err := db.repo.Load()
	if err != nil {
		if !db.warned {
			db.warned = true
			log.Printf("beardrive: org registry re-read failed, serving the last known memberships: %v", err)
		}
		return
	}
	db.warned = false
	db.ver.fresh(token)
	nextOrgs := make(map[string]Org, len(orgs))
	for _, o := range orgs {
		nextOrgs[o.ID] = o
	}
	nextInvites := make(map[string]OrgInvite, len(invites))
	for _, i := range invites {
		nextInvites[i.Token] = i
	}
	db.byID, db.invites = nextOrgs, nextInvites
}

// Create makes a new org owned by ownerEmail.
func (db *OrgDB) Create(name, ownerEmail string) (Org, error) {
	name = trimName(name)
	if name == "" {
		return Org{}, fmt.Errorf("organization name must not be empty")
	}
	o := Org{
		ID: "o-" + randHex(4), Name: name,
		Members: map[string]string{}, Joined: map[string]time.Time{},
		Created: time.Now().UTC(),
	}
	if e := normEmail(ownerEmail); e != "" {
		o.Members[e] = RoleOwner
		o.Joined[e] = o.Created
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	db.refresh()
	db.byID[o.ID] = o
	if err := db.repo.PutOrg(o); err != nil {
		delete(db.byID, o.ID)
		return Org{}, err
	}
	return o.clone(), nil
}

func (db *OrgDB) Get(id string) (Org, bool) {
	db.mu.Lock()
	defer db.mu.Unlock()
	db.refresh()
	o, ok := db.byID[id]
	if !ok {
		return Org{}, false
	}
	return o.clone(), true
}

// Role returns the account's role in the org, or "" for non-members.
func (db *OrgDB) Role(orgID, email string) string {
	db.mu.Lock()
	defer db.mu.Unlock()
	db.refresh()
	o, ok := db.byID[orgID]
	if !ok {
		return ""
	}
	return o.Members[normEmail(email)]
}

// OrgsFor returns the orgs the account belongs to, sorted by name.
func (db *OrgDB) OrgsFor(email string) []Org {
	e := normEmail(email)
	db.mu.Lock()
	defer db.mu.Unlock()
	db.refresh()
	var out []Org
	for _, o := range db.byID {
		if o.Members[e] != "" {
			out = append(out, o.clone())
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// AddMember adds (or keeps) the account in the org with the given role. An
// existing member's role is never downgraded by an invite.
func (db *OrgDB) AddMember(orgID, email, role string) error {
	e := normEmail(email)
	if e == "" {
		return fmt.Errorf("email must not be empty")
	}
	if role != RoleOwner && role != RoleMember {
		return fmt.Errorf("invalid role %q", role)
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	db.refresh()
	o, ok := db.byID[orgID]
	if !ok {
		return fmt.Errorf("no such organization")
	}
	if o.Members[e] == RoleOwner {
		return nil
	}
	next := o.clone()
	next.Members[e] = role
	if _, dated := next.Joined[e]; !dated {
		next.Joined[e] = time.Now().UTC()
	}
	return db.putMember(o, next, e, role)
}

// RemoveMember drops an account from the org. The last owner cannot be
// removed (an org must always have someone who can administer it).
func (db *OrgDB) RemoveMember(orgID, email string) error {
	e := normEmail(email)
	db.mu.Lock()
	defer db.mu.Unlock()
	db.refresh()
	o, ok := db.byID[orgID]
	if !ok {
		return fmt.Errorf("no such organization")
	}
	if o.Members[e] == RoleOwner && db.ownerCount(o) <= 1 {
		return fmt.Errorf("cannot remove the last owner")
	}
	return db.removeLocked(o, e)
}

// EvictMember drops an account from the org unconditionally — the form
// offboard needs when the ACCOUNT itself is gone. The last-owner rule keeps a
// LIVE org administrable; it must never preserve an ownership row for an
// address nobody can sign in as, because the next signup on that address
// inherits it — org ownership, and through it admin on every project in the
// org. An org left with no owner is a recovery problem, not an authorization
// one.
// Dropping the sole owner is where this differs from RemoveMember in the other
// direction too: every org route is gated on RoleOwner and nothing adopts an
// ownerless org, so an org left with members and no owner can never again gain
// one, lose one, or change a role. The longest-standing remaining member is
// promoted instead — by Org.Joined, never by the address string. Round 8's
// heir was `lowestMember`, the smallest address, so the successor to every org
// was decided by what a member typed at signup: someone who joined last through
// an ordinary invite, holding no grant on anything, inherited org ownership and
// with it admin on every project in the org — triggered by the most routine
// operator action there is.
func (db *OrgDB) EvictMember(orgID, email string) error {
	e := normEmail(email)
	db.mu.Lock()
	defer db.mu.Unlock()
	db.refresh()
	o, ok := db.byID[orgID]
	if !ok {
		return fmt.Errorf("no such organization")
	}
	if o.Members[e] == "" {
		return nil // already not a member: the postcondition already holds
	}
	next := o.clone()
	delete(next.Members, e)
	delete(next.Joined, e)
	if err := db.putMember(o, next, e, ""); err != nil {
		return err
	}
	if o.Members[e] == RoleOwner && db.ownerCount(next) == 0 {
		if heir := db.heir(next); heir != "" {
			promoted := next.clone()
			promoted.Members[heir] = RoleOwner
			// The heir's ownership starts NOW. Any invite they minted under an
			// earlier ownership was retired when they lost it (liveLocked), and
			// this promotion must not resurrect it: read-time role resolution
			// alone would, since the creator reads as an owner again. This is
			// the one place a role is granted by the hub rather than by an
			// owner's deliberate act, so it is the one place that has to say so.
			for tok, inv := range db.invites {
				if inv.Org == orgID && inv.Creator == heir {
					db.retireLocked(tok)
				}
			}
			return db.putMember(next, promoted, heir, RoleOwner)
		}
	}
	return nil
}

// heir is the member who inherits an org whose last owner is gone: the one who
// has been in it longest, by the join time on the row.
//
// The tie is the whole finding. Every member row written before Joined existed
// carries the zero time, so on an upgraded hub — file, sqlite and postgres
// alike — EVERY member ties and the old fallback ("lowest address") was the
// rule again, unchanged from round 8: a newcomer who picked an address
// inherited the org and project-admin on everything in it, triggered by the
// most routine operator action there is.
//
// Ties resolve on account seniority (AuthProvider.Accounts, oldest first —
// already the org migration's rule for picking the default org's owner). With
// no seniority available there is NO evidence at all, and the answer is nobody:
// an ownerless org is a repair a hub admin makes deliberately, while an
// arbitrary heir is a privilege grant nobody asked for.
//
// Callers hold db.mu.
func (db *OrgDB) heir(o Org) string {
	heir, when := "", time.Time{}
	tied := false
	for m := range o.Members {
		t := o.Joined[m]
		switch {
		case heir == "":
			heir, when, tied = m, t, false
		case t.Before(when):
			heir, when, tied = m, t, false
		case t.Equal(when):
			tied = true
		}
	}
	if !tied || db.seniority == nil {
		if tied {
			return ""
		}
		return heir
	}
	for _, m := range db.seniority() {
		e := normEmail(m)
		if o.Members[e] != "" && o.Joined[e].Equal(when) {
			return e
		}
	}
	return ""
}

// removeLocked deletes a member row. Callers hold mu.
func (db *OrgDB) removeLocked(o Org, e string) error {
	if o.Members[e] == "" {
		return nil // already not a member: the postcondition already holds
	}
	next := o.clone()
	delete(next.Members, e)
	delete(next.Joined, e)
	return db.putMember(o, next, e, "")
}

// SetRole changes an account's role. Demoting the last owner is refused.
func (db *OrgDB) SetRole(orgID, email, role string) error {
	if role != RoleOwner && role != RoleMember {
		return fmt.Errorf("invalid role %q", role)
	}
	e := normEmail(email)
	db.mu.Lock()
	defer db.mu.Unlock()
	db.refresh()
	o, ok := db.byID[orgID]
	if !ok {
		return fmt.Errorf("no such organization")
	}
	if o.Members[e] == "" {
		return fmt.Errorf("%s is not a member", email)
	}
	if o.Members[e] == RoleOwner && role == RoleMember && db.ownerCount(o) <= 1 {
		return fmt.Errorf("cannot demote the last owner")
	}
	next := o.clone()
	next.Members[e] = role
	return db.putMember(o, next, e, role)
}

// Rename changes the org's display name.
func (db *OrgDB) Rename(orgID, name string) error {
	name = trimName(name)
	if name == "" {
		return fmt.Errorf("organization name must not be empty")
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	db.refresh()
	o, ok := db.byID[orgID]
	if !ok {
		return fmt.Errorf("no such organization")
	}
	next := o
	next.Name = name
	return db.putOrg(o, next)
}

// Delete removes the org and its invites. The org row goes first — an org
// the store refuses to drop stays whole, invites and all; an invite the
// store then refuses to drop is already dead, because every membership check
// walks through the (gone) org row.
func (db *OrgDB) Delete(orgID string) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	db.refresh()
	if _, ok := db.byID[orgID]; !ok {
		return fmt.Errorf("no such organization")
	}
	if err := db.repo.DeleteOrg(orgID); err != nil {
		return err
	}
	delete(db.byID, orgID)
	for token, inv := range db.invites {
		if inv.Org == orgID {
			db.retireLocked(token)
		}
	}
	return nil
}

// ownerCount counts owners in an org. Callers hold mu.
func (db *OrgDB) ownerCount(o Org) int {
	n := 0
	for _, role := range o.Members {
		if role == RoleOwner {
			n++
		}
	}
	return n
}

// ListInvites returns the org's live (non-expired) invites.
func (db *OrgDB) ListInvites(orgID string) []OrgInvite {
	db.mu.Lock()
	defer db.mu.Unlock()
	db.refresh()
	var out []OrgInvite
	for _, inv := range db.invites {
		if inv.Org == orgID && db.liveLocked(inv) {
			out = append(out, inv)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.After(out[j].Created) })
	return out
}

// RevokeInvite deletes an invite so its link stops working immediately.
func (db *OrgDB) RevokeInvite(token string) bool {
	db.mu.Lock()
	defer db.mu.Unlock()
	db.refresh()
	inv, ok := db.invites[token]
	if !ok {
		return false
	}
	delete(db.invites, token)
	if err := db.repo.DeleteInvite(token); err != nil {
		// Revocation is the emergency stop for a leaked join link, and on an
		// invite-only hub that link bootstraps accounts. A delete the store
		// refused would come back at the next restart, so put it back and
		// report the failure instead of reporting a revocation that isn't one.
		db.invites[token] = inv
		return false
	}
	return true
}

// CreateInvite mints a join link for the org.
func (db *OrgDB) CreateInvite(orgID, creator string, ttl time.Duration) (OrgInvite, error) {
	if ttl <= 0 {
		ttl = DefaultInviteTTL
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	db.refresh()
	if _, ok := db.byID[orgID]; !ok {
		return OrgInvite{}, fmt.Errorf("no such organization")
	}
	inv := OrgInvite{
		Token: randHex(16), Org: orgID, Creator: normEmail(creator),
		Created: time.Now().UTC(), Expires: time.Now().UTC().Add(ttl),
	}
	db.invites[inv.Token] = inv
	if err := db.repo.PutInvite(inv); err != nil {
		delete(db.invites, inv.Token)
		return OrgInvite{}, err
	}
	return inv, nil
}

// Redeem consumes nothing — an invite link can onboard a whole team until it
// expires — it just resolves the token to its live invite.
func (db *OrgDB) Redeem(token string) (OrgInvite, bool) {
	db.mu.Lock()
	defer db.mu.Unlock()
	db.refresh()
	inv, ok := db.invites[token]
	if !ok || !db.liveLocked(inv) {
		return OrgInvite{}, false
	}
	return inv, true
}

// ValidInvite reports whether a token is a live invite, without consuming it.
// It lets the signup page permit account creation from an invite link even
// when public self-signup is closed (invite-only hubs).
func (db *OrgDB) ValidInvite(token string) bool {
	db.mu.Lock()
	defer db.mu.Unlock()
	db.refresh()
	inv, ok := db.invites[token]
	return ok && db.liveLocked(inv)
}

// liveLocked reports whether an invite is still redeemable, and RETIRES it when
// it is not. Callers hold mu.
//
// An invite is the strongest grant this hub hands out: redeeming one is org
// membership, which is read access to every project in the org, and on the
// default invite-only posture it also bootstraps the ACCOUNT that will hold it.
// Four rounds each found a different grant outliving the revocation that should
// have killed it; this one outlived all three at once — the membership, the
// ownership, and the account of the owner who minted it.
//
// The rule is the one shares.go already applies to a share link
// (shareCreatorStillBelongs): resolve the MINTER's standing at read time, not
// at mint time. Only an org owner may mint an invite (handleInviteCreate), so
// an invite whose creator is no longer an owner of that org is a capability its
// holder can no longer exercise, and neither can the link they left behind.
// Read-time, not a sweep in RemoveMember and SetRole and offboard: one rule and
// no path to forget, where a sweep is three places that each have to remember.
//
// Retiring rather than merely refusing is the latch, and it is load-bearing:
// EvictMember promotes an heir when the last owner's ACCOUNT is deleted, so a
// creator who was demoted and later inherits the org would otherwise see every
// invite they minted come back to life. Death is recorded, not recomputed. (The
// promotion itself drops the heir's invites for the same reason — see
// EvictMember.)
//
// A creator-less invite (minted before the field existed) has no standing to
// resolve and keeps its expiry as its only bound, the same "pre-accounts link"
// pass shareCreatorStillBelongs gives.
func (db *OrgDB) liveLocked(inv OrgInvite) bool {
	if inv.expired() {
		return false
	}
	if inv.Creator == "" {
		return true
	}
	if db.byID[inv.Org].Members[inv.Creator] == RoleOwner {
		return true
	}
	db.retireLocked(inv.Token)
	return false
}

// retireLocked deletes an invite whose creator lost the ownership behind it.
// A store that refuses the delete leaves the row on disk; the in-memory drop
// still stands, and the next process to read the token retires it again.
func (db *OrgDB) retireLocked(token string) {
	delete(db.invites, token)
	if err := db.repo.DeleteInvite(token); err != nil {
		log.Printf("beardrive: invite %s retired in memory but left on disk: %v", token, err)
	}
}

// ---- migration ----

// MigrateOrgs assigns every org-less project to a default org so a hub that
// predates organizations keeps working with zero manual steps. All existing
// accounts join it — they could all see every project before, so anything
// narrower would lock someone out — with the oldest account as owner.
// orgWriter is the slice of Directory that MigrateOrgs needs: it creates one
// org and fills it. Taking the narrow type keeps the sweep usable with a bare
// OrgDB (which is what the CLI has at that point) instead of forcing a
// LocalDirectory wrapper on a function that has no use for ManageURL.
type orgWriter interface {
	Create(name, ownerEmail string) (Org, error)
	AddMember(orgID, email, role string) error
}

func MigrateOrgs(projects *ProjectDB, orgs orgWriter, accounts []User) error {
	var orphans []Project
	for _, p := range projects.List() {
		if p.Org == "" {
			orphans = append(orphans, p)
		}
	}
	if len(orphans) == 0 {
		return nil
	}
	owner := ""
	if len(accounts) > 0 {
		owner = accounts[0].Email
	}
	def, err := orgs.Create("default", owner)
	if err != nil {
		return err
	}
	for _, u := range accounts[min(1, len(accounts)):] {
		if err := orgs.AddMember(def.ID, u.Email, RoleMember); err != nil {
			return err
		}
	}
	for _, p := range orphans {
		if err := projects.SetOrg(p.ID, def.ID); err != nil {
			return err
		}
	}
	return nil
}

// offboard drops every grant an address holds, at the moment its ACCOUNT goes
// away. Every authorization decision on the hub keys on the email — OrgDB.Role,
// Project.Perms, and share liveness through shareCreatorStillBelongs — and
// account removal touched none of them, so the grants outlived the account and
// the next account on that address (a re-signup, a redeemed invite, an admin
// re-adding someone) inherited them, project admin included. Round 1 ruled a
// grant must not outlive org membership; this is the same rule one level up.
//
// One choke point rather than N sweeps: it is wired into the hub's only
// account-removal path (BuiltinAuth.Deny) in Handler.
func (s *Server) offboard(email string) {
	e := normEmail(email)
	if e == "" {
		return
	}
	if s.Projects != nil {
		for _, p := range s.Projects.List() {
			if _, has := p.Perms[e]; has {
				if err := s.Projects.dropPerm(p.ID, e); err != nil {
					log.Printf("beardrive: offboard %s: project %s: %v", e, p.ID, err)
				}
			}
		}
	}
	// The device claim is hub-wide and is the WRITE gate's resolver, so it
	// outlives every org and project grant above unless it is dropped here.
	s.Devices.Release(e)
	if s.Dir != nil {
		// Last, because membership is what share liveness resolves through:
		// clearing it is what makes a removed account's public links stop
		// serving.
		//
		// "The account is gone" is authoritative here: RemoveMember refuses to
		// drop the last owner, and logging that refusal left the hub's most
		// privileged row attached to an address anyone could then sign up on.
		// Evict instead, where the directory owns its orgs.
		// Hand the directory the hub's account order before it has to pick an
		// heir: the org rows alone cannot break a Joined tie, and every row
		// written before Joined existed is one. Set here rather than at
		// startup because a directory can be rebuilt from its repo.
		//
		// Accounts() is NOT the source here. It is documented "oldest first"
		// and is now a total order, but a total order over rows that all carry
		// the zero Created stamp is deterministic noise, not evidence — and
		// heir's contract is that no evidence means no heir. seniorityLister
		// is the provider's own answer to "which accounts can I actually date",
		// and a provider that cannot answer supplies nothing, which is the
		// fail-closed direction.
		if ld, ok := s.Dir.(LocalDirectory); ok && ld.OrgDB != nil && s.Auth != nil {
			if sl, ok := s.Auth.(seniorityLister); ok {
				ld.OrgDB.SetSeniority(sl.Seniority)
			}
		}
		drop := s.Dir.RemoveMember
		if ev, ok := s.Dir.(orgEvictor); ok {
			drop = ev.EvictMember
		}
		for _, o := range s.Dir.OrgsFor(e) {
			if err := drop(o.ID, e); err != nil {
				log.Printf("beardrive: offboard %s: org %s NOT removed, the address keeps its grants: %v",
					e, o.ID, err)
			}
		}
	}
}

// orgEvictor is the part of a directory that can drop a row for an account
// that no longer exists. LocalDirectory (OrgDB) implements it; a directory
// managing its orgs elsewhere does not, and offboard reports the failure
// rather than hiding it.
type orgEvictor interface {
	EvictMember(orgID, email string) error
}

// seniorityLister is the part of an auth provider that can order accounts by
// AGE — and, crucially, say when it cannot. BuiltinAuth implements it; a
// provider that does not supplies no seniority at all, and heir then declines
// to invent one. See BuiltinAuth.Seniority.
type seniorityLister interface {
	Seniority() []string
}

// ---- HTTP ----

// orgFor resolves a project's org; zero value when orgs are off.
func (s *Server) orgOf(projectID string) string {
	if s.Projects == nil {
		return ""
	}
	p, _ := s.Projects.Get(projectID)
	return p.Org
}

// handleOrgList returns the caller's orgs with members (visible to any
// member) and the caller's role.
func (s *Server) handleOrgList(w http.ResponseWriter, r *http.Request) {
	if s.Dir == nil {
		writeJSON(w, map[string]any{"orgs": []any{}})
		return
	}
	me := s.requestUser(r)
	out := []map[string]any{}
	for _, o := range s.Dir.OrgsFor(me.Email) {
		members := make([]map[string]string, 0, len(o.Members))
		for email, role := range o.Members {
			members = append(members, map[string]string{"email": email, "role": role})
		}
		sort.Slice(members, func(i, j int) bool { return members[i]["email"] < members[j]["email"] })
		out = append(out, map[string]any{
			"id": o.ID, "name": o.Name, "role": o.Members[normEmail(me.Email)],
			"members": members, "created": o.Created,
			"manage_url": s.Dir.ManageURL(o.ID),
		})
	}
	writeJSON(w, map[string]any{"orgs": out})
}

// writeDirErr answers a failed directory write. A directory that does not own
// its organizations says so with ErrManagedElsewhere, and the answer is 409
// plus the page that does own them — the request was well-formed, it is the
// state of the world that makes it wrong. The hub never learns WHY the write
// was refused, only where to send the user.
func (s *Server) writeDirErr(w http.ResponseWriter, orgID string, err error) {
	if errors.Is(err, ErrManagedElsewhere) {
		http.Error(w, err.Error()+": "+s.Dir.ManageURL(orgID), http.StatusConflict)
		return
	}
	http.Error(w, err.Error(), http.StatusBadRequest)
}

// requireOwner returns true and the caller's email when they own the org;
// otherwise it writes the error response and returns false.
func (s *Server) requireOwner(w http.ResponseWriter, r *http.Request, orgID string) (string, bool) {
	if s.Dir == nil {
		http.Error(w, "organizations are not enabled on this server", http.StatusNotFound)
		return "", false
	}
	me := s.requestUser(r)
	if s.Dir.Role(orgID, me.Email) != RoleOwner {
		http.Error(w, "only an organization owner can do that", http.StatusForbidden)
		return "", false
	}
	return normEmail(me.Email), true
}

// handleOrgRename renames the org. Owners only.
func (s *Server) handleOrgRename(w http.ResponseWriter, r *http.Request) {
	orgID := r.PathValue("org")
	if _, ok := s.requireOwner(w, r, orgID); !ok {
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.Dir.Rename(orgID, req.Name); err != nil {
		s.writeDirErr(w, orgID, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// handleMemberUpdate changes a member's role. Owners only.
func (s *Server) handleMemberUpdate(w http.ResponseWriter, r *http.Request) {
	orgID := r.PathValue("org")
	if _, ok := s.requireOwner(w, r, orgID); !ok {
		return
	}
	var req struct {
		Role string `json:"role"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.Dir.SetRole(orgID, r.PathValue("email"), req.Role); err != nil {
		s.writeDirErr(w, orgID, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// handleMemberRemove drops a member. Owners only.
func (s *Server) handleMemberRemove(w http.ResponseWriter, r *http.Request) {
	orgID := r.PathValue("org")
	if _, ok := s.requireOwner(w, r, orgID); !ok {
		return
	}
	if err := s.Dir.RemoveMember(orgID, r.PathValue("email")); err != nil {
		s.writeDirErr(w, orgID, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// handleInviteList shows an org's live invite links. Owners only.
func (s *Server) handleInviteList(w http.ResponseWriter, r *http.Request) {
	orgID := r.PathValue("org")
	if _, ok := s.requireOwner(w, r, orgID); !ok {
		return
	}
	invs := s.Dir.ListInvites(orgID)
	out := make([]map[string]any, 0, len(invs))
	for _, inv := range invs {
		out = append(out, map[string]any{
			"token": inv.Token, "url": requestBaseURL(r) + "/join/" + inv.Token,
			"creator": inv.Creator, "created": inv.Created, "expires": inv.Expires, "uses": inv.Uses,
		})
	}
	writeJSON(w, map[string]any{"invites": out})
}

// handleInviteRevoke kills an invite link. Owners only.
func (s *Server) handleInviteRevoke(w http.ResponseWriter, r *http.Request) {
	orgID := r.PathValue("org")
	if _, ok := s.requireOwner(w, r, orgID); !ok {
		return
	}
	// Confirm the invite belongs to this org before revoking.
	inv, ok := s.Dir.Redeem(r.PathValue("token"))
	if !ok || inv.Org != orgID {
		http.Error(w, "no such invite", http.StatusNotFound)
		return
	}
	s.Dir.RevokeInvite(r.PathValue("token"))
	writeJSON(w, map[string]any{"ok": true})
}

// handleInviteCreate mints an invite link. Owners only.
func (s *Server) handleInviteCreate(w http.ResponseWriter, r *http.Request) {
	if s.Dir == nil {
		http.Error(w, "organizations are not enabled on this server", http.StatusNotFound)
		return
	}
	orgID := r.PathValue("org")
	if s.Dir.Role(orgID, s.requestUser(r).Email) != RoleOwner {
		http.Error(w, "only an organization owner can invite", http.StatusForbidden)
		return
	}
	var req struct {
		ExpiresIn string `json:"expires_in,omitempty"` // Go duration, e.g. "168h"
	}
	json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req) // body is optional
	var ttl time.Duration
	if req.ExpiresIn != "" {
		d, err := time.ParseDuration(req.ExpiresIn)
		if err != nil || d <= 0 {
			http.Error(w, "invalid expires_in", http.StatusBadRequest)
			return
		}
		ttl = d
	}
	inv, err := s.Dir.CreateInvite(orgID, s.requestUser(r).Email, ttl)
	if err != nil {
		s.writeDirErr(w, orgID, err)
		return
	}
	writeJSON(w, map[string]any{
		"token":   inv.Token,
		"url":     requestBaseURL(r) + "/join/" + inv.Token,
		"expires": inv.Expires,
	})
}

// handleInviteAccept joins the signed-in account to the invite's org.
func (s *Server) handleInviteAccept(w http.ResponseWriter, r *http.Request) {
	if s.Dir == nil {
		http.Error(w, "organizations are not enabled on this server", http.StatusNotFound)
		return
	}
	// Normalized, because every decision downstream is: AddMember, Role and
	// the grant maps all key on normEmail. Guarding on the raw string left the
	// values in between — "   ", "\t" — running Redeem and the seat check
	// before AddMember refused, which is an invite-token validity oracle for a
	// principal the hub cannot name (an invite bootstraps an account on the
	// default, invite-only posture).
	me := s.requestUser(r)
	if normEmail(me.Email) == "" {
		http.Error(w, "sign in to accept an invite", http.StatusUnauthorized)
		return
	}
	// Check-and-add is one operation: the seat check counts members and the
	// join adds one, so without this the last seat can be sold twice.
	s.joinMu.Lock()
	defer s.joinMu.Unlock()
	inv, ok := s.Dir.Redeem(r.PathValue("token"))
	if !ok {
		http.Error(w, "this invite is invalid or expired", http.StatusNotFound)
		return
	}
	org, _ := s.Dir.Get(inv.Org)
	if org.Members[normEmail(me.Email)] == "" {
		if err := s.quota().CheckSeat(inv.Org, len(org.Members)); err != nil {
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
	}
	newMember := org.Members[normEmail(me.Email)] == ""
	if err := s.Dir.AddMember(inv.Org, me.Email, RoleMember); err != nil {
		s.writeDirErr(w, inv.Org, err)
		return
	}
	if newMember {
		s.Dir.RecordInviteUse(r.PathValue("token"))
	}
	writeJSON(w, map[string]any{"ok": true, "org": map[string]string{"id": org.ID, "name": org.Name}})
}
