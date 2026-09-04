package webapp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// Round 12 — row 24 (the frontend application) and row 8 (orgs/invites) from
// the browser's side of the wire.
//
// Everything in this file is assertable from Go. The two findings that are
// NOT — the in-repo router's behavior on a path a browser will hand it —
// live in internal/webapp/frontend/e2e/sec12.spec.ts, because the router is
// TypeScript that only runs in a document. What this file adds for them is
// the half Go owns: proof that the server really does hand the SPA the byte
// sequences the router chokes on, so the browser test is attacking a
// reachable input and not a hypothetical one.

// sec12invite mints an invite for org as `who` and returns its token.
func sec12invite(t *testing.T, h http.Handler, org string, who *http.Cookie) string {
	t.Helper()
	rec := doAs(t, h, "POST", "/api/orgs/"+org+"/invites", nil, who)
	if rec.Code != 200 {
		t.Fatalf("mint invite: %d %s", rec.Code, rec.Body)
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.Token == "" {
		t.Fatalf("invite body %q: %v", rec.Body, err)
	}
	return out.Token
}

// sec12member reports whether `who` currently reaches the org's project.
func sec12member(t *testing.T, h http.Handler, project string, who *http.Cookie) int {
	t.Helper()
	return doAs(t, h, "GET", "/api/p/"+project+"/tree", nil, who).Code
}

/*
Removing a member is the action a hub operator takes when someone leaves.
An invite link is minted by an owner and lives on its own record, keyed only
by token — nothing at redemption asks whether the account that minted it is
still in the org, and nothing at removal revokes what the removed account
minted. So an owner who is removed can walk straight back in through the
link they created on their way out, with no current member involved.

The positive control in the same test proves the flow itself works: a
stranger redeeming a live invite from a CURRENT owner joins (200). The delta
is the removed owner's.
*/
func TestSec_Invite_ARemovedOwnerCannotRejoinWithTheInviteTheyMinted(t *testing.T) {
	h, _, c, p := permHub(t)

	// alice, the org owner, mints an invite before she is removed.
	tok := sec12invite(t, h, p.Org, c["alice"])

	// The flow works: dave (another org entirely) redeems it and joins.
	if rec := doAs(t, h, "POST", "/api/invites/"+tok, nil, c["dave"]); rec.Code != 200 {
		t.Fatalf("positive control: dave redeeming a live invite: %d %s", rec.Code, rec.Body)
	}
	if got := sec12member(t, h, p.ID, c["dave"]); got != 200 {
		t.Fatalf("positive control: dave should be in after joining, got %d", got)
	}

	// bob is promoted so the org still has an owner, then bob removes alice.
	if rec := doAs(t, h, "PATCH", "/api/orgs/"+p.Org+"/members/bob@x.io",
		map[string]string{"role": RoleOwner}, c["alice"]); rec.Code != 200 {
		t.Fatalf("promote bob: %d %s", rec.Code, rec.Body)
	}
	if rec := doAs(t, h, "DELETE", "/api/orgs/"+p.Org+"/members/alice@x.io", nil, c["bob"]); rec.Code != 200 {
		t.Fatalf("remove alice: %d %s", rec.Code, rec.Body)
	}
	// Removal really happened.
	if got := sec12member(t, h, p.ID, c["alice"]); got != http.StatusForbidden {
		t.Fatalf("after removal alice reads the project: %d, want 403", got)
	}

	// The attack: alice redeems the invite she minted herself.
	rec := doAs(t, h, "POST", "/api/invites/"+tok, nil, c["alice"])
	if rec.Code == 200 {
		t.Errorf("a removed owner rejoined with the invite she minted: POST /api/invites/<tok> = 200 %s", rec.Body)
	}
	if got := sec12member(t, h, p.ID, c["alice"]); got != http.StatusForbidden {
		t.Fatalf("a removed owner is back in the org: GET /api/p/%s/tree = %d, want 403", p.ID, got)
	}
}

/*
The same root cause pointed at a stranger rather than at the removed owner:
an invite outlives its creator's authority, so a link handed out by someone
the org has since ejected still onboards whoever holds it. No current member
of the org authorized that join, and no current member was asked.

Stated separately from the self-rejoin case because the two have different
answers available to a fixer: this one can also be closed by revoking a
removed member's invites, while the self-rejoin case additionally needs the
redeeming account checked.
*/
func TestSec_Invite_ARemovedOwnersLinkNoLongerOnboardsStrangers(t *testing.T) {
	h, _, c, p := permHub(t)

	tok := sec12invite(t, h, p.Org, c["alice"])

	if rec := doAs(t, h, "PATCH", "/api/orgs/"+p.Org+"/members/bob@x.io",
		map[string]string{"role": RoleOwner}, c["alice"]); rec.Code != 200 {
		t.Fatalf("promote bob: %d %s", rec.Code, rec.Body)
	}
	if rec := doAs(t, h, "DELETE", "/api/orgs/"+p.Org+"/members/alice@x.io", nil, c["bob"]); rec.Code != 200 {
		t.Fatalf("remove alice: %d %s", rec.Code, rec.Body)
	}

	rec := doAs(t, h, "POST", "/api/invites/"+tok, nil, c["dave"])
	if rec.Code == 200 {
		t.Errorf("a removed owner's link still onboards a stranger: %d %s", rec.Code, rec.Body)
	}
	if got := sec12member(t, h, p.ID, c["dave"]); got != http.StatusForbidden {
		t.Fatalf("stranger reached the org's project through a removed owner's link: %d, want 403", got)
	}
}

/*
Supporting evidence for the browser findings, and a boundary in its own
right: the SPA shell is what the hub serves for any non-asset path, so every
byte sequence a URL may legally carry is delivered to the in-repo router.

"%80" is a syntactically valid percent escape (Go's URL parser accepts it) but
not valid UTF-8, so decodeURIComponent throws URIError on it. The client half
of this pair is asserted in e2e/sec12.spec.ts. This half asserts only what Go
owns: the request is not rejected at the door, so the router really does see
the string.
*/
func TestSec_Router_TheShellIsServedForPathsTheClientMustSurvive(t *testing.T) {
	h, _, c, p := permHub(t)

	for _, path := range []string{
		"/" + p.ID + "/%80",                 // valid escape, invalid UTF-8
		"/" + p.ID + "/a%C0%80b",            // overlong encoding
		"/" + p.ID + "/dashboard/%ED%A0%80", // lone surrogate, under a view route
		"/" + p.ID + "/constructor",         // an Object.prototype member as a path segment
		"/" + p.ID + "/__proto__",
	} {
		req := httptest.NewRequest("GET", path, nil)
		req.AddCookie(c["alice"])
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("GET %s = %d, want 200 (the shell); the browser test's input would be unreachable", path, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "<div id=\"root\">") && !strings.Contains(rec.Body.String(), "id=root") {
			t.Fatalf("GET %s did not return the SPA shell: %.200s", path, rec.Body.String())
		}
	}
}

/*
Row 24, admin surfaces: OrgAdmin gates the invite list and the org-wide share
audit on `org.role === "owner"` in the CLIENT. A plain member who reaches the
same panel (the org route is a real URL, /orgs/<id>, and every member of the
org resolves it) must not be able to fetch the data the client is declining
to render. This asserts the server is the gate, not the component.
*/
func TestSec_OrgAdmin_AMemberWhoReachesThePanelStillCannotFetchOwnerData(t *testing.T) {
	h, _, c, p := permHub(t)

	// bob is a plain member: /api/orgs resolves the org for him, so the
	// panel renders. That is the reachability premise.
	rec := doAs(t, h, "GET", "/api/orgs", nil, c["bob"])
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), p.Org) {
		t.Fatalf("member cannot resolve the org route at all: %d %s", rec.Code, rec.Body)
	}
	// And it must tell him he is a member, not an owner — the client's
	// whole gate is this field.
	var orgs struct {
		Orgs []struct {
			ID   string `json:"id"`
			Role string `json:"role"`
		} `json:"orgs"`
	}
	json.Unmarshal(rec.Body.Bytes(), &orgs)
	found := false
	for _, o := range orgs.Orgs {
		if o.ID == p.Org {
			found = true
			if o.Role != RoleMember {
				t.Fatalf("plain member is told role=%q", o.Role)
			}
		}
	}
	if !found {
		t.Fatalf("org %s missing from a member's /api/orgs", p.Org)
	}

	// Everything the panel hides from him must 4xx, not merely be unrendered.
	for _, probe := range []struct {
		method, url string
	}{
		{"GET", "/api/orgs/" + p.Org + "/invites"},
		{"POST", "/api/orgs/" + p.Org + "/invites"},
		{"PATCH", "/api/orgs/" + p.Org},
		{"PATCH", "/api/orgs/" + p.Org + "/members/carol@x.io"},
		{"DELETE", "/api/orgs/" + p.Org + "/members/carol@x.io"},
	} {
		var body any
		if probe.method == "PATCH" {
			body = map[string]string{"name": "pwned", "role": RoleOwner}
		}
		rec := doAs(t, h, probe.method, probe.url, body, c["bob"])
		if rec.Code < 400 {
			t.Errorf("%s %s as a plain member = %d, want 4xx; body: %s",
				probe.method, probe.url, rec.Code, rec.Body)
		}
		if strings.Contains(rec.Body.String(), "/join/") {
			t.Errorf("%s %s leaked an invite URL to a plain member: %s", probe.method, probe.url, rec.Body)
		}
	}
}

/*
The org-wide share audit (GET /api/orgs/{org}/shares) is deliberately open to
every org member — the client hides it behind `owner`, the server does not,
and that is the documented intent. What must hold is the narrower rule the
handler states: a share row carries the public /s/ token, so a project the
member is denied must contribute no rows. This asserts that rule against a
member who has been set to `none` on the project after the share was minted.
*/
func TestSec_OrgShares_ADeniedProjectContributesNoTokensToTheOrgAudit(t *testing.T) {
	h, _, c, p := permHub(t)

	// alice publishes a file from her project.
	secauthzUpload(t, h, p.ID, "notes/secret.md", "# internal roadmap", c["alice"])
	rec := doAs(t, h, "POST", "/api/p/"+p.ID+"/shares",
		map[string]string{"path": "notes/secret.md"}, c["alice"])
	if rec.Code != 200 {
		t.Fatalf("mint share: %d %s", rec.Code, rec.Body)
	}
	var sh struct {
		Token string `json:"token"`
		URL   string `json:"url"`
	}
	json.Unmarshal(rec.Body.Bytes(), &sh)
	if sh.Token == "" {
		t.Fatalf("no share token in %s", rec.Body)
	}

	// Positive control: bob, a member with the default grant, sees it.
	rec = doAs(t, h, "GET", "/api/orgs/"+p.Org+"/shares", nil, c["bob"])
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), sh.Token) {
		t.Fatalf("control: a permitted member should see the audit row: %d %s", rec.Code, rec.Body)
	}

	// carol is denied the project outright.
	if rec := doAs(t, h, "PUT", "/api/p/"+p.ID+"/permissions/carol@x.io",
		map[string]string{"level": PermNone}, c["alice"]); rec.Code != 200 {
		t.Fatalf("deny carol: %d %s", rec.Code, rec.Body)
	}
	if got := sec12member(t, h, p.ID, c["carol"]); got != http.StatusForbidden {
		t.Fatalf("carol still reads the project: %d, want 403", got)
	}

	rec = doAs(t, h, "GET", "/api/orgs/"+p.Org+"/shares", nil, c["carol"])
	if strings.Contains(rec.Body.String(), sh.Token) {
		t.Errorf("the org share audit handed a denied member the public token of a project they cannot read: %s", rec.Body)
	}
}

/*
Row 24, the /auth pages as documents: pageSignup offers account creation on
an invite-only hub when `next` names a live invite, and signupInvited then
skips the domain / verification / approval gates. That is deliberate — the
invite is the vetting — so the attack is whether a signup that holds NO valid
invite can reach that path anyway: by naming a token that does not exist, one
that has been revoked, or by hiding "/join/" somewhere else in `next`.
*/
func TestSec_JoinPage_OnlyALiveInviteUnlocksSignupOnAClosedHub(t *testing.T) {
	h, srv, c, p := permHub(t)

	// The hub the fixture builds allows signup, which would mask the gate.
	// Close it, the way a default hub ships.
	auth, ok := srv.Auth.(*BuiltinAuth)
	if !ok {
		t.Fatal("fixture has no BuiltinAuth")
	}
	auth.AllowSignup = false
	// A served hub wires this (cmd/bdrive/web.go); permHub does not, and
	// without it the invite-signup path is unreachable and untestable.
	auth.InviteValid = srv.Dir.ValidInvite

	live := sec12invite(t, h, p.Org, c["alice"])
	revoked := sec12invite(t, h, p.Org, c["alice"])
	if rec := doAs(t, h, "DELETE", "/api/orgs/"+p.Org+"/invites/"+revoked, nil, c["alice"]); rec.Code != 200 {
		t.Fatalf("revoke: %d %s", rec.Code, rec.Body)
	}

	open := func(next string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/auth/signup?next="+next, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	offersSignup := func(rec *httptest.ResponseRecorder) bool {
		return strings.Contains(rec.Body.String(), `action="/auth/signup`)
	}

	// Positive control: a live invite really does unlock the form.
	if !offersSignup(open("%2Fjoin%2F" + live)) {
		t.Fatalf("a live invite does not unlock signup — the gate cannot be measured")
	}
	// A project-scoped invite ("/join/<tok>?p=<project-id>") is the same live
	// invite with a landing hint attached. The recipient who most needs the
	// form is exactly this one, and the query must not hide the token.
	if !offersSignup(open("%2Fjoin%2F" + live + "%3Fp%3D" + p.ID)) {
		t.Fatalf("a live project-scoped invite does not unlock signup")
	}

	for _, next := range []string{
		"%2Fjoin%2F" + revoked,                       // revoked
		"%2Fjoin%2Fdeadbeefdeadbeefdeadbeefdeadbeef", // forged
		"%2Fjoin%2F", // empty
		"%2Fwiki%2Fnote.md%3Fx%3D%2Fjoin%2F" + live, // a live token buried off the join route
		"%2Fjoin%2F" + strings.ToUpper(live),        // case-shifted
		"%2Fjoin%2F" + live + "%2F..%2F..",          // traversal after the token
	} {
		if rec := open(next); offersSignup(rec) {
			t.Errorf("signup form offered on a closed hub for next=%s", next)
		}
	}
}

/*
The consequence of the substring match above, spelled out so the finding is
not merely cosmetic: a holder of a live invite can create an account through
`next` values that never route to /join/, so the account is created ACTIVE on
an invite-only hub (signupInvited skips verification and approval) while the
org owner's ledger records nothing — the account is in no member list and the
invite still reads "unused". "The invite is the vetting" is only true if the
invite is also redeemed; here the vetting is spent and leaves no trace.
*/
func TestSec_JoinPage_AnInviteThatUnlocksSignupIsAlsoRedeemed(t *testing.T) {
	h, srv, c, p := permHub(t)
	auth, ok := srv.Auth.(*BuiltinAuth)
	if !ok {
		t.Fatal("fixture has no BuiltinAuth")
	}
	auth.AllowSignup = false
	auth.InviteValid = srv.Dir.ValidInvite

	tok := sec12invite(t, h, p.Org, c["alice"])

	// Sign up with the token buried off the join route.
	form := url.Values{
		"email": {"ghost@x.io"}, "name": {"Ghost"}, "password": {"password1"},
		"next": {"/wiki/note.md?x=/join/" + tok},
	}
	req := httptest.NewRequest("POST", "/auth/signup", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Skipf("signup did not complete (%d) — the unlock is already closed", rec.Code)
	}
	// It completed: the account exists and is active (a session was started).
	if len(rec.Result().Cookies()) == 0 {
		t.Fatalf("signup returned 303 with no session cookie: %v", rec.Header())
	}

	// Nothing in the org's ledger records it.
	orgRec := doAs(t, h, "GET", "/api/orgs/"+p.Org+"/invites", nil, c["alice"])
	var invs struct {
		Invites []struct {
			Token string `json:"token"`
			Uses  int    `json:"uses"`
		} `json:"invites"`
	}
	json.Unmarshal(orgRec.Body.Bytes(), &invs)
	for _, iv := range invs.Invites {
		if iv.Token == tok && iv.Uses == 0 {
			t.Errorf("an invite was spent to create an active account but still reads unused (uses=0): "+
				"the owner has no record of ghost@x.io; org members: %s",
				doAs(t, h, "GET", "/api/orgs", nil, c["alice"]).Body)
		}
	}
}

/*
BEA-170, the logged-out half of a project-scoped invite. A hub ships with
self-signup CLOSED, so the recipient who most needs the account-creation form
is the one arriving from "/join/<tok>?p=<pid>". Everything after the form is
the query surviving safeNext: the 303 has to carry "?p=" back to the join
route, or the newcomer creates an account and lands nowhere in particular —
which is the dead end the whole feature exists to remove.
*/
func TestJoin_ProjectScopedInvite_CarriesTheLoggedOutFlowEndToEnd(t *testing.T) {
	h, srv, c, p := permHub(t)
	auth, ok := srv.Auth.(*BuiltinAuth)
	if !ok {
		t.Fatal("fixture has no BuiltinAuth")
	}
	auth.AllowSignup = false
	auth.InviteValid = srv.Dir.ValidInvite

	tok := sec12invite(t, h, p.Org, c["alice"])
	next := "/join/" + tok + "?p=" + p.ID

	// 1. The form is offered at all.
	req := httptest.NewRequest("GET", "/auth/signup?next="+url.QueryEscape(next), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), `action="/auth/signup`) {
		t.Fatalf("no signup form for a project-scoped invite on a closed hub: %d", rec.Code)
	}

	// 2. Submitting it activates the account and sends the browser BACK to
	//    the join route with ?p= intact.
	form := url.Values{
		"email": {"newbie@x.io"}, "name": {"Newbie"}, "password": {"password1"},
		"next": {next},
	}
	req = httptest.NewRequest("POST", "/auth/signup", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("signup did not complete: %d %s", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Location"); got != next {
		t.Fatalf("post-signup redirect dropped the project hint: got %q want %q", got, next)
	}
	cookies := rec.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("signup returned 303 with no session cookie")
	}

	// 3. The join route itself serves the SPA shell, which redeems and then
	//    lands on /<p>/install. Nothing here 404s or bounces to login.
	req = httptest.NewRequest("GET", next, nil)
	for _, ck := range cookies {
		req.AddCookie(ck)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("join route with ?p= is not the app shell: %d %s", rec.Code, rec.Body)
	}
}
