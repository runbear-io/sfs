package webapp

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"html"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// BuiltinAuth is the open-source identity provider: email + password + name
// accounts and long-lived device tokens, persisted in one JSON file (loaded
// at open, rewritten atomically on every change — same discipline as the
// project registry). It owns the /auth/* pages the browser sees and the
// /api/auth/* endpoints the CLI uses.
type BuiltinAuth struct {
	AllowSignup bool
	Mail        *Mailer // nil → reset links go to the server log

	// Public-URL signup gating (all optional; set after Open). A hub reachable
	// from the internet should use at least one of these.
	AllowedDomains      []string        // if non-empty, signup email domain must match one
	RequireVerification bool            // new accounts must click an email link before activation
	RequireApproval     bool            // new accounts wait for an admin to approve them
	Admins              map[string]bool // hub admins (lowercase emails): approve users, govern shares
	Brand               string          // optional name shown on the sign-in page

	// BaseURL is the hub's public origin ("https://drive.acme.com"). Links the
	// hub MAILS are built from it. Empty → the hub has no origin it can trust
	// and mailed links stop being absolute as soon as two requests disagree
	// about the host; see mailBaseURL.
	BaseURL string

	// InviteValid, when set, reports whether a token is a live org invite.
	// It lets an invite link bootstrap an account on an invite-only hub
	// (AllowSignup false) — the one path in without self-signup. Wired to
	// OrgDB.ValidInvite by the server. Nil → no invite-based signup.
	InviteValid func(token string) bool

	// Offboard, when set, is called with the address of an account that has
	// just been removed. Everything downstream of removal is keyed by email
	// (org role, project grant, share liveness), so without it the grants
	// outlive the account. Wired to Server.offboard by the server.
	Offboard func(email string)

	// BindDevice, when set, records that a device id belongs to an account, at
	// the moment a token is minted for it. It is the ONLY way an ownership row
	// is created for an id that has never synced — see DeviceRegistry.Bind for
	// why first-claim-on-write could not be. Installed by UseDeviceBinder.
	BindDevice DeviceBinder

	store AccountRepo
	ver   versionGate // skips the re-read when the store has not moved

	// cli serves `bdrive login` — the browser and device flows, shared with
	// every other provider (see CLIAuth), which is why nothing about them
	// lives in this file.
	cli *CLIAuth

	mu         sync.Mutex
	warnedBase bool                 // "no auth.base_url" logged once (see mailBaseURL)
	warnedLoad bool                 // "re-read failed" logged once (see refresh)
	users      map[string]*authUser // by id
	tokens     map[string]authToken // by sha256(token)

	// Ephemeral single-use state; a server restart just cancels pending
	// verifications and resets.
	pending map[string]pendingGrant // verification links, reset tokens
}

type authUser struct {
	ID      string    `json:"id"`
	Email   string    `json:"email"`
	Name    string    `json:"name"`
	Pass    string    `json:"pass"` // bcrypt hash
	Status  string    `json:"status,omitempty"`
	Created time.Time `json:"created"`
}

// Account status. Empty is treated as active so accounts created before
// gating existed keep working.
const (
	statusActive     = "active"
	statusUnverified = "unverified" // awaiting email verification
	statusPending    = "pending"    // verified (or verification off) but awaiting admin approval
)

func (u *authUser) active() bool { return u.Status == "" || u.Status == statusActive }

type authToken struct {
	Hash    string    `json:"hash"` // sha256 of the token; plaintext is never stored
	User    string    `json:"user"`
	Device  string    `json:"device"`
	Created time.Time `json:"created"`
}

type pendingGrant struct {
	kind    string // "verify" (email link) | "reset" (password reset link)
	user    string
	expires time.Time
}

// NewBuiltinAuth builds the account service over an AccountRepo, loading its
// accounts, tokens, and persisted policy.
func NewBuiltinAuth(store AccountRepo, allowSignup bool, mail *Mailer) (*BuiltinAuth, error) {
	a := &BuiltinAuth{
		AllowSignup: allowSignup, Mail: mail, store: store,
		users:   make(map[string]*authUser),
		tokens:  make(map[string]authToken),
		pending: make(map[string]pendingGrant),
	}
	a.cli = NewCLIAuth(a.sessionUser, a.finishLogin)
	users, tokens, policy, err := store.Load()
	if err != nil {
		return nil, err
	}
	for _, u := range users {
		a.users[u.ID] = u
	}
	for _, t := range tokens {
		a.tokens[t.Hash] = t
	}
	// A UI-saved policy is the persisted operational default; the server
	// config can still override it at startup (see web.go), so a sysadmin who
	// pins a value in the config file always wins over a browser toggle.
	if policy != nil {
		a.RequireVerification = policy.RequireVerification
		a.RequireApproval = policy.RequireApproval
	}
	return a, nil
}

// OpenBuiltinAuth loads (or starts) the file-backed account registry at path.
func OpenBuiltinAuth(path string, allowSignup bool, mail *Mailer) (*BuiltinAuth, error) {
	return NewBuiltinAuth(newFileAccountRepo(path), allowSignup, mail)
}

// authPolicy is the UI-tunable slice of gating (persisted in auth.json).
// Domain allowlist and the admin list are intentionally NOT here — they are
// security-critical identity config owned by whoever controls the server,
// not something a browser session should be able to widen.
type authPolicy struct {
	RequireVerification bool `json:"require_verification"`
	RequireApproval     bool `json:"require_approval"`
}

// SetPolicy updates the tunable gating toggles and persists them.
//
// The prospective policy goes through the startup validator FIRST. The hub
// starts legally as {allow_signup:true, require_approval:true}; one admin POST
// used to remove the only gate, and because SetPolicy persists, the hub then
// survived a restart the same binary refuses to perform — CLAUDE.md states the
// guarantee as "refuses an ungated open hub rather than silently leaving the
// door open". Here rather than in handleAdminPolicy because the handler is one
// caller of this and a second caller would arrive without the check; the
// handler only had the mailer half of the rule anyway.
func (a *BuiltinAuth) SetPolicy(requireVerification, requireApproval bool) error {
	if err := a.signupPolicyError(requireVerification, requireApproval); err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	// Persist first: a gating change the store refused must not un-gate the
	// hub in memory, which is the widening direction — new signups become
	// active across a restart the store never agreed to.
	if err := a.store.PutPolicy(authPolicy{RequireVerification: requireVerification, RequireApproval: requireApproval}); err != nil {
		return err
	}
	a.RequireVerification = requireVerification
	a.RequireApproval = requireApproval
	return nil
}

// refresh re-reads accounts and device tokens from the store. Callers hold mu.
//
// Same defect ProjectDB.refresh closed in round 12, one wall further out: these
// maps are the CREDENTIAL, not a grant on top of one. Loaded at open and never
// re-read, a hub running two processes served `bdrive logout` (or an admin's
// "revoke this device") on whichever process handled it and on no other — the
// lost laptop's token kept authenticating everywhere else for the life of those
// processes — and a deleted account could sign in again with its old password
// as soon as any second-process write rewrote auth.json.
//
// Policy is deliberately NOT re-read: the config file overrides it at startup
// (web.go), and reloading would let the persisted value quietly win back over
// the value the sysadmin pinned.
//
// A store that cannot answer leaves the maps in place — see ProjectDB.refresh
// for the trade.
//
// Gated on the store's change token (Versioned) like ProjectDB.refresh: still
// a re-read on every authenticated request, but only when something moved.
func (a *BuiltinAuth) refresh() {
	token, stale := a.ver.stale(a.store)
	if !stale {
		return
	}
	users, tokens, _, err := a.store.Load()
	if err != nil {
		if !a.warnedLoad {
			a.warnedLoad = true
			log.Printf("beardrive: account store re-read failed, serving the last known accounts: %v", err)
		}
		return
	}
	a.warnedLoad = false
	a.ver.fresh(token)
	nextUsers := make(map[string]*authUser, len(users))
	for _, u := range users {
		nextUsers[u.ID] = u
	}
	nextTokens := make(map[string]authToken, len(tokens))
	for _, t := range tokens {
		nextTokens[t.Hash] = t
	}
	a.users, a.tokens = nextUsers, nextTokens
}

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func hashToken(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

// ---- account + token operations ----

func (a *BuiltinAuth) findByEmail(email string) *authUser {
	for _, u := range a.users {
		if strings.EqualFold(u.Email, email) {
			return u
		}
	}
	return nil
}

// signup creates a self-service account, subject to the domain allowlist and
// starting in the state the gating policy dictates.
func (a *BuiltinAuth) signup(email, name, password string) (*authUser, error) {
	return a.createAccount(email, name, password, false)
}

// signupInvited creates an account from a valid invite link. An invite is an
// explicit grant by an owner, so it is the vetting: the domain allowlist and
// the approval/verification gates are bypassed and the account is active. The
// caller must have already checked the invite token is live.
func (a *BuiltinAuth) signupInvited(email, name, password string) (*authUser, error) {
	return a.createAccount(email, name, password, true)
}

func (a *BuiltinAuth) createAccount(email, name, password string, viaInvite bool) (*authUser, error) {
	email = strings.TrimSpace(strings.ToLower(email))
	// trimText, not TrimSpace: a display name is peer-written text that travels
	// as far as a project name does. RemoteSource.Commit stamps it as
	// Op.UserName on every browser write, so it lands in the journal every
	// device replays and in the History row whoChanged() renders — carrying the
	// bidi overrides and C0/C1 runs a NOTE on the same row is refused for, with
	// no device and no journal access needed. Route it through the choke point
	// that already normalizes project names rather than growing a second rule.
	name = trimText(name, 128)
	if email == "" || !strings.Contains(email, "@") {
		return nil, fmt.Errorf("a valid email is required")
	}
	if !viaInvite && !a.domainAllowed(email) {
		return nil, fmt.Errorf("this server only accepts %s email addresses", a.domainList())
	}
	if name == "" {
		return nil, fmt.Errorf("a name is required")
	}
	if len(password) < 8 {
		return nil, fmt.Errorf("password must be at least 8 characters")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return nil, err
	}
	status := a.initialStatus()
	// An invite is an owner's explicit grant; an email on the config's admin
	// list is the operator's own. Both are stronger vetting than the signup
	// gates, so these accounts activate immediately — otherwise a fresh
	// approval-gated hub would strand its first admin as pending forever.
	if viaInvite || a.isAdmin(email) {
		status = statusActive
	}
	a.mu.Lock()
	a.refresh()
	defer a.mu.Unlock()
	if a.findByEmail(email) != nil {
		return nil, fmt.Errorf("an account with this email already exists")
	}
	// 128 bits, and never one already in use. randHex(4) was 32 bits with no
	// uniqueness check: no attacker is needed, the birthday bound alone gives
	// ~1% at 9,300 accounts and even odds at 77,000 — and a collision put a new
	// account on a live one's id, so the victim's device tokens authenticated
	// as the newcomer and PutAccount overwrote the victim's row, password hash
	// included, with no way back. The repos refuse the overwrite too; this is
	// the half that makes a legitimate signup never ask for it.
	id := "u-" + randHex(16)
	for a.users[id] != nil {
		id = "u-" + randHex(16)
	}
	u := &authUser{
		ID: id, Email: email, Name: name,
		Pass: string(hash), Status: status, Created: time.Now().UTC(),
	}
	a.users[u.ID] = u
	if err := a.store.PutAccount(u); err != nil {
		delete(a.users, u.ID)
		return nil, err
	}
	return u, nil
}

// initialStatus is the account state a new signup starts in, given the
// server's gating config: verify first, else approve first, else active.
func (a *BuiltinAuth) initialStatus() string {
	switch {
	case a.RequireVerification:
		return statusUnverified
	case a.RequireApproval:
		return statusPending
	default:
		return statusActive
	}
}

// ValidateSignupPolicy rejects incoherent signup configurations at startup so
// a hub is never accidentally left open to fake-email signups. The three
// supported postures are: invite-only (AllowSignup false — the default),
// approval-gated, and domain-restricted with email verification.
//
//   - Open self-signup must carry at least one gate (allowed domains, admin
//     approval, or email verification). Without one, anyone can register any
//     address — the exact hole this guards.
//   - Email verification needs a mailer: without SMTP the link only reaches
//     the server log, so it can't actually gate real users.
func (a *BuiltinAuth) ValidateSignupPolicy() error {
	if err := a.signupPolicyError(a.RequireVerification, a.RequireApproval); err != nil {
		return err
	}
	// Startup only: SetPolicy cannot change either side of this, so refusing a
	// live toggle over it would be refusing an unrelated misconfiguration.
	if a.Mail != nil && a.BaseURL == "" {
		return fmt.Errorf("auth: smtp is configured but auth.base_url is not — the only other origin a mailed link could carry is the Host header of the request that triggered it, which an anonymous stranger chooses; set auth.base_url to this hub's public origin")
	}
	return nil
}

// signupPolicyError is the gating rule applied to a PROSPECTIVE pair of
// toggles, so the startup check and the live change (SetPolicy) are literally
// the same predicate rather than two that drift — which is how a browser POST
// reached a posture the same binary refuses to boot in.
func (a *BuiltinAuth) signupPolicyError(requireVerification, requireApproval bool) error {
	if requireVerification && a.Mail == nil {
		return fmt.Errorf("auth: require_verification needs an smtp mailer — without one the verification link only reaches the server log; configure auth.smtp or turn verification off")
	}
	if a.AllowSignup && len(a.AllowedDomains) == 0 && !requireApproval && !requireVerification {
		return fmt.Errorf("auth: open self-signup has no gate, so anyone could register any email — set allow_signup:false (invite-only, the default), or add allowed_domains, require_approval, or require_verification")
	}
	return nil
}

// afterVerify is the state a just-verified account moves to.
func (a *BuiltinAuth) afterVerify() string {
	if a.RequireApproval {
		return statusPending
	}
	return statusActive
}

func emailDomain(email string) string {
	if i := strings.LastIndex(email, "@"); i >= 0 {
		return strings.ToLower(email[i+1:])
	}
	return ""
}

func (a *BuiltinAuth) domainAllowed(email string) bool {
	if len(a.AllowedDomains) == 0 {
		return true
	}
	d := emailDomain(email)
	for _, allowed := range a.AllowedDomains {
		if strings.EqualFold(strings.TrimPrefix(strings.TrimSpace(allowed), "@"), d) {
			return true
		}
	}
	return false
}

func (a *BuiltinAuth) domainList() string {
	parts := make([]string, len(a.AllowedDomains))
	for i, d := range a.AllowedDomains {
		parts[i] = "@" + strings.TrimPrefix(strings.TrimSpace(d), "@")
	}
	return strings.Join(parts, ", ")
}

func (a *BuiltinAuth) isAdmin(email string) bool {
	return a.Admins != nil && a.Admins[normEmail(email)]
}

func (a *BuiltinAuth) accountCount() int {
	a.mu.Lock()
	a.refresh()
	defer a.mu.Unlock()
	return len(a.users)
}

func (a *BuiltinAuth) verifyPassword(email, password string) *authUser {
	a.mu.Lock()
	a.refresh()
	u := a.findByEmail(email)
	a.mu.Unlock()
	if u == nil {
		// burn comparable time so missing accounts aren't detectable
		bcrypt.CompareHashAndPassword([]byte("$2a$10$0000000000000000000000000000000000000000000000000000"), []byte(password))
		return nil
	}
	if bcrypt.CompareHashAndPassword([]byte(u.Pass), []byte(password)) != nil {
		return nil
	}
	return u
}

// issueToken mints a device token for the user and persists its hash. The
// plaintext is returned exactly once.
func (a *BuiltinAuth) issueToken(userID, device string) (string, error) {
	tok := "bdt_" + randHex(20)
	a.mu.Lock()
	a.refresh()
	defer a.mu.Unlock()
	t := authToken{Hash: hashToken(tok), User: userID, Device: device, Created: time.Now().UTC()}
	a.tokens[t.Hash] = t
	if err := a.store.PutToken(t); err != nil {
		delete(a.tokens, t.Hash)
		return "", err
	}
	return tok, nil
}

func (a *BuiltinAuth) revokeToken(tok string) error {
	a.mu.Lock()
	a.refresh()
	defer a.mu.Unlock()
	t, ok := a.tokens[hashToken(tok)]
	if !ok {
		return nil // already gone: the postcondition holds
	}
	delete(a.tokens, hashToken(tok))
	return a.killToken(t)
}

// killToken ends one credential durably. The delete alone was not durable: its
// error was discarded, so a logout or a password reset reported success while
// the row survived on disk and came back live at the next restart. Voiding the
// row first is a write that has to succeed for the revocation to be reported
// as done — a row naming no account resolves to no user in userForToken, so
// even if the delete then fails the credential is dead. Caller holds a.mu.
func (a *BuiltinAuth) killToken(t authToken) error {
	void := t
	void.User = ""
	if err := a.store.PutToken(void); err != nil {
		log.Printf("beardrive: could not revoke a token durably: %v", err)
		return err
	}
	if err := a.store.DeleteToken(t.Hash); err != nil {
		log.Printf("beardrive: revoked token row left void on disk (delete failed): %v", err)
	}
	return nil
}

// revokeTokensFor kills every credential a user holds — browser sessions and
// device tokens alike, since both are rows in a.tokens. Used by the password
// reset: re-keying an account that a thief still has a live token for would
// recover nothing.
func (a *BuiltinAuth) revokeTokensFor(userID string) {
	a.mu.Lock()
	a.refresh()
	defer a.mu.Unlock()
	a.revokeTokensForLocked(userID)
}

func (a *BuiltinAuth) revokeTokensForLocked(userID string) {
	if userID == "" {
		return
	}
	for hash, t := range a.tokens {
		if t.User == userID {
			delete(a.tokens, hash)
			a.killToken(t)
		}
	}
	a.revokeGrantsForLocked(userID)
}

// revokeGrantsForLocked drops every outstanding one-time mail grant an account
// holds. Callers hold mu.
//
// a.pending is a credential table, not a scratchpad: a "reset" grant sets the
// password without knowing the old one, and a "verify" grant signs its holder
// straight in (pageVerify's last arm calls startSession) with no password at
// all, on a 24-hour TTL minted at signup. Revoking the token table and stopping
// there left both alive across the ONE action a user is told to take when they
// suspect compromise — so a thief who requested a reset link before the victim
// recovered still held a password-setting capability afterwards, and a stale
// verification mail was still a passwordless sign-in.
//
// It lives inside revokeTokensForLocked rather than beside its two call sites
// because "end every credential this account holds" is one operation with one
// meaning; the reset page and Deny already both ask for it.
func (a *BuiltinAuth) revokeGrantsForLocked(userID string) {
	for id, g := range a.pending {
		if g.user == userID {
			delete(a.pending, id)
		}
	}
}

func (a *BuiltinAuth) userForToken(tok string) (User, bool) {
	a.mu.Lock()
	a.refresh()
	defer a.mu.Unlock()
	t, ok := a.tokens[hashToken(tok)]
	if !ok || t.User == "" {
		return User{}, false
	}
	u, ok := a.users[t.User]
	if !ok || !u.active() {
		return User{}, false
	}
	return User{ID: u.ID, Email: u.Email, Name: u.Name, Admin: a.isAdmin(u.Email)}, true
}

// sendVerification emails (or logs) a verification link for the account.
func (a *BuiltinAuth) sendVerification(u *authUser) {
	tok := a.newGrant("verify", u.ID, 24*time.Hour)
	link := a.mailBaseURL() + "/auth/verify?token=" + tok
	subject := "Verify your BearDrive account"
	body := "Confirm your email to activate your BearDrive account:\n\n  " + link +
		"\n\nThis link is valid for 24 hours. If you didn't sign up, ignore this email."
	if a.Mail == nil {
		fmt.Printf("verification link for %s:\n  %s\n", u.Email, link)
		return
	}
	if err := a.Mail.Send(u.Email, subject, body); err != nil {
		fmt.Printf("verification link for %s (email not sent: %v):\n  %s\n", u.Email, err, link)
	}
}

// grant helpers: single-use email links with expiry (verification, reset).
// The CLI's own pending sign-ins live in CLIAuth, not here.
func (a *BuiltinAuth) newGrant(kind, user string, ttl time.Duration) string {
	id := randHex(16)
	a.mu.Lock()
	a.pending[id] = pendingGrant{kind: kind, user: user, expires: time.Now().Add(ttl)}
	a.mu.Unlock()
	return id
}

func (a *BuiltinAuth) takeGrant(kind, id string) (pendingGrant, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	g, ok := a.pending[id]
	if !ok || g.kind != kind || time.Now().After(g.expires) {
		delete(a.pending, id)
		return pendingGrant{}, false
	}
	delete(a.pending, id)
	return g, true
}

// Branding is the hub name this provider renders on its own pages.
func (a *BuiltinAuth) Branding() string { return a.Brand }

// Policy reports this provider's signup gates (webapp.AccountApprover). The
// provider assembles it so the hub never reaches into these fields itself.
func (a *BuiltinAuth) Policy() SignupPolicy {
	a.mu.Lock()
	defer a.mu.Unlock()
	admins := make([]string, 0, len(a.Admins))
	for e := range a.Admins {
		admins = append(admins, e)
	}
	sort.Strings(admins)
	return SignupPolicy{
		RequireVerification: a.RequireVerification,
		RequireApproval:     a.RequireApproval,
		AllowSignup:         a.AllowSignup,
		AllowedDomains:      a.AllowedDomains,
		Admins:              admins,
		Mailer:              a.Mail != nil,
	}
}

// PendingUsers lists accounts awaiting admin approval, oldest first.
func (a *BuiltinAuth) PendingUsers() []User {
	a.mu.Lock()
	a.refresh()
	defer a.mu.Unlock()
	var us []*authUser
	for _, u := range a.users {
		if u.Status == statusPending {
			us = append(us, u)
		}
	}
	sortByAge(us)
	out := make([]User, len(us))
	for i, u := range us {
		out[i] = User{ID: u.ID, Email: u.Email, Name: u.Name}
	}
	return out
}

// Approve activates a pending account.
func (a *BuiltinAuth) Approve(id string) error {
	a.mu.Lock()
	a.refresh()
	defer a.mu.Unlock()
	u, ok := a.users[id]
	if !ok {
		return fmt.Errorf("no such account")
	}
	// Persist first, apply after: an approval the store refused must not
	// activate the account in memory — the admin is told it failed while the
	// account authenticates until the next restart.
	next := *u
	next.Status = statusActive
	if err := a.store.PutAccount(&next); err != nil {
		return err
	}
	u.Status = statusActive
	return nil
}

// Deny removes a pending account.
func (a *BuiltinAuth) Deny(id string) error {
	a.mu.Lock()
	a.refresh()
	u, ok := a.users[id]
	if !ok {
		a.mu.Unlock()
		return fmt.Errorf("no such account")
	}
	// Persist first: a removal the store refused must not empty the registry
	// anyway, or the account is gone until the next restart and then signs in
	// again with its old password.
	if err := a.store.DeleteAccount(id); err != nil {
		a.mu.Unlock()
		return err
	}
	// Removing the account is not enough on its own: its device tokens and
	// session cookies stay rows in the token table, dead today only because
	// userForToken also has to resolve the account — so an id that ever came
	// back (a restore, a re-created account, a repo that reuses ids) would
	// resurrect every credential with it.
	a.revokeTokensForLocked(id)
	email := u.Email
	delete(a.users, id)
	a.mu.Unlock()
	// Outside the lock, and outside this provider: org roles, project grants
	// and share liveness all key on the address, not on the account id.
	if a.Offboard != nil {
		a.Offboard(email)
	}
	return nil
}

// Accounts returns every account, oldest first (used by the org migration
// to pick the default org's owner).
func (a *BuiltinAuth) Accounts() []User {
	a.mu.Lock()
	a.refresh()
	defer a.mu.Unlock()
	users := make([]*authUser, 0, len(a.users))
	for _, u := range a.users {
		if u.active() {
			users = append(users, u)
		}
	}
	sortByAge(users)
	out := make([]User, len(users))
	for i, u := range users {
		out[i] = User{ID: u.ID, Email: u.Email, Name: u.Name}
	}
	return out
}

// sortByAge is the one "oldest first" order, and it is a TOTAL order.
//
// It used to be `sort.Slice` on Created alone, over a slice built by ranging a
// map. Created arrived as a column after the fact, so on every upgraded hub
// every row ties at the zero time, an unstable sort over a random permutation
// is a random permutation, and the org heir — which reads this list — was
// therefore drawn by Go map iteration. The ID tiebreak makes the answer a fact
// about the store instead of a fact about this process.
//
// A deterministic order is not the same as evidence of age. Anything that
// needs the latter asks Seniority.
func sortByAge(users []*authUser) {
	sort.SliceStable(users, func(i, j int) bool {
		if !users[i].Created.Equal(users[j].Created) {
			return users[i].Created.Before(users[j].Created)
		}
		return users[i].ID < users[j].ID
	})
}

// Seniority is the oldest-first account order, and it is EMPTY when this hub
// holds no evidence of age at all.
//
// OrgDB.heir breaks a Joined tie on it, and its own doc comment says "with no
// seniority available there is NO evidence, and the answer is nobody: an
// ownerless org is a repair a hub admin makes deliberately, while an arbitrary
// heir is a privilege grant nobody asked for". Handing back a merely
// deterministic order would satisfy the letter and not the sentence — it would
// promote the same arbitrary member every time, which is round 8's finding
// with a different arbitrary key. Rows with no Created stamp are dropped, so
// an upgraded hub that recorded nothing says nothing.
func (a *BuiltinAuth) Seniority() []string {
	a.mu.Lock()
	a.refresh()
	defer a.mu.Unlock()
	dated := make([]*authUser, 0, len(a.users))
	for _, u := range a.users {
		if u.active() && !u.Created.IsZero() {
			dated = append(dated, u)
		}
	}
	sortByAge(dated)
	out := make([]string, len(dated))
	for i, u := range dated {
		out[i] = u.Email
	}
	return out
}

// ---- AuthProvider ----

const sessionCookie = "bdrive_session"

func (a *BuiltinAuth) CLILoginPath() string { return "/auth/cli" }

// UseDeviceBinder installs the hub's binding hook. finishLogin — the one place
// this provider mints a CLI token, reached by all three flows — calls it before
// it issues anything.
func (a *BuiltinAuth) UseDeviceBinder(bind DeviceBinder) { a.BindDevice = bind }

func (a *BuiltinAuth) Authenticate(r *http.Request) (User, bool) {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return a.userForToken(strings.TrimPrefix(h, "Bearer "))
	}
	if c, err := r.Cookie(sessionCookie); err == nil {
		return a.userForToken(c.Value)
	}
	return User{}, false
}

func (a *BuiltinAuth) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /auth/login", a.pageLogin)
	mux.HandleFunc("POST /auth/login", a.pageLogin)
	mux.HandleFunc("GET /auth/signup", a.pageSignup)
	mux.HandleFunc("POST /auth/signup", a.pageSignup)
	mux.HandleFunc("GET /auth/logout", a.pageLogout)
	mux.HandleFunc("GET /auth/verify", a.pageVerify)
	mux.HandleFunc("GET /auth/reset", a.pageReset)
	mux.HandleFunc("POST /auth/reset", a.pageReset)
	mux.HandleFunc("GET /auth/reset/confirm", a.pageResetConfirm)
	mux.HandleFunc("POST /auth/reset/confirm", a.pageResetConfirm)
	mux.HandleFunc("GET /api/auth/me", a.apiMe)
	mux.HandleFunc("DELETE /api/auth/token", a.apiRevokeToken)
	a.cli.Register(mux)
}

// apiRevokeToken ends the credential the request presents — `bdrive logout` on
// the wire. The token authenticates its own revocation, so no other permission
// is involved and nothing else can be revoked with it.
//
// Device tokens have no expiry, so without this the documented way to sign a
// device out ("no longer authenticated to the bdrive server") only rewrote a
// local file: a lost laptop or a leaked token could be answered only by
// resetting the account's password hub-wide. The browser half already ended
// its session server-side.
func (a *BuiltinAuth) apiRevokeToken(w http.ResponseWriter, r *http.Request) {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Bearer ") {
		http.Error(w, "a device token is required", http.StatusUnauthorized)
		return
	}
	tok := strings.TrimPrefix(h, "Bearer ")
	if _, ok := a.userForToken(tok); !ok {
		http.Error(w, "invalid or expired token", http.StatusUnauthorized)
		return
	}
	if err := a.revokeToken(tok); err != nil {
		// Reported, never swallowed: a revocation that only happened in
		// memory is a credential that comes back at the next restart.
		http.Error(w, "could not revoke the token", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// sessionUser resolves the browser session (cookie only, not Bearer).
func (a *BuiltinAuth) sessionUser(r *http.Request) (User, bool) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		return a.userForToken(c.Value)
	}
	return User{}, false
}

// cliSignIn reports whether a next URL is a pending CLI sign-in.
func cliSignIn(next string) bool { return strings.HasPrefix(next, "/auth/cli?") }

func (a *BuiltinAuth) startSession(w http.ResponseWriter, userID string) error {
	tok, err := a.issueToken(userID, "web-session")
	if err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: tok, Path: "/",
		HttpOnly: true, SameSite: http.SameSiteLaxMode,
	})
	return nil
}

// inviteBanner shows an invitation cue when the post-login destination is a
// join link, so a visitor who clicked an invite knows why they're here.
func inviteBanner(next string) string {
	if !strings.Contains(next, "join/") && !strings.Contains(next, "join%2F") {
		return ""
	}
	return `<p class="msg" style="margin:0 0 14px">You've been invited to a team. Sign in (or sign up) to accept.</p>`
}

// cliBanner says what a sign-in reached from `bdrive login` is for, so the form
// is not a bare password prompt appearing for no visible reason. Approving is
// still its own step on the next page — this only explains why signing in is
// being asked for at all.
func cliBanner(next string) string {
	if !cliSignIn(next) {
		return ""
	}
	return `<p class="msg" style="margin:0 0 14px">A terminal on this computer is waiting to sign in. ` +
		`The account you use here is the one it will act as.</p>`
}

// safeNext keeps post-login redirects on this site. A browser fills in the
// authority slot for anything that looks like "//host", and it does that
// AFTER stripping tab/CR/LF and after treating a backslash as a separator —
// so "/\evil.example", "/\t/evil.example" and "//evil.example" are all the
// same off-site jump, arriving straight from the page where the user just
// typed their password. Strip what a browser strips, then demand a single
// leading slash followed by neither.
func safeNext(next string) string {
	next = strings.Map(func(r rune) rune {
		if r == '\t' || r == '\r' || r == '\n' {
			return -1
		}
		return r
	}, next)
	if len(next) < 1 || next[0] != '/' {
		return "/"
	}
	if len(next) > 1 && (next[1] == '/' || next[1] == '\\') {
		return "/"
	}
	return next
}

// inviteTokenFromNext pulls an org-invite token out of a post-login target
// like "/join/<token>". Tokens are lowercase hex.
//
// `next` must BE the join route, not merely contain it. This used to be
// strings.Index anywhere in the string, and what it unlocks is not a redirect:
// a live token found here makes pageSignup offer account creation on a hub with
// self-signup CLOSED, and signupInvited then skips the domain allowlist,
// verification and approval and activates the account outright. So
// "/wiki/note.md?x=/join/<tok>" bought the whole invite-only bypass while
// routing somewhere that never redeems anything — the account landed active, in
// no member roster, with the invite still reading unused. "The invite is the
// vetting" only holds if the invite is also spent, and it can only be spent by
// arriving at /join/.
func inviteTokenFromNext(next string) string {
	const marker = "/join/"
	// A project-scoped invite is "/join/<tok>?p=<project-id>", so the query
	// is cut BEFORE the prefix check: it is not part of the route the token
	// has to BE, and cutting at the FIRST "?" keeps every negative below
	// closed — "/wiki/note.md?x=/join/<tok>" cuts to "/wiki/note.md" and
	// still fails the prefix.
	next, _, _ = strings.Cut(next, "?")
	if !strings.HasPrefix(next, marker) {
		return ""
	}
	// The whole remainder, with nothing after it: "/join/<tok>/../.." is a
	// target a browser resolves to "/", so it unlocks the signup and redeems
	// nothing, exactly like burying the token in a query.
	tok := next[len(marker):]
	if tok == "" {
		return ""
	}
	for _, c := range tok {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return ""
		}
	}
	return tok
}

// invitedVia returns the invite token in next when it points at a live invite,
// so the login/signup pages can let an invitee create an account even on an
// invite-only hub. Empty string when there's no valid invite.
func (a *BuiltinAuth) invitedVia(next string) string {
	tok := inviteTokenFromNext(next)
	if tok != "" && a.InviteValid != nil && a.InviteValid(tok) {
		return tok
	}
	return ""
}

// ---- pages ----

func authPage(w http.ResponseWriter, title, body string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Every page this renders is part of the credential surface: the reset
	// form echoes a single-use grant into its own body, and the two approval
	// pages name the signed-in account and hand a machine a token acting as
	// it. Nothing here may be stored by a browser disk cache or a shared
	// forward proxy, and nothing here may be framed — the app shell next door
	// has answered both questions since round 3 (server.go), and these
	// handlers were simply never asked.
	w.Header().Set("Cache-Control", "no-store, private")
	w.Header().Set("Vary", "Cookie")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", "frame-ancestors 'none'")
	fmt.Fprintf(w, `<!doctype html><html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1"><title>%s — BearDrive</title>
<style>
/* The app's tokens, name for name, so sign-in and the app read as one
   product. Source of truth: frontend/src/tw.css @theme — keep the values
   here identical to the token of the same name there. */
:root{--bg:#0a0b0d;--raise:#15171b;--surface:rgba(255,255,255,.03);--hovered:rgba(255,255,255,.06);
--line:rgba(255,255,255,.07);--line-2:rgba(255,255,255,.11);--text:#eef0f3;--dim:#9aa0a9;--faint:#868b93;
--honey:#f5a623;--honey-bright:#ffcf85;--on-honey:#1a1204;--add:#4cc38a;--del:#f26d6d;
--radius-ctl:7px;--radius-over:14px;
--mono:ui-monospace,"SF Mono","JetBrains Mono",Menlo,Consolas,monospace}
body{font:14px/1.5 -apple-system,BlinkMacSystemFont,"SF Pro Text","Inter","Segoe UI",Roboto,sans-serif;
background:var(--bg);color:var(--text);display:flex;justify-content:center;padding:13vh 16px;margin:0;
letter-spacing:-.006em;-webkit-font-smoothing:antialiased}
.card{background:var(--raise);border:1px solid var(--line);border-radius:var(--radius-over);padding:28px 30px;
width:344px;max-width:100%%;box-sizing:border-box;box-shadow:0 24px 70px -24px rgba(0,0,0,.7)}
.logo{width:30px;height:30px;display:grid;place-items:center;color:var(--honey);margin-bottom:16px}
.logo svg{width:30px;height:30px;fill:currentColor}
h1{font-size:18px;font-weight:640;letter-spacing:-.02em;margin:0 0 18px}
label{display:block;font-size:12px;color:var(--dim);margin:14px 0 5px;font-weight:500}
input{width:100%%;box-sizing:border-box;height:38px;padding:0 12px;border-radius:var(--radius-ctl);
border:1px solid var(--line-2);background:var(--surface);color:var(--text);font:inherit;font-size:14px;outline:none}
input:focus-visible{outline:2px solid var(--honey);outline-offset:1px;border-color:var(--honey)}
button{margin-top:20px;width:100%%;height:40px;border:none;border-radius:var(--radius-ctl);background:var(--honey);
color:var(--on-honey);font:inherit;font-size:14px;font-weight:600;cursor:pointer}
button:hover{background:var(--honey-bright)}
button:focus-visible{outline:2px solid var(--honey-bright);outline-offset:2px}
.err{color:var(--del);font-size:13px;margin:12px 0 0}
.msg{color:var(--add);font-size:13px;margin:12px 0 0}
.lede{margin:0;color:var(--dim);font-size:13px}
.alt{margin-top:16px;font-size:12.5px;color:var(--faint)}
.alt a{color:var(--honey-bright);text-decoration:none}
.alt a:hover{text-decoration:underline}
/* Device approval: who you'd be granting as, then what is asking. */
.who{display:flex;align-items:center;gap:12px;justify-content:space-between;margin:16px 0 4px;
padding:12px 14px;border:1px solid var(--line);border-radius:var(--radius-ctl);background:var(--surface)}
.who-id{min-width:0}
.who-l{display:block;font-size:11px;text-transform:uppercase;letter-spacing:.04em;color:var(--faint)}
.who-id b{display:block;font-weight:600;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.who-sub{display:block;font-size:12px;color:var(--dim);overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.who-swap{flex:none;font-size:12.5px;color:var(--honey-bright);text-decoration:none}
.who-swap:hover{text-decoration:underline}
.rows{display:grid;grid-template-columns:auto 1fr;gap:6px 14px;margin:14px 0 0;font-size:13px}
.rows dt{color:var(--faint)}
/* Every value in this list is chosen by the unauthenticated stranger asking
   to be approved, and this is the one page a human reads before a device
   credential is minted. trimText + html.EscapeString stop markup and the
   invisible classes; neither stops a strong-RTL LETTER (category Lo) from
   repainting the row out of order — "laptop-7א (unverified)" reads as
   "laptop-7 )unverified(א". isolate-override, not isolate: measured in
   Chromium, isolate leaves the reordering intact. Same rule as the SPA's
   peer-written names (frontend/src/style.css). */
.rows dd{margin:0;font-family:var(--mono);font-size:12.5px;overflow-wrap:anywhere;unicode-bidi:isolate-override;direction:ltr}
@media (max-width:900px){input{height:44px}button{height:44px}}
code{background:var(--hovered);border:1px solid var(--line);padding:2px 6px;border-radius:5px;
font-family:var(--mono)}
</style></head><body><div class="card"><div class="logo"><svg viewBox="0 0 32 32" role="img" aria-label="BearDrive">`+
		`<rect x="4" y="4" width="5.6" height="24"/><rect x="11.2" y="4" width="14.4" height="11.2"/>`+
		`<rect x="11.2" y="16.8" width="16.8" height="11.2"/></svg></div><h1>%s</h1>%s</div></body></html>`,
		html.EscapeString(title), html.EscapeString(title), body)
}

func field(label, name, typ, value string) string {
	return fmt.Sprintf(`<label for="f-%s">%s</label><input id="f-%s" name=%q type=%q value=%q autocomplete=%q required>`,
		name, html.EscapeString(label), name, name, typ, html.EscapeString(value), autocompleteFor(name, typ))
}

// autocompleteFor names a field's purpose so browsers and password managers
// fill it (WCAG 1.3.5). A password field's purpose depends on the page, not
// its name: offering a saved credential on a signup form is worse than
// offering nothing, so those callers use newPasswordField.
func autocompleteFor(name, typ string) string {
	switch name {
	case "email":
		return "email"
	case "name":
		return "name"
	case "code":
		return "one-time-code"
	}
	if typ == "password" {
		return "current-password"
	}
	return "on"
}

// newPasswordField is field() for a password being CREATED (signup, reset),
// so a manager generates one instead of filling an existing login.
func newPasswordField(label, name string) string {
	return fmt.Sprintf(`<label for="f-%s">%s</label><input id="f-%s" name=%q type="password" autocomplete="new-password" required>`,
		name, html.EscapeString(label), name, name)
}

func (a *BuiltinAuth) pageLogin(w http.ResponseWriter, r *http.Request) {
	next := safeNext(r.FormValue("next"))
	var errMsg string
	if r.Method == http.MethodPost {
		if u := a.verifyPassword(r.FormValue("email"), r.FormValue("password")); u != nil {
			switch u.Status {
			case statusUnverified:
				a.sendVerification(u)
				errMsg = `<p class="err">Please verify your email first — we've re-sent the link.</p>`
			case statusPending:
				errMsg = `<p class="err">Your account is still awaiting administrator approval.</p>`
			default:
				if err := a.startSession(w, u.ID); err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
				http.Redirect(w, r, next, http.StatusSeeOther)
				return
			}
		} else {
			errMsg = `<p class="err">Wrong email or password.</p>`
		}
	}
	// Offer account creation when public signup is open, or when the visitor
	// arrived through a valid invite (the way into an invite-only hub).
	signup := ""
	invited := a.invitedVia(next) != ""
	if a.AllowSignup || invited {
		note := ""
		if a.AllowSignup && len(a.AllowedDomains) > 0 {
			note = ` <span style="color:var(--faint)">(` + html.EscapeString(a.domainList()) + ` only)</span>`
		}
		label := "No account?"
		if invited && !a.AllowSignup {
			label = "New here?"
		}
		signup = fmt.Sprintf(`<p class="alt">%s <a href="/auth/signup?next=%s">Sign up</a>%s</p>`, label, url.QueryEscape(next), note)
	}
	brand := ""
	if a.Brand != "" {
		brand = `<p class="alt" style="margin:0 0 14px;color:var(--dim)">` + html.EscapeString(a.Brand) + `</p>`
	}
	authPage(w, "Sign in", brand+inviteBanner(next)+cliBanner(next)+fmt.Sprintf(`<form method="post" action="/auth/login?next=%s">%s%s%s<button type="submit">Sign in</button></form>
%s<p class="alt"><a href="/auth/reset">Forgot password?</a></p>`,
		url.QueryEscape(next),
		field("Email", "email", "email", r.FormValue("email")),
		field("Password", "password", "password", ""),
		errMsg, signup))
}

func (a *BuiltinAuth) pageSignup(w http.ResponseWriter, r *http.Request) {
	next := safeNext(r.FormValue("next"))
	// An invite link authorizes account creation even when public self-signup
	// is closed — it's the only way into an invite-only hub. Except once: a
	// brand-new hub has no accounts to mint an invite with, so until the
	// first account exists, the emails on the config's admin list may sign
	// up directly (the operator wrote them there — that's the vetting).
	inviteTok := a.invitedVia(next)
	bootstrap := !a.AllowSignup && inviteTok == "" && len(a.Admins) > 0 && a.accountCount() == 0
	if !a.AllowSignup && inviteTok == "" && !bootstrap {
		authPage(w, "Sign up disabled", `<p>This server is invite-only. Ask a team owner for an invite link, or sign in if you already have an account.</p>
<p class="alt"><a href="/auth/login">Back to sign in</a></p>`)
		return
	}
	var errMsg string
	if r.Method == http.MethodPost {
		signup := a.signup
		if inviteTok != "" {
			signup = a.signupInvited // invite is the vetting: skip gates, activate
		}
		if bootstrap && !a.isAdmin(r.FormValue("email")) {
			signup = func(string, string, string) (*authUser, error) {
				return nil, fmt.Errorf("this server is invite-only; only a hub admin can create the first account")
			}
		}
		u, err := signup(r.FormValue("email"), r.FormValue("name"), r.FormValue("password"))
		if err == nil {
			switch u.Status {
			case statusUnverified:
				a.sendVerification(u)
				authPage(w, "Verify your email", `<p class="msg">Almost there — we sent a verification link to <b>`+
					html.EscapeString(u.Email)+`</b>.</p><p class="alt">Click it to activate your account. No email on this server? The link is in the server log.</p>`)
				return
			case statusPending:
				authPage(w, "Awaiting approval", `<p class="msg">Thanks — your account was created and is waiting for an administrator to approve it.</p>
<p class="alt">You'll be able to sign in once it's approved.</p>`)
				return
			}
			if err := a.startSession(w, u.ID); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			http.Redirect(w, r, next, http.StatusSeeOther)
			return
		}
		errMsg = `<p class="err">` + html.EscapeString(err.Error()) + `</p>`
	}
	// State the domain restriction up front, where the stranger types their
	// email — not only after a rejected submit. An invite bypasses the domain
	// allowlist, so don't show it when arriving through one.
	domainNote := ""
	if inviteTok == "" && len(a.AllowedDomains) > 0 {
		domainNote = `<p class="alt" style="margin:2px 0 0">Only ` + html.EscapeString(a.domainList()) + ` email addresses can sign up here.</p>`
	}
	brand := ""
	if a.Brand != "" {
		brand = `<p class="alt" style="margin:0 0 14px;color:var(--dim)">` + html.EscapeString(a.Brand) + `</p>`
	}
	authPage(w, "Create account", brand+inviteBanner(next)+cliBanner(next)+fmt.Sprintf(`<form method="post" action="/auth/signup?next=%s">%s%s%s%s%s<button type="submit">Sign up</button></form>
<p class="alt">Have an account? <a href="/auth/login?next=%s">Sign in</a></p>`,
		url.QueryEscape(next),
		field("Name", "name", "text", r.FormValue("name")),
		field("Email", "email", "email", r.FormValue("email")),
		domainNote,
		newPasswordField("Password (min 8 chars)", "password"),
		errMsg, url.QueryEscape(next)))
}

// pageLogout ends the browser session. It honors ?next= so "switch account"
// on a page that needed one (device approval, an invite) lands back there as
// the new account instead of dumping the visitor at the hub root.
func (a *BuiltinAuth) pageLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		a.revokeToken(c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1})
	dest := "/auth/login"
	if next := safeNext(r.FormValue("next")); next != "/" {
		dest += "?next=" + url.QueryEscape(next)
	}
	http.Redirect(w, r, dest, http.StatusSeeOther)
}

// pageVerify activates an account from an email link, then either starts a
// session (or explains it's now awaiting approval).
func (a *BuiltinAuth) pageVerify(w http.ResponseWriter, r *http.Request) {
	g, ok := a.takeGrant("verify", r.URL.Query().Get("token"))
	if !ok {
		authPage(w, "Link expired", `<p class="err">This verification link is invalid or expired.</p>
<p class="alt"><a href="/auth/login">Back to sign in</a></p>`)
		return
	}
	a.mu.Lock()
	a.refresh()
	u := a.users[g.user]
	next := a.afterVerify()
	var perr error
	if u != nil && u.Status == statusUnverified {
		// Persist first: a verification the store refused must not activate
		// the account in memory. Same shape as Approve.
		row := *u
		row.Status = next
		if perr = a.store.PutAccount(&row); perr == nil {
			u.Status = next
		}
	}
	a.mu.Unlock()
	if perr != nil {
		authPage(w, "Not verified yet", `<p class="err">Your account could not be updated, so it is not verified yet.</p>
<p class="alt"><a href="/auth/login">Back to sign in</a></p>`)
		return
	}
	if u != nil && u.Status == statusPending {
		authPage(w, "Email verified", `<p class="msg">Your email is verified. Your account is now waiting for an administrator to approve it.</p>`)
		return
	}
	if u != nil {
		if err := a.startSession(w, u.ID); err == nil {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
	}
	authPage(w, "Email verified", `<p class="msg">Your email is verified.</p><p class="alt"><a href="/auth/login">Sign in</a></p>`)
}

func (a *BuiltinAuth) pageReset(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		email := strings.TrimSpace(strings.ToLower(r.FormValue("email")))
		a.mu.Lock()
		a.refresh()
		u := a.findByEmail(email)
		a.mu.Unlock()
		if u != nil {
			tok := a.newGrant("reset", u.ID, time.Hour)
			addr := u.Email
			link := a.mailBaseURL() + "/auth/reset/confirm?token=" + tok
			subject := "Reset your BearDrive password"
			body := "Someone (hopefully you) asked to reset the BearDrive password for " + addr +
				".\n\nReset it here (valid for 1 hour):\n\n  " + link + "\n\nIf this wasn't you, ignore this email."
			// Sent off the request path. Mail is the one step whose cost
			// depends on whether the address exists, so a handler that waits
			// for it answers a known address in SMTP-round-trip time and an
			// unknown one instantly — an account-enumeration oracle that needs
			// no statistics, just one slow mail server. Nothing is lost by not
			// waiting: a delivery failure was never surfaced to the caller
			// anyway, only logged.
			go func() {
				if err := a.Mail.Send(addr, subject, body); err != nil {
					// Never break reset: the admin can hand over the logged link.
					fmt.Printf("password reset for %s (email not sent: %v):\n  %s\n", addr, err, link)
				}
			}()
		}
		authPage(w, "Check your email", `<p class="msg">If that account exists, a reset link is on its way.</p>
<p class="alt">No email configured on this server? The link is in the server log.</p>`)
		return
	}
	authPage(w, "Reset password", fmt.Sprintf(`<form method="post">%s<button type="submit">Send reset link</button></form>
<p class="alt"><a href="/auth/login">Back to sign in</a></p>`,
		field("Email", "email", "email", "")))
}

func (a *BuiltinAuth) pageResetConfirm(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		tok, password := r.FormValue("token"), r.FormValue("password")
		if len(password) < 8 {
			authPage(w, "Set a new password", resetForm(tok, `<p class="err">Password must be at least 8 characters.</p>`))
			return
		}
		g, ok := a.takeGrant("reset", tok)
		if !ok {
			authPage(w, "Link expired", `<p class="err">This reset link is invalid or expired.</p>
<p class="alt"><a href="/auth/reset">Request a new one</a></p>`)
			return
		}
		hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		// Persist first, apply after, and surface a refusal: round 5 made the
		// TOKEN half of a reset durable and left this half discarding the
		// store's error, so the page said "Password updated" while the hub
		// came back at the next restart with the password the thief chose.
		a.mu.Lock()
		a.refresh()
		u := a.users[g.user]
		var perr error
		if u != nil {
			next := *u
			next.Pass = string(hash)
			if perr = a.store.PutAccount(&next); perr == nil {
				u.Pass = string(hash)
			}
		}
		a.mu.Unlock()
		if perr != nil {
			authPage(w, "Password not changed", `<p class="err">Your password could not be saved, so nothing was changed.</p>
<p class="alt"><a href="/auth/reset">Request a new reset link</a></p>`)
			return
		}
		// A reset is the documented recovery for a stolen account, so it has
		// to end the thief's access too: every session cookie and device token
		// minted under the old password dies with it.
		a.revokeTokensFor(g.user)
		authPage(w, "Password updated", `<p class="msg">Your password is updated.</p>
<p class="alt"><a href="/auth/login">Sign in</a></p>`)
		return
	}
	authPage(w, "Set a new password", resetForm(r.URL.Query().Get("token"), ""))
}

func resetForm(token, msg string) string {
	return fmt.Sprintf(`<form method="post"><input type="hidden" name="token" value=%q>%s%s<button type="submit">Set password</button></form>`,
		html.EscapeString(token), newPasswordField("New password (min 8 chars)", "password"), msg)
}

func requestBaseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// mailBaseURL is where a link the hub SENDS SOMEWHERE ELSE points. It is
// deliberately not requestBaseURL: the Host header (and X-Forwarded-Proto) are
// chosen by whoever made the request, and /auth/reset is unauthenticated — so
// a stranger posting a victim's address with a Host of their choosing had the
// hub mail the victim a genuine link that hands the single-use grant to the
// attacker's server. Classic reset poisoning. The three other requestBaseURL
// callers hand the URL back to the caller who chose the host, which is
// self-inflicted; these two do not.
//
// A configured origin (auth.base_url) is the only trustworthy answer, and it
// is now required whenever smtp is configured (ValidateSignupPolicy). Two
// weaker rules were tried and both were the same hole in a different shape:
// using the request's own host aims the victim's link wherever the requester
// says, and pinning the first host seen only moves the choice to whoever mails
// first — on a fresh process that is one anonymous POST, which both picks the
// origin and picks who receives it.
//
// So with no configured origin the hub has nothing it can trust and says so:
// the link goes out root-relative (usable by hand, and the log names the
// config that fixes it) rather than aimed somewhere a stranger picked.
func (a *BuiltinAuth) mailBaseURL() string {
	if a.BaseURL != "" {
		return strings.TrimRight(a.BaseURL, "/")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.warnedBase {
		a.warnedBase = true
		log.Print("beardrive: mailed links are root-relative because auth.base_url is not set; " +
			"configure it with this hub's public origin so reset and verification links are usable")
	}
	return ""
}

// ---- CLI API ----

func (a *BuiltinAuth) finishLogin(w http.ResponseWriter, r *http.Request, userID, device string) {
	if device == "" {
		device = "cli"
	}
	a.mu.Lock()
	a.refresh()
	u := a.users[userID]
	a.mu.Unlock()
	if u == nil {
		http.Error(w, "unknown user", http.StatusUnauthorized)
		return
	}
	// Minting the token is where a device identity is BOUND to an account, and
	// it is the only place a binding is created. Every mint point routes
	// through here — the loopback browser flow, the device-code flow, and the
	// login `bdrive init` runs inside itself — so binding here covers all three
	// rather than the one a fix would otherwise name.
	//
	// Before the token, not after: a login that cannot bind must not hand back
	// a credential that then cannot push. 409 is the honest status — the id is
	// taken, and the message says what to do about it.
	if a.BindDevice != nil {
		if err := a.BindDevice(u.Email, r); err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
	}
	tok, err := a.issueToken(userID, device)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{
		"token": tok,
		"user":  User{ID: u.ID, Email: u.Email, Name: u.Name},
	})
}

func (a *BuiltinAuth) apiMe(w http.ResponseWriter, r *http.Request) {
	u, ok := a.Authenticate(r)
	if !ok {
		http.Error(w, "not signed in", http.StatusUnauthorized)
		return
	}
	writeJSON(w, u)
}
