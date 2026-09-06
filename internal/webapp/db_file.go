package webapp

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The file backend: each repository is one JSON file, cached in memory and
// rewritten atomically (temp + rename) on every change — the exact on-disk
// format and discipline the registries used before the store abstraction, so a
// running hub upgrades with no migration.

// writeFileAtomic writes data to path via a temp file + rename. Files land at
// 0600 and their directory at 0700 — one mode for the whole store, because
// every repo writes into the SAME hub data directory and MkdirAll is a no-op
// once it exists: a per-repo mode meant whichever file was written first
// decided whether the directory holding auth.json was world-readable.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".bdrive-tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// fileVersion is the file backend's Versioned token: size and modification
// time, which the temp-file + rename every write goes through always moves.
// A missing file gets its own token, so creating one counts as a change.
//
// ponytail: mtime+size, not an inode and not a content hash. Two hub PROCESSES
// writing the same byte count within one filesystem timestamp tick would look
// unchanged to each other — nanosecond mtimes (APFS, ext4, xfs, btrfs, ZFS)
// make that a theoretical window, and the file backend is not multi-process-
// safe regardless: every write is still read-modify-write-rename, so two
// processes can lose each other's records outright. This narrows the
// stale-read race; it does not close it. The SQL backend is the fix.
func fileVersion(path string) (string, error) {
	fi, err := os.Stat(path)
	if os.IsNotExist(err) {
		return "absent", nil
	}
	if err != nil {
		return "", err
	}
	return strconv.FormatInt(fi.Size(), 10) + "@" + strconv.FormatInt(fi.ModTime().UnixNano(), 10), nil
}

func readJSONFile(path string, into any) (found bool, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if err := json.Unmarshal(data, into); err != nil {
		return false, fmt.Errorf("parse %s: %w", path, err)
	}
	return true, nil
}

// fileMetaStore bundles the five file repositories rooted in one directory,
// keeping the historical filenames so an existing hub's data loads unchanged.
type fileMetaStore struct {
	accounts *fileAccountRepo
	projects *fileProjectRepo
	orgs     *fileOrgRepo
	shares   *fileShareRepo
	devices  *fileDeviceRepo
	reads    *fileReadRepo
	sessions *fileSessionReadRepo
}

// OpenFileStore builds the file backend over dir, using the historical
// filenames (auth.json, projects.json, orgs.json, shares.json, devices.json).
func OpenFileStore(dir string) (MetaStore, error) {
	return &fileMetaStore{
		accounts: newFileAccountRepo(filepath.Join(dir, "auth.json")),
		projects: newFileProjectRepo(filepath.Join(dir, "projects.json")),
		orgs:     newFileOrgRepo(filepath.Join(dir, "orgs.json")),
		shares:   newFileShareRepo(filepath.Join(dir, "shares.json")),
		devices:  newFileDeviceRepo(filepath.Join(dir, "devices.json")),
		reads:    newFileReadRepo(filepath.Join(dir, "reads.json")),
		sessions: newFileSessionReadRepo(filepath.Join(dir, "sessions.json")),
	}, nil
}

func (s *fileMetaStore) Accounts() AccountRepo         { return s.accounts }
func (s *fileMetaStore) Projects() ProjectRepo         { return s.projects }
func (s *fileMetaStore) Orgs() OrgRepo                 { return s.orgs }
func (s *fileMetaStore) Shares() ShareRepo             { return s.shares }
func (s *fileMetaStore) Devices() DeviceRepo           { return s.devices }
func (s *fileMetaStore) Reads() ReadRepo               { return s.reads }
func (s *fileMetaStore) SessionReads() SessionReadRepo { return s.sessions }
func (s *fileMetaStore) Close() error                  { return nil }

// ---- accounts (auth.json: users + tokens + policy) ----

type authFileShape struct {
	Users  []*authUser `json:"users"`
	Tokens []authToken `json:"tokens"`
	Policy *authPolicy `json:"policy,omitempty"`
}

type fileAccountRepo struct {
	path string

	mu     sync.Mutex
	users  map[string]*authUser
	tokens map[string]authToken
	policy *authPolicy
}

func newFileAccountRepo(path string) *fileAccountRepo {
	return &fileAccountRepo{path: path, users: map[string]*authUser{}, tokens: map[string]authToken{}}
}

func (r *fileAccountRepo) Version() (string, error) { return fileVersion(r.path) }

func (r *fileAccountRepo) Load() ([]*authUser, []authToken, *authPolicy, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reload()
}

// reload re-reads the file. Every write goes through it first, for the reason
// fileProjectRepo.reload states. The rows a stale rewrite brings back here are
// a deleted ACCOUNT and a revoked device TOKEN — the credential itself, not a
// grant on top of one. Callers hold mu.
func (r *fileAccountRepo) reload() ([]*authUser, []authToken, *authPolicy, error) {
	var f authFileShape
	if _, err := readJSONFile(r.path, &f); err != nil {
		return nil, nil, nil, err
	}
	r.users = map[string]*authUser{}
	r.tokens = map[string]authToken{}
	for _, u := range f.Users {
		r.users[u.ID] = u
	}
	for _, t := range f.Tokens {
		r.tokens[t.Hash] = t
	}
	r.policy = f.Policy
	return f.Users, f.Tokens, f.Policy, nil
}

// write persists users, tokens, and policy. Callers hold mu.
func (r *fileAccountRepo) write() error {
	var f authFileShape
	for _, u := range r.users {
		f.Users = append(f.Users, u)
	}
	for _, t := range r.tokens {
		f.Tokens = append(f.Tokens, t)
	}
	f.Policy = r.policy
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(r.path, append(data, '\n')) // holds password hashes
}

func (r *fileAccountRepo) PutAccount(u *authUser) error {
	if err := checkAccount(u); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, _, _, err := r.reload(); err != nil {
		return err
	}
	// An id identifies one account for the life of the hub. Overwriting a row
	// with a DIFFERENT account's is never an update — it is one account's
	// identity, org memberships and live device tokens transferring onto
	// another, and the original's password hash gone from disk.
	if prev, ok := r.users[u.ID]; ok && !strings.EqualFold(prev.Email, u.Email) {
		return fmt.Errorf("account id %s already belongs to another account", u.ID)
	}
	r.users[u.ID] = u
	return r.write()
}

func (r *fileAccountRepo) DeleteAccount(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, _, _, err := r.reload(); err != nil {
		return err
	}
	delete(r.users, id)
	return r.write()
}

func (r *fileAccountRepo) PutToken(t authToken) error {
	if err := checkToken(t); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, _, _, err := r.reload(); err != nil {
		return err
	}
	r.tokens[t.Hash] = t
	return r.write()
}

func (r *fileAccountRepo) DeleteToken(hash string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, _, _, err := r.reload(); err != nil {
		return err
	}
	delete(r.tokens, hash)
	return r.write()
}

func (r *fileAccountRepo) PutPolicy(p authPolicy) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, _, _, err := r.reload(); err != nil {
		return err
	}
	r.policy = &p
	return r.write()
}

// ---- projects (projects.json) ----

type fileProjectRepo struct {
	path string
	mu   sync.Mutex
	byID map[string]Project
}

func newFileProjectRepo(path string) *fileProjectRepo {
	return &fileProjectRepo{path: path, byID: map[string]Project{}}
}

func (r *fileProjectRepo) Version() (string, error) { return fileVersion(r.path) }

func (r *fileProjectRepo) Load() ([]Project, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reload()
}

// reload re-reads the file into byID. Every write goes through it first: byID
// is this process's copy of a file another hub process may also be writing, and
// a rewrite from a stale copy is how one hub's unrelated edit resurrected
// another hub's revoked grant. Callers hold mu.
func (r *fileProjectRepo) reload() ([]Project, error) {
	var f struct {
		Projects []Project `json:"projects"`
	}
	if _, err := readJSONFile(r.path, &f); err != nil {
		return nil, err
	}
	r.byID = map[string]Project{}
	for _, p := range f.Projects {
		r.byID[p.ID] = p
	}
	return f.Projects, nil
}

func (r *fileProjectRepo) write() error {
	list := make([]Project, 0, len(r.byID))
	for _, p := range r.byID {
		list = append(list, p)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	data, err := json.MarshalIndent(struct {
		Projects []Project `json:"projects"`
	}{list}, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(r.path, append(data, '\n'))
}

func (r *fileProjectRepo) Put(p Project) error {
	if err := checkProject(p); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, err := r.reload(); err != nil {
		return err
	}
	r.byID[p.ID] = p
	return r.write()
}

// PutMeta writes the project's own fields and keeps whatever grants are on
// disk — see rowScopedProjectRepo.
func (r *fileProjectRepo) PutMeta(p Project) error {
	if err := checkProject(p); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, err := r.reload(); err != nil {
		return err
	}
	// Folders is preserved for exactly the reason Perms is: a rename carries
	// the writer's whole in-memory Project, so without this a stale process
	// renaming a project resurrects every folder rule an admin removed.
	p.Perms, p.Folders = r.byID[p.ID].Perms, r.byID[p.ID].Folders
	r.byID[p.ID] = p
	return r.write()
}

// PutFolder writes one folder rule, leaving the project's other rules and its
// project-level grants exactly as they are on disk.
func (r *fileProjectRepo) PutFolder(project string, rule FolderRule) error {
	if err := storable(project, rule.Prefix); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, err := r.reload(); err != nil {
		return err
	}
	p, ok := r.byID[project]
	if !ok {
		return fmt.Errorf("no such project %q", project)
	}
	p = p.clone()
	replaced := false
	for i := range p.Folders {
		if p.Folders[i].Prefix == rule.Prefix {
			p.Folders[i] = rule.clone()
			replaced = true
			break
		}
	}
	if !replaced {
		p.Folders = append(p.Folders, rule.clone())
	}
	r.byID[project] = p
	return r.write()
}

// DeleteFolder removes one rule. A prefix with no rule is not an error: the
// caller's intent (this prefix carries no rule) already holds.
func (r *fileProjectRepo) DeleteFolder(project, prefix string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, err := r.reload(); err != nil {
		return err
	}
	p, ok := r.byID[project]
	if !ok {
		return fmt.Errorf("no such project %q", project)
	}
	p = p.clone()
	kept := p.Folders[:0]
	for _, f := range p.Folders {
		if f.Prefix != prefix {
			kept = append(kept, f)
		}
	}
	p.Folders = kept
	r.byID[project] = p
	return r.write()
}

// PutPerm writes one grant. An empty level removes it.
func (r *fileProjectRepo) PutPerm(project, email, level string) error {
	if err := storable(project, email, level); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, err := r.reload(); err != nil {
		return err
	}
	p, ok := r.byID[project]
	if !ok {
		return fmt.Errorf("no such project %q", project)
	}
	p = p.clone()
	switch {
	case level == "":
		delete(p.Perms, email)
	case p.Perms == nil:
		p.Perms = map[string]string{email: level}
	default:
		p.Perms[email] = level
	}
	r.byID[project] = p
	return r.write()
}

func (r *fileProjectRepo) Delete(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, err := r.reload(); err != nil {
		return err
	}
	delete(r.byID, id)
	return r.write()
}

// ---- orgs (orgs.json: orgs + invites) ----

type fileOrgRepo struct {
	path    string
	mu      sync.Mutex
	byID    map[string]Org
	invites map[string]OrgInvite
}

func newFileOrgRepo(path string) *fileOrgRepo {
	return &fileOrgRepo{path: path, byID: map[string]Org{}, invites: map[string]OrgInvite{}}
}

func (r *fileOrgRepo) Version() (string, error) { return fileVersion(r.path) }

func (r *fileOrgRepo) Load() ([]Org, []OrgInvite, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reload()
}

// reload re-reads the file. Every write goes through it first, for the reason
// fileProjectRepo.reload states: this map is one process's copy of a file
// another hub process may also be writing, and rewriting the whole file from a
// stale copy is how one hub's unrelated edit resurrected another hub's
// revocation. On orgs the resurrected row is the OUTER wall — every per-project
// route 403s for a non-member — so it undoes more than a project grant does.
// Callers hold mu.
func (r *fileOrgRepo) reload() ([]Org, []OrgInvite, error) {
	var f struct {
		Orgs    []Org       `json:"orgs"`
		Invites []OrgInvite `json:"invites"`
	}
	if _, err := readJSONFile(r.path, &f); err != nil {
		return nil, nil, err
	}
	r.byID = map[string]Org{}
	r.invites = map[string]OrgInvite{}
	for _, o := range f.Orgs {
		r.byID[o.ID] = o
	}
	for _, i := range f.Invites {
		r.invites[i.Token] = i
	}
	return f.Orgs, f.Invites, nil
}

func (r *fileOrgRepo) write() error {
	var f struct {
		Orgs    []Org       `json:"orgs"`
		Invites []OrgInvite `json:"invites"`
	}
	for _, o := range r.byID {
		f.Orgs = append(f.Orgs, o)
	}
	sort.Slice(f.Orgs, func(i, j int) bool { return f.Orgs[i].ID < f.Orgs[j].ID })
	for _, i := range r.invites {
		if !i.expired() {
			f.Invites = append(f.Invites, i)
		}
	}
	sort.Slice(f.Invites, func(i, j int) bool { return f.Invites[i].Token < f.Invites[j].Token })
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(r.path, append(data, '\n'))
}

func (r *fileOrgRepo) PutOrg(o Org) error {
	if err := checkOrg(o); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, _, err := r.reload(); err != nil {
		return err
	}
	r.byID[o.ID] = o
	return r.write()
}

// PutOrgMeta writes the org's own fields and keeps whatever members are on
// disk — see rowScopedOrgRepo.
func (r *fileOrgRepo) PutOrgMeta(o Org) error {
	if err := checkOrg(o); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, _, err := r.reload(); err != nil {
		return err
	}
	prev := r.byID[o.ID]
	o.Members, o.Joined = prev.Members, prev.Joined
	r.byID[o.ID] = o
	return r.write()
}

// PutMember writes one membership row. An empty role removes it.
func (r *fileOrgRepo) PutMember(org, email, role string, joined time.Time) error {
	if err := storable(org, email, role); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, _, err := r.reload(); err != nil {
		return err
	}
	o, ok := r.byID[org]
	if !ok {
		return fmt.Errorf("no such organization %q", org)
	}
	o = o.clone()
	if role == "" {
		delete(o.Members, email)
		delete(o.Joined, email)
	} else {
		o.Members[email] = role
		o.Joined[email] = joined
	}
	r.byID[org] = o
	return r.write()
}

func (r *fileOrgRepo) DeleteOrg(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, _, err := r.reload(); err != nil {
		return err
	}
	delete(r.byID, id)
	return r.write()
}

func (r *fileOrgRepo) PutInvite(i OrgInvite) error {
	if err := checkInvite(i); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, _, err := r.reload(); err != nil {
		return err
	}
	r.invites[i.Token] = i
	return r.write()
}

func (r *fileOrgRepo) DeleteInvite(token string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, _, err := r.reload(); err != nil {
		return err
	}
	delete(r.invites, token)
	return r.write()
}

// ---- shares (shares.json) ----

type fileShareRepo struct {
	path    string
	mu      sync.Mutex
	byToken map[string]Share
}

func newFileShareRepo(path string) *fileShareRepo {
	return &fileShareRepo{path: path, byToken: map[string]Share{}}
}

func (r *fileShareRepo) Version() (string, error) { return fileVersion(r.path) }

func (r *fileShareRepo) Load() ([]Share, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reload()
}

// reload re-reads the file before every write, for the reason
// fileProjectRepo.reload states. Here the row a stale rewrite brings back is an
// UNAUTHENTICATED public URL: a /s/<token> revoked on one hub process returned
// the moment any second process minted any unrelated share. Callers hold mu.
func (r *fileShareRepo) reload() ([]Share, error) {
	var f struct {
		Shares []Share `json:"shares"`
	}
	if _, err := readJSONFile(r.path, &f); err != nil {
		return nil, err
	}
	r.byToken = map[string]Share{}
	for _, s := range f.Shares {
		r.byToken[s.Token] = s
	}
	return f.Shares, nil
}

func (r *fileShareRepo) write() error {
	var f struct {
		Shares []Share `json:"shares"`
	}
	for _, s := range r.byToken {
		f.Shares = append(f.Shares, s)
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(r.path, append(data, '\n'))
}

func (r *fileShareRepo) Put(s Share) error {
	if err := checkShare(s); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, err := r.reload(); err != nil {
		return err
	}
	r.byToken[s.Token] = s
	return r.write()
}

func (r *fileShareRepo) Delete(token string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, err := r.reload(); err != nil {
		return err
	}
	delete(r.byToken, token)
	return r.write()
}

// ---- devices (devices.json) ----

// The row key is (user, id), matching the registry above it. Keyed by id
// alone, two accounts' rows collapsed into one on disk and whichever wrote
// last was the only one a restart reloaded — so the whole per-account model
// lived exactly as long as the process, and after any deploy the hub believed
// a device belonged to whoever named it last.
type fileDeviceRepo struct {
	path string
	mu   sync.Mutex
	rows map[devKey]DeviceInfo
}

func newFileDeviceRepo(path string) *fileDeviceRepo {
	return &fileDeviceRepo{path: path, rows: map[devKey]DeviceInfo{}}
}

func (r *fileDeviceRepo) Version() (string, error) { return fileVersion(r.path) }

func (r *fileDeviceRepo) Load() ([]DeviceInfo, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reload()
}

// reload re-reads the file before every write, for the reason
// fileProjectRepo.reload states. The row a stale rewrite ERASES here is the one
// ownership fact ownJournal consults, and an id with no owning row is an id
// DeviceRegistry.Bind hands to the next account that asks for it — the
// one-writer invariant lost to a second process's routine Observe. Callers hold
// mu.
func (r *fileDeviceRepo) reload() ([]DeviceInfo, error) {
	var f struct {
		Devices []DeviceInfo `json:"devices"`
	}
	if _, err := readJSONFile(r.path, &f); err != nil {
		return nil, err
	}
	r.rows = map[devKey]DeviceInfo{}
	for _, d := range f.Devices {
		r.rows[devKey{d.User, d.ID}] = d
	}
	return f.Devices, nil
}

func (r *fileDeviceRepo) write() error {
	var f struct {
		Devices []DeviceInfo `json:"devices"`
	}
	for _, d := range r.rows {
		f.Devices = append(f.Devices, d)
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(r.path, append(data, '\n'))
}

func (r *fileDeviceRepo) Put(d DeviceInfo) error {
	if err := checkDevice(d); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, err := r.reload(); err != nil {
		return err
	}
	r.rows[devKey{d.User, d.ID}] = d
	return r.write()
}

func (r *fileDeviceRepo) Delete(user, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, err := r.reload(); err != nil {
		return err
	}
	delete(r.rows, devKey{user, id})
	return r.write()
}

// ---- reads (reads.json) ----

type fileReadRepo struct {
	path  string
	mu    sync.Mutex
	byKey map[ReadStatKey]ReadStat
}

func newFileReadRepo(path string) *fileReadRepo {
	return &fileReadRepo{path: path, byKey: map[ReadStatKey]ReadStat{}}
}

func (r *fileReadRepo) Version() (string, error) { return fileVersion(r.path) }

func (r *fileReadRepo) Load() ([]ReadStat, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reload()
}

// reload re-reads the file before every write, for the reason
// fileProjectRepo.reload states. Not authorization — integrity: a stale rewrite
// ERASES every bucket another hub process recorded since boot (the operator's
// staleness view silently loses reads), and a stale DeleteBatch resurrects the
// daily buckets a fold already rolled into an all-time row, double-counting
// them. Callers hold mu.
func (r *fileReadRepo) reload() ([]ReadStat, error) {
	var f struct {
		Reads []ReadStat `json:"reads"`
	}
	if _, err := readJSONFile(r.path, &f); err != nil {
		return nil, err
	}
	r.byKey = map[ReadStatKey]ReadStat{}
	for _, st := range f.Reads {
		r.byKey[st.key()] = st
	}
	return f.Reads, nil
}

func (r *fileReadRepo) write() error {
	var f struct {
		Reads []ReadStat `json:"reads"`
	}
	f.Reads = make([]ReadStat, 0, len(r.byKey))
	for _, st := range r.byKey {
		f.Reads = append(f.Reads, st)
	}
	sort.Slice(f.Reads, func(i, j int) bool {
		a, b := f.Reads[i], f.Reads[j]
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		return a.Day < b.Day
	})
	data, err := json.Marshal(f) // telemetry: compact beats pretty
	if err != nil {
		return err
	}
	// 0700 dir: buckets carry actor emails, like auth.json carries accounts.
	return writeFileAtomic(r.path, append(data, '\n'))
}

func (r *fileReadRepo) PutBatch(stats []ReadStat) error {
	for _, s := range stats {
		if err := checkReadStat(s); err != nil {
			return err
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, err := r.reload(); err != nil {
		return err
	}
	for _, st := range stats {
		r.byKey[st.key()] = st
	}
	return r.write()
}

func (r *fileReadRepo) DeleteBatch(keys []ReadStatKey) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, err := r.reload(); err != nil {
		return err
	}
	for _, k := range keys {
		delete(r.byKey, k)
	}
	return r.write()
}

// ---- session reads (sessions.json) ----

type fileSessionReadRepo struct {
	path  string
	mu    sync.Mutex
	byKey map[sessionReadKey]SessionRead
}

func newFileSessionReadRepo(path string) *fileSessionReadRepo {
	return &fileSessionReadRepo{path: path, byKey: map[sessionReadKey]SessionRead{}}
}

// reload re-reads before every write, for fileReadRepo.reload's reason: a
// stale rewrite erases rows another hub process recorded since boot. Callers
// hold mu.
func (r *fileSessionReadRepo) reload() error {
	var f struct {
		Sessions []SessionRead `json:"sessions"`
	}
	if _, err := readJSONFile(r.path, &f); err != nil {
		return err
	}
	r.byKey = map[sessionReadKey]SessionRead{}
	for _, sr := range f.Sessions {
		r.byKey[sr.key()] = sr
	}
	return nil
}

func (r *fileSessionReadRepo) write() error {
	var f struct {
		Sessions []SessionRead `json:"sessions"`
	}
	f.Sessions = make([]SessionRead, 0, len(r.byKey))
	for _, sr := range r.byKey {
		f.Sessions = append(f.Sessions, sr)
	}
	sort.Slice(f.Sessions, func(i, j int) bool {
		a, b := f.Sessions[i], f.Sessions[j]
		if a.Session != b.Session {
			return a.Session < b.Session
		}
		return a.Path < b.Path
	})
	data, err := json.Marshal(f) // telemetry: compact beats pretty
	if err != nil {
		return err
	}
	return writeFileAtomic(r.path, append(data, '\n'))
}

func (r *fileSessionReadRepo) PutBatch(reads []SessionRead) error {
	for _, sr := range reads {
		if err := checkSessionRead(sr); err != nil {
			return err
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.reload(); err != nil {
		return err
	}
	for _, sr := range reads {
		r.byKey[sr.key()] = sr
	}
	return r.write()
}

func (r *fileSessionReadRepo) ListBySession(project, session, device string) ([]SessionRead, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.reload(); err != nil {
		return nil, err
	}
	var out []SessionRead
	for k, sr := range r.byKey {
		if k.Project == project && k.Session == session && k.Device == device {
			out = append(out, sr)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

func (r *fileSessionReadRepo) PruneBefore(t time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.reload(); err != nil {
		return err
	}
	n := 0
	for k, sr := range r.byKey {
		if sr.Last.Before(t) {
			delete(r.byKey, k)
			n++
		}
	}
	if n == 0 {
		return nil
	}
	return r.write()
}
