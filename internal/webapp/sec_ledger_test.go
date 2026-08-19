package webapp

// Round 3. Three leads round 2 raised but could not close:
//
//  1. the reader-differencing oracle on /heat (row 10) — no single response
//     leaks an identity; does the ARITHMETIC across nested prefixes, day
//     windows and actor kinds name a specific reader?
//  2. the /s/* → ReadKindShare recording path (rows 7+10), never driven end to
//     end, plus the negative CLAUDE.md states: /store/* replication and
//     history /blob views are NEVER reads
//  3. q()'s ?→$N rewrite in db_sql.go (row 14) — Postgres is the only backend
//     where it is live and it has never run in any test
//
// Helpers are prefixed `secled` per the harness rules. Every test asserts the
// SECURE behavior.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// ---- helpers -------------------------------------------------------------

// secledReads turns a permHub into a read-telemetry hub.
func secledReads(t *testing.T, srv *Server) {
	t.Helper()
	var err error
	if srv.Reads, err = OpenReadLedger(filepath.Join(t.TempDir(), "reads.json"), 0); err != nil {
		t.Fatal(err)
	}
	if srv.Devices, err = OpenDeviceRegistry(filepath.Join(t.TempDir(), "devices.json")); err != nil {
		t.Fatal(err)
	}
}

// secledSeedFile puts real content into a project through the documented sync
// routes (blob first, then the journal op that names it), so the viewer, the
// share route and the history routes all serve a 200 rather than a 502. c must
// belong to an account with write on the project.
func secledSeedFile(t *testing.T, h http.Handler, c *http.Cookie, project, device, path, content string, seq int) string {
	t.Helper()
	secRegisterDevice(t, h, project, c, device, device, "linux")
	sum := sha256.Sum256([]byte(content))
	sha := hex.EncodeToString(sum[:])
	base := "/api/p/" + project + "/store/object?key="

	req := httptest.NewRequest("PUT", base+"blobs/"+sha, strings.NewReader(content))
	req.AddCookie(c)
	req.Header.Set("X-Bdrive-Device", device)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("seed blob %s: %d %s", path, rec.Code, rec.Body)
	}

	line := fmt.Sprintf(
		`{"seq":%d,"lamport":%d,"time":"2026-01-0%dT00:00:00Z","device":%q,`+
			`"kind":"put","path":%q,"blob":%q,"size":%d}`+"\n",
		seq, seq, seq, device, path, sha, len(content))
	req = httptest.NewRequest("PUT", base+"journal/"+device+".jsonl", strings.NewReader(line))
	req.AddCookie(c)
	req.Header.Set("X-Bdrive-Device", device)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("seed journal %s: %d %s", path, rec.Code, rec.Body)
	}
	return sha
}

// secledBucket writes one aggregation bucket straight into the ledger. Record
// always stamps today's date, so a day-window differencing attack needs a
// history the HTTP surface cannot produce.
func secledBucket(l *ReadLedger, project, path, day, kind, actor string, count int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	key := ReadStatKey{Project: project, Path: path, Day: day, Kind: kind, Actor: actor}
	l.byKey[key] = ReadStat{
		Project: project, Path: path, Day: day, Kind: kind, Actor: actor,
		Count: count,
		// A fixed instant: a wall-clock timestamp would differ between the two
		// worlds for reasons that have nothing to do with who read the file.
		Last: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	}
}

// secledBuckets snapshots every bucket in the ledger — the server-side truth
// the API is supposed to summarize without identifying anyone.
func secledBuckets(l *ReadLedger) []ReadStat {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]ReadStat, 0, len(l.byKey))
	for _, st := range l.byKey {
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		return a.Actor < b.Actor
	})
	return out
}

// secledNorm strips the fields that legitimately differ between two otherwise
// identical hubs (wall-clock stamps, the window echo) so a body comparison
// tests what the response SAYS, not when it was produced.
func secledNorm(t *testing.T, body []byte) string {
	t.Helper()
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		return "non-json:" + string(body)
	}
	var strip func(any) any
	strip = func(x any) any {
		switch n := x.(type) {
		case map[string]any:
			out := map[string]any{}
			for k, val := range n {
				if k == "last_read" || k == "since" {
					continue
				}
				out[k] = strip(val)
			}
			return out
		case []any:
			out := make([]any, len(n))
			for i, val := range n {
				out[i] = strip(val)
			}
			return out
		}
		return x
	}
	// encoding/json sorts map keys, so this is stable.
	out, err := json.Marshal(strip(v))
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// secledHeatShapes is the differencing corpus: every prefix nesting, every day
// window, and both actor-kind views a member can ask for.
func secledHeatShapes() []string {
	var out []string
	for _, by := range []string{"", "&by=device"} {
		for _, prefix := range []string{"", "hr", "hr/", "hr/eng", "hr/eng/", "eng", "notes"} {
			for _, days := range []string{"0", "1", "2", "3", "7", "30", "365", "3650"} {
				out = append(out, "?prefix="+prefix+"&days="+days+by)
			}
		}
	}
	return out
}

// ---- lead 1: the reader-differencing oracle on /heat ----------------------

// CLAUDE.md's guarantee for /api/p/<id>/heat is that it returns "counts /
// distinct-readers / last-read only — actor identities must never appear in an
// API response". Round 2 proved no single response contains an identity
// STRING. The open question is arithmetic: on a small team, can a member name
// WHO read a specific file by differencing Readers counts across nested
// prefixes, across day windows, and between the human/share/agent kinds?
//
// The strongest possible statement of the negative is indistinguishability:
// build two hubs identical in every way except WHICH teammate read the
// sensitive file, then let the attacker (bob, an ordinary member) ask every
// question the API accepts. If no query separates the two worlds, then no
// amount of differencing can name the reader — the oracle does not exist,
// rather than merely being hard to operate.
func TestSec_Heat_ReaderDifferencingCannotNameAReader(t *testing.T) {
	// world builds a hub where `reader` read hr/eng/payroll.md, with an
	// identical background of reads by everyone else.
	world := func(t *testing.T, reader string) map[string]string {
		t.Helper()
		h, srv, c, p := permHub(t)
		secledReads(t, srv)
		base := "/api/p/" + p.ID + "/"
		today := time.Now().UTC().Format("2006-01-02")
		yst := time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02")
		old := time.Now().UTC().AddDate(0, 0, -20).Format("2006-01-02")

		// Background identical in both worlds: bob reads everything, an agent
		// device and a share visitor leave their own marks, and there is real
		// multi-day history (Record can only stamp today).
		for _, path := range []string{"hr/eng/payroll.md", "hr/eng/notes.md", "hr/handbook.md", "top.md"} {
			secledBucket(srv.Reads, p.ID, path, today, ReadKindHuman, "bob@x.io", 3)
			secledBucket(srv.Reads, p.ID, path, yst, ReadKindHuman, "bob@x.io", 1)
			secledBucket(srv.Reads, p.ID, path, old, ReadKindHuman, "bob@x.io", 7)
			secledBucket(srv.Reads, p.ID, path, "", ReadKindHuman, "bob@x.io", 11)
			secledBucket(srv.Reads, p.ID, path, today, ReadKindShare, "tok-abc/198.51.100.9", 2)
			secledBucket(srv.Reads, p.ID, path, today, ReadKindAgent, "dev-shared", 5)
		}

		// The one difference: alice in one world, carol in the other, on the
		// sensitive file, across every day bucket a window can select.
		for _, day := range []string{today, yst, old, ""} {
			secledBucket(srv.Reads, p.ID, "hr/eng/payroll.md", day, ReadKindHuman, reader, 4)
		}

		// bob, the attacker, asks every question the API accepts.
		out := map[string]string{}
		for _, q := range secledHeatShapes() {
			rec := doAs(t, h, "GET", base+"heat"+q, nil, c["bob"])
			if rec.Code != 200 {
				t.Fatalf("GET heat%s: %d %s", q, rec.Code, rec.Body)
			}
			out[q] = secledNorm(t, rec.Body.Bytes())
		}
		return out
	}

	withAlice := world(t, "alice@x.io")
	withCarol := world(t, "carol@x.io")

	for _, q := range secledHeatShapes() {
		if withAlice[q] != withCarol[q] {
			t.Errorf("GET /heat%s distinguishes WHO read hr/eng/payroll.md —\n"+
				"  alice read it: %s\n  carol read it: %s",
				q, withAlice[q], withCarol[q])
		}
	}
}

// The differencing attack's other half: even when the attacker can move the
// population himself, the API must not turn a count into a name. bob reads the
// file, measures, and then tries to subtract himself out — across nested
// prefixes (does hr/ minus hr/eng/ isolate anyone?) and across day windows
// (does days=1 minus days=2 isolate yesterday's reader?).
//
// The invariant that makes all of that dead-end: Readers is computed per PATH,
// never per prefix and never per actor, and no response carries an actor list.
// So every difference bob can compute is a cardinality, and a cardinality
// names nobody. Asserted directly.
func TestSec_Heat_NestedPrefixAndDayWindowsCarryNoActorAxis(t *testing.T) {
	h, srv, c, p := permHub(t)
	secledReads(t, srv)
	base := "/api/p/" + p.ID + "/"
	today := time.Now().UTC().Format("2006-01-02")
	yst := time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02")

	// alice read it yesterday, carol today, bob both days.
	secledBucket(srv.Reads, p.ID, "hr/eng/payroll.md", yst, ReadKindHuman, "alice@x.io", 1)
	secledBucket(srv.Reads, p.ID, "hr/eng/payroll.md", today, ReadKindHuman, "carol@x.io", 1)
	secledBucket(srv.Reads, p.ID, "hr/eng/payroll.md", today, ReadKindHuman, "bob@x.io", 1)
	secledBucket(srv.Reads, p.ID, "hr/eng/payroll.md", yst, ReadKindHuman, "bob@x.io", 1)
	secledBucket(srv.Reads, p.ID, "hr/handbook.md", today, ReadKindHuman, "alice@x.io", 1)

	// The buckets the server is holding DO name all three — that is the point
	// of the attack: the identities exist, one API away.
	if got := len(secledBuckets(srv.Reads)); got != 5 {
		t.Fatalf("fixture: %d buckets, want 5", got)
	}

	members := []string{"alice@x.io", "carol@x.io", "bob@x.io", "alice", "carol"}
	for _, q := range secledHeatShapes() {
		rec := doAs(t, h, "GET", base+"heat"+q, nil, c["bob"])
		if rec.Code != 200 {
			t.Fatalf("GET heat%s: %d %s", q, rec.Code, rec.Body)
		}
		body := rec.Body.String()
		for _, m := range members {
			if strings.Contains(body, m) {
				t.Errorf("GET /heat%s names a reader (%q): %s", q, m, body)
			}
		}
		// No actor axis of any shape: the response must never grow a key that
		// enumerates readers, only count them.
		for _, k := range []string{`"actor"`, `"actors"`, `"reader_list"`, `"emails"`, `"users"`} {
			if strings.Contains(body, k) {
				t.Errorf("GET /heat%s carries an actor axis %s: %s", q, k, body)
			}
		}
	}
}

// ---- lead 2: /s/* → ReadKindShare, driven end to end ---------------------

// secledShare mints a share on a project through the real API.
func secledShare(t *testing.T, h http.Handler, c *http.Cookie, project, path string) string {
	t.Helper()
	rec := doAs(t, h, "POST", "/api/p/"+project+"/shares", map[string]string{"path": path}, c)
	if rec.Code != 200 {
		t.Fatalf("create share for %s: %d %s", path, rec.Code, rec.Body)
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Token == "" {
		t.Fatalf("share response carried no token: %s", rec.Body)
	}
	return out.Token
}

// secledGet issues an unauthenticated request from a chosen source address.
func secledGet(h http.Handler, target, remoteAddr string, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", target, nil)
	if remoteAddr != "" {
		req.RemoteAddr = remoteAddr
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// Round 2 seeded share buckets by calling Record directly. This drives the
// real link: an anonymous visitor fetches /s/<token> and the ledger must gain
// exactly one share-kind bucket, for the share's own project and path — and
// nothing human, nothing agent, and nothing that a member can then read back
// out of /heat.
func TestSec_Share_PublicHitRecordsShareKindEndToEnd(t *testing.T) {
	h, srv, c, p := permHub(t)
	secledReads(t, srv)
	secledSeedFile(t, h, c["alice"], p.ID, "dev1", "hr/eng/payroll.md", "# Payroll\n\nsecret", 1)
	token := secledShare(t, h, c["alice"], p.ID, "hr/eng/payroll.md")

	if got := secledBuckets(srv.Reads); len(got) != 0 {
		t.Fatalf("minting a share already recorded reads: %+v", got)
	}

	rec := secledGet(h, "/s/"+token, "203.0.113.7:5555", nil)
	if rec.Code != 200 {
		t.Fatalf("share visit: %d %s", rec.Code, rec.Body)
	}

	got := secledBuckets(srv.Reads)
	if len(got) != 1 {
		t.Fatalf("one share visit recorded %d buckets, want 1: %+v", len(got), got)
	}
	b := got[0]
	if b.Kind != ReadKindShare {
		t.Errorf("share visit recorded kind %q, want %q", b.Kind, ReadKindShare)
	}
	if b.Project != p.ID || b.Path != "hr/eng/payroll.md" {
		t.Errorf("share visit recorded (%s, %s), want (%s, hr/eng/payroll.md)", b.Project, b.Path, p.ID)
	}
	// token/ip/uahash (BEA-151): the browser component is what stops a whole
	// office behind one NAT from reading as one person. Asserted as a PREFIX
	// so the identity half — link plus network, never a name — stays pinned
	// without pinning the hash of an empty User-Agent.
	if want := token + "/203.0.113.7/"; !strings.HasPrefix(b.Actor, want) {
		t.Errorf("share actor = %q, want prefix %q", b.Actor, want)
	}

	// The member view: a share hit is share traffic, never a human reader.
	rec = doAs(t, h, "GET", "/api/p/"+p.ID+"/heat?days=0", nil, c["bob"])
	if rec.Code != 200 {
		t.Fatalf("heat: %d %s", rec.Code, rec.Body)
	}
	var heat struct {
		Entries map[string]HeatEntry `json:"entries"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &heat); err != nil {
		t.Fatal(err)
	}
	e := heat.Entries["hr/eng/payroll.md"]
	if e.Share != 1 || e.Human != 0 || e.Agent != 0 || e.Readers != 0 {
		t.Errorf("heat after one share visit = %+v, want share=1 and nothing else", e)
	}
	// The actor is a live bearer credential plus a visitor's IP. Neither may
	// reach a member through any heat view.
	for _, q := range []string{"?days=0", "?days=0&by=device", "?prefix=hr&days=0"} {
		rec := doAs(t, h, "GET", "/api/p/"+p.ID+"/heat"+q, nil, c["bob"])
		for _, leak := range []string{token, "203.0.113.7"} {
			if strings.Contains(rec.Body.String(), leak) {
				t.Errorf("GET /heat%s leaked the share actor %q: %s", q, leak, rec.Body)
			}
		}
	}
}

// CLAUDE.md: "/store/* replication and history /blob views are NEVER reads."
// Verified end to end rather than by reading the code — every sync route and
// every history route driven by a real member, then the delta proof that the
// ledger was wired up all along: one viewer fetch does record.
func TestSec_Ledger_ReplicationAndHistoryViewsAreNeverReads(t *testing.T) {
	h, srv, c, p := permHub(t)
	secledReads(t, srv)
	sha := secledSeedFile(t, h, c["alice"], p.ID, "dev1", "hr/eng/payroll.md", "# Payroll\n\nsecret", 1)
	base := "/api/p/" + p.ID + "/"

	// Replication: everything a syncing device does.
	for _, u := range []string{
		"store/list?prefix=journal/",
		"store/list?prefix=blobs/",
		"store/object?key=blobs/" + sha,
		"store/object?key=journal/dev1.jsonl",
		"store/exists?key=blobs/" + sha,
	} {
		if rec := doAs(t, h, "GET", base+u, nil, c["bob"]); rec.Code != 200 {
			t.Fatalf("GET %s: %d %s", u, rec.Code, rec.Body)
		}
	}
	secledSeedFile(t, h, c["bob"], p.ID, "dev2", "hr/eng/payroll.md", "# Payroll v2\n\nsecret", 2)

	// History: the change feed and any exact past version of a file.
	for _, u := range []string{
		"history",
		"history?path=hr/eng/payroll.md",
		"history?prefix=hr",
		"blob?sha=" + sha,
		"render?sha=" + sha + "&path=hr/eng/payroll.md",
	} {
		if rec := doAs(t, h, "GET", base+u, nil, c["bob"]); rec.Code != 200 {
			t.Fatalf("GET %s: %d %s", u, rec.Code, rec.Body)
		}
	}

	if got := secledBuckets(srv.Reads); len(got) != 0 {
		t.Errorf("replication and history views were recorded as reads: %+v", got)
	}

	// Delta proof: the same hub, the same member, one viewer fetch — this one
	// IS a read, so the empty result above is the server's decision and not a
	// ledger that was never connected.
	if rec := doAs(t, h, "GET", base+"file?path=hr/eng/payroll.md", nil, c["bob"]); rec.Code != 200 {
		t.Fatalf("viewer read: %d %s", rec.Code, rec.Body)
	}
	got := secledBuckets(srv.Reads)
	if len(got) != 1 || got[0].Kind != ReadKindHuman || got[0].Actor != "bob@x.io" {
		t.Fatalf("viewer read did not record a human bucket for bob: %+v", got)
	}
}

// Attacking the share entry point into the ledger. It is the only write path
// an UNAUTHENTICATED caller can reach, and round 2's fix for identity
// injection (ownsDevice) guards POST /reads, not this one. Three questions:
//
//   - can a visitor poison ANOTHER project's buckets?
//   - can the 10-minute visit debounce be defeated by varying something the
//     visitor controls — the query string, the method, the headers, the
//     casing of the token, X-Forwarded-For?
//   - can a visitor get an identity of its choosing recorded as an actor?
//
// User-Agent is the ONE exception, and it is deliberate (BEA-151): the actor
// key includes a hash of it, because without it three people in one office
// were one reader and the share panel reported "1 open" for all of them. So a
// visitor CAN split its own visits by rotating UAs — bounded by the per-IP
// limiter above the handler (ratelimit.go) and by retention folding, and it
// inflates only the count on a link the visitor already holds. Asserted below
// as intended behavior rather than left to look like a hole.
func TestSec_Share_VisitorCannotInflateOrRedirectTheLedger(t *testing.T) {
	h, srv, c, p := permHub(t)
	secledReads(t, srv)
	srv.ShareRPM = 100000 // the debounce is under test here, not the limiter
	secledSeedFile(t, h, c["alice"], p.ID, "dev1", "hr/eng/payroll.md", "# Payroll\n\nsecret", 1)
	token := secledShare(t, h, c["alice"], p.ID, "hr/eng/payroll.md")

	// A second project, in a different org, whose buckets must stay untouched.
	victim := secledProjectFor(t, h, c["dave"], "daves-notes")

	const addr = "203.0.113.7:5555"
	// Everything an anonymous visitor gets to choose, one hit each.
	variants := []struct {
		target string
		hdr    map[string]string
	}{
		{"/s/" + token, nil},
		{"/s/" + token + "?download=1", nil},
		{"/s/" + token + "?cachebust=1", nil},
		{"/s/" + token + "?cachebust=2", nil},
		{"/s/" + token + "?", nil},
		{"/s/" + token + "?download=1&x=" + strings.Repeat("y", 200), nil},
		{"/s/" + token, map[string]string{"X-Forwarded-For": "10.1.1.1"}},
		{"/s/" + token, map[string]string{"X-Forwarded-For": "10.1.1.2, 10.1.1.3"}},
		{"/s/" + token, map[string]string{"X-Real-IP": "10.2.2.2"}},
		{"/s/" + token, map[string]string{"X-Bdrive-Device": "alice@x.io"}},
		{"/s/" + token, map[string]string{"X-Bdrive-Device": "dev-alice", "X-Bdrive-Device-Name": "pwned"}},
		{"/s/" + token, map[string]string{"Cookie": "bdrive_session=whatever"}},
		{"/s/" + token, map[string]string{"Referer": "http://evil.example/"}},
	}
	served := 0
	for _, v := range variants {
		if rec := secledGet(h, v.target, addr, v.hdr); rec.Code == 200 {
			served++
		}
	}
	if served < len(variants) {
		t.Fatalf("harness: only %d/%d variants were served, the attack never ran", served, len(variants))
	}
	// Percent-encoded and case-shifted spellings of the same token: if any of
	// them resolves to the share, it must resolve to the SAME actor too.
	secledGet(h, "/s/"+strings.ToUpper(token), addr, nil)
	if len(token) > 2 {
		enc := fmt.Sprintf("%%%02x", token[0])
		secledGet(h, "/s/"+enc+token[1:], addr, nil)
	}

	got := secledBuckets(srv.Reads)
	if len(got) != 1 {
		t.Errorf("one visitor at one address produced %d ledger buckets — the 10-minute "+
			"visit debounce is defeated by something the visitor chooses: %+v", len(got), got)
	}

	// The documented exception: distinct browsers are distinct readers, so two
	// UAs are two buckets. That is the fix, not a defeat of the debounce — and
	// the actor still says only "this link, this network, some browser".
	for _, ua := range []string{"one", "two"} {
		secledGet(h, "/s/"+token, addr, map[string]string{"User-Agent": ua})
	}
	got = secledBuckets(srv.Reads)
	if len(got) != 3 {
		t.Errorf("two more browsers on one network produced %d buckets in total, want 3 — "+
			"distinct browsers must count separately (BEA-151): %+v", len(got), got)
	}
	for _, b := range got {
		if b.Project != p.ID {
			t.Errorf("a share visitor wrote a bucket for project %s, but the share is on %s: %+v",
				b.Project, p.ID, b)
		}
		if b.Kind != ReadKindShare {
			t.Errorf("an anonymous share visitor recorded a %s-kind bucket: %+v", b.Kind, b)
		}
		if !strings.HasPrefix(b.Actor, token+"/203.0.113.7/") {
			t.Errorf("share actor %q is not token/ip/browser — the visitor chose the identifying part of it: %+v", b.Actor, b)
		}
		for _, planted := range []string{"alice@x.io", "dev-alice", "pwned", "10.1.1.1", "10.1.1.2", "10.2.2.2"} {
			if strings.Contains(b.Actor, planted) {
				t.Errorf("a visitor-supplied identity %q became a ledger actor: %+v", planted, b)
			}
		}
	}
	if h := srv.Reads.Heat(victim.ID, "", time.Time{}); len(h) != 0 {
		t.Errorf("a share visit polluted another org's project buckets: %+v", h)
	}
	// The forged device headers must not have registered a device either.
	if _, ok := srv.Devices.Get("dev-alice"); ok {
		t.Error("an anonymous share visitor registered a device in the hub-wide registry")
	}
}

// secledProjectFor creates a project owned by whoever the cookie belongs to.
func secledProjectFor(t *testing.T, h http.Handler, c *http.Cookie, name string) Project {
	t.Helper()
	rec := doAs(t, h, "POST", "/api/projects", map[string]string{"name": name}, c)
	if rec.Code != 200 {
		t.Fatalf("create project %q: %d %s", name, rec.Code, rec.Body)
	}
	var out struct {
		Project Project `json:"project"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out.Project
}

// A share link that must not serve — revoked, expired, or minted by someone
// who has left the org — must not record a read either. Otherwise the ledger
// becomes a probe: a holder of a dead token can still count against the
// project, and the last_read stamp on a file confirms the token was once real.
func TestSec_Share_DeadLinksRecordNothing(t *testing.T) {
	h, srv, c, p := permHub(t)
	secledReads(t, srv)
	srv.ShareRPM = 100000
	secledSeedFile(t, h, c["alice"], p.ID, "dev1", "hr/eng/payroll.md", "# Payroll\n\nsecret", 1)

	revoked := secledShare(t, h, c["alice"], p.ID, "hr/eng/payroll.md")
	if rec := doAs(t, h, "DELETE", "/api/shares/"+revoked, nil, c["alice"]); rec.Code != 200 && rec.Code != 204 {
		t.Fatalf("revoke: %d %s", rec.Code, rec.Body)
	}

	// A link whose creator left the org (round 2's shareCreatorStillBelongs).
	orphan := secledShare(t, h, c["carol"], p.ID, "hr/eng/payroll.md")
	if err := srv.Dir.(LocalDirectory).RemoveMember(p.Org, "carol@x.io"); err != nil {
		t.Fatal(err)
	}

	// A link whose file has since been deleted from the project. The delete
	// op goes in its own device's journal — each device writes only its own.
	secledSeedFile(t, h, c["alice"], p.ID, "dev3", "hr/eng/temp.md", "temporary", 2)
	ghost := secledShare(t, h, c["alice"], p.ID, "hr/eng/temp.md")
	del := `{"seq":1,"lamport":9,"time":"2026-02-01T00:00:00Z","device":"dev4",` +
		`"kind":"delete","path":"hr/eng/temp.md"}` + "\n"
	req := httptest.NewRequest("PUT", "/api/p/"+p.ID+"/store/object?key=journal/dev4.jsonl", strings.NewReader(del))
	req.AddCookie(c["alice"])
	req.Header.Set("X-Bdrive-Device", "dev4")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("seed delete op: %d %s", rec.Code, rec.Body)
	}

	for _, tok := range []string{revoked, orphan, ghost, "totally-made-up-token"} {
		rec := secledGet(h, "/s/"+tok, "203.0.113.9:5555", nil)
		if rec.Code == 200 {
			t.Fatalf("a dead share link served content: token %s → %d", tok, rec.Code)
		}
	}
	if got := secledBuckets(srv.Reads); len(got) != 0 {
		t.Errorf("share links that refused to serve still recorded reads: %+v", got)
	}
}

// ---- lead 3: q()'s ?→$N rewrite (Postgres) -------------------------------

// q() rewrites ?-placeholders to $1,$2,… by walking the query TEXT. That is
// string surgery over SQL, and it is only correct as long as no runtime value
// can reach the string it walks: one `?` inside an interpolated value would
// shift every placeholder after it onto the wrong argument — a bound query
// turned into a wrong-row read or write, on the one backend no test has ever
// run against.
//
// This is a static assertion because it is a static property: every SQL string
// in db_sql.go must be a compile-time constant. The day someone writes
// s.q("... WHERE path LIKE '"+p+"'"), this fails.
// secledBadSQL is the negative control: a file shaped exactly like db_sql.go
// but with a value interpolated into the query text. The sweep below must flag
// every line of it, or the sweep is vacuous.
const secledBadSQL = `package p

func f(s *sqlMetaStore, path, table string) {
	s.db.Exec(s.q("DELETE FROM read_stats WHERE path = '" + path + "'"))
	s.db.Query("SELECT * FROM " + table)
	s.exec("UPDATE projects SET name = '" + path + "' WHERE id = ?", path)
	s.addColumns(table, map[string]string{"x": "TEXT"})
}
`

func TestSec_DB_QueryRewriteOnlyEverSeesStaticSQL(t *testing.T) {
	fset := token.NewFileSet()

	// static reports whether e is a string built entirely from literals —
	// including q(literal), whose output is a pure function of its input.
	var static func(ast.Expr) bool
	static = func(e ast.Expr) bool {
		switch n := e.(type) {
		case *ast.BasicLit:
			return n.Kind == token.STRING
		case *ast.ParenExpr:
			return static(n.X)
		case *ast.BinaryExpr:
			return n.Op == token.ADD && static(n.X) && static(n.Y)
		case *ast.CallExpr:
			sel, ok := n.Fun.(*ast.SelectorExpr)
			return ok && sel.Sel.Name == "q" && len(n.Args) == 1 && static(n.Args[0])
		}
		return false
	}

	// Three functions legitimately take a query (or a DDL fragment) in a
	// variable: exec is the one-liner wrapper — guarded at its own call sites,
	// which this same sweep checks — while migrate ranges over a slice of
	// literal statements and addColumns builds DDL from identifiers whose
	// values must be literals at every call site.
	passthrough := map[string]bool{"exec": true, "migrate": true, "addColumns": true}
	sqlCalls := map[string]bool{"q": true, "exec": true, "Exec": true, "Query": true, "QueryRow": true}

	// sweep reports every place a runtime value can reach SQL text.
	sweep := func(file *ast.File) []string {
		var bad []string
		var enclosing string
		ast.Inspect(file, func(n ast.Node) bool {
			if fn, ok := n.(*ast.FuncDecl); ok {
				enclosing = fn.Name.Name
			}
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) == 0 {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			switch {
			case sqlCalls[sel.Sel.Name]:
				if static(call.Args[0]) || passthrough[enclosing] {
					return true
				}
				bad = append(bad, fmt.Sprintf("%s: %s.%s() takes a non-constant SQL string inside %s()",
					fset.Position(call.Pos()), exprString(sel.X), sel.Sel.Name, enclosing))
			case sel.Sel.Name == "addColumns":
				for i, a := range call.Args {
					if i == 0 {
						if !static(a) {
							bad = append(bad, fmt.Sprintf("%s: addColumns table name is not a literal",
								fset.Position(a.Pos())))
						}
						continue
					}
					lit, ok := a.(*ast.CompositeLit)
					if !ok {
						bad = append(bad, fmt.Sprintf("%s: addColumns column spec is not a literal map",
							fset.Position(a.Pos())))
						continue
					}
					for _, el := range lit.Elts {
						kv, ok := el.(*ast.KeyValueExpr)
						if !ok || !static(kv.Key) || !static(kv.Value) {
							bad = append(bad, fmt.Sprintf("%s: addColumns column spec is not built from literals",
								fset.Position(el.Pos())))
						}
					}
				}
			}
			return true
		})
		return bad
	}

	// The sweep must have teeth: on the negative control every statement is a
	// violation, and a sweep that misses them proves nothing about the real file.
	ctl, err := parser.ParseFile(fset, "secled_control.go", secledBadSQL, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Four interpolated statements; the outer Exec and its nested q() both
	// count, so five flags is the expected floor.
	if got := sweep(ctl); len(got) < 5 {
		t.Fatalf("the sweep is vacuous: it flagged only %d violations in the control: %v", len(got), got)
	}

	file, err := parser.ParseFile(fset, "db_sql.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range sweep(file) {
		t.Errorf("%s — a runtime value in the query text desynchronizes q()'s ?→$N rewrite", b)
	}
}

// exprString renders the receiver of a call for the message above.
func exprString(e ast.Expr) string {
	switch n := e.(type) {
	case *ast.Ident:
		return n.Name
	case *ast.SelectorExpr:
		return exprString(n.X) + "." + n.Sel.Name
	}
	return "?"
}

// The rewrite itself: N placeholders in, $1..$N out, in order. Argument values
// are never part of the input — q takes only the query — so this is the whole
// contract, and it is what the round-trip test below leans on.
func TestSec_DB_PlaceholderRewriteIsPositional(t *testing.T) {
	s := &sqlMetaStore{d: dialectPostgres}
	got := s.q(`INSERT INTO read_stats (project,path,day,kind,actor,count,last) VALUES (?,?,?,?,?,?,?)`)
	want := `INSERT INTO read_stats (project,path,day,kind,actor,count,last) VALUES ($1,$2,$3,$4,$5,$6,$7)`
	if got != want {
		t.Errorf("q() rewrote to\n  %s\nwant\n  %s", got, want)
	}
	lite := &sqlMetaStore{d: dialectSQLite}
	if in := `SELECT 1 WHERE a = ? AND b = ?`; lite.q(in) != in {
		t.Errorf("q() rewrote a sqlite query: %s", lite.q(in))
	}
}

// Running the never-run backend turned up the one thing q()'s rewrite was a
// decoy for: Postgres refuses a NUL byte in TEXT (SQLSTATE 22021), and the
// read ledger is the one store that MUST swallow a write error — "telemetry
// never fails a request or a sync cycle". It swallows it by keeping every
// dirty bucket queued and retrying the whole batch as one transaction, so a
// single unstorable bucket takes every other bucket down with it, forever:
// the batch never succeeds, l.dirty never clears, and nothing the hub has
// counted since reaches disk.
//
// The path in is POST /api/p/<id>/reads, which filters only "" and "..": any
// member with the LOWEST privilege that exists (read) can send one report with
// a NUL byte in a path and permanently stop read telemetry from persisting — for
// every project on the hub, not just theirs — losing everything at the next
// restart. The request is answered 200, as telemetry always is, so nothing
// surfaces.
//
// Secure behavior: a bucket the store will never accept is dropped (log once),
// and the buckets around it persist. Asserted on every backend, because the
// divergence is the point — file and sqlite store the NUL happily.
func TestSec_Reads_OneUnstorableBucketCannotWedgeTheLedger(t *testing.T) {
	for _, be := range metaBackends(t) {
		t.Run(be.name, func(t *testing.T) {
			be.reset(t)
			st := be.open(t)
			h, srv, c, p := permHub(t)
			secledReads(t, srv) // devices registry; the ledger is replaced below
			var err error
			if srv.Reads, err = NewReadLedger(st.Reads(), 0); err != nil {
				t.Fatal(err)
			}
			// bob is a read-only member: the least privilege the hub grants.
			if err := srv.Projects.SetPerm(p.ID, "bob@x.io", PermRead); err != nil {
				t.Fatal(err)
			}
			base := "/api/p/" + p.ID + "/reads"
			hdr := map[string]string{"X-Bdrive-Device": "dev-bob"}

			// Round 12 fixture update, not a change of subject: handleReadReport
			// now drops a reported path that is not in the project's replayed
			// state (TestSec_Heat_AgentReadsAreNotForgeableForUnreadPaths), so
			// the control reads have to be reads of real files or nothing is
			// recorded and this test measures the wrong absence. The assertion
			// below — one unstorable bucket must not take its neighbours with it
			// — is untouched.
			for _, f := range []string{"warmup.md", "before.md", "after.md"} {
				secauthzUpload(t, h, p.ID, f, "x", c["alice"])
			}

			report := func(path string) {
				t.Helper()
				rec := secledDo(t, h, "POST", base,
					map[string]any{"reads": []map[string]string{{"path": path}}}, c["bob"], hdr)
				if rec.Code != 200 {
					t.Fatalf("read report for %q: %d %s", path, rec.Code, rec.Body)
				}
			}
			// ownsDevice runs before observeDevice, so a device's very first
			// report is accepted but counted for nobody. Warm it up first, or
			// the fixture eats the control read.
			report("warmup.md")
			report("before.md")
			report("pois\x00on.md") // the attack: one NUL, one request
			report("after.md")
			// A bystander project, to size the blast radius: one ledger, one
			// dirty set, one batch — so this is not bob's project's problem.
			srv.Reads.Record("other-project", "unrelated.md", ReadKindHuman, "someone@x.io")

			// Whatever the ledger could not store, what it COULD store must
			// reach disk. Close is the hub's shutdown flush.
			closeErr := srv.Reads.Close()
			st.Close()

			st2 := be.open(t)
			defer st2.Close()
			l2, err := NewReadLedger(st2.Reads(), 0)
			if err != nil {
				t.Fatal(err)
			}
			heat := l2.Heat(p.ID, "", time.Time{})
			for _, path := range []string{"before.md", "after.md"} {
				if heat[path].Agent != 1 {
					t.Errorf("one member's NUL-bearing read report discarded the hub's whole "+
						"read ledger: %q is gone after a restart (heat=%+v, close reported %v)",
						path, heat, closeErr)
				}
			}
			if other := l2.Heat("other-project", "", time.Time{}); other["unrelated.md"].Human != 1 {
				t.Errorf("the wedge is hub-wide: a bystander project's reads went with it (%+v)", other)
			}
		})
	}
}

// secledDo is doAs plus request headers (the read-report route needs both a
// session cookie and a device identity).
func secledDo(t *testing.T, h http.Handler, method, url string, body any, c *http.Cookie, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(method, url, strings.NewReader(string(data)))
	if c != nil {
		req.AddCookie(c)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// The behavioral half, run against every backend the environment offers
// (postgres included when BDRIVE_TEST_POSTGRES is set): values full of `?`
// must land in the columns they were bound to, and must not disturb a control
// row. If the rewrite could ever be desynchronized by a value, this is where
// the wrong-row read shows up — a grant on the wrong project, a share pointing
// at the wrong file, a read bucket attributed to the wrong actor.
func TestSec_DB_QuestionMarksInValuesDoNotShiftPlaceholders(t *testing.T) {
	const (
		qProject = `q3? report'; --`
		qEmail   = `who?me@x.io`
		qPath    = `docs/what?.md`
		qActor   = `agent?1`
		qDevice  = `dev?1`
		qOrg     = `o-?-1`
	)
	for _, be := range metaBackends(t) {
		t.Run(be.name, func(t *testing.T) {
			be.reset(t)
			st := be.open(t)

			projects, err := NewProjectDB(st.Projects())
			if err != nil {
				t.Fatal(err)
			}
			ctl, _, err := projects.GetOrCreate("control", "o-control")
			if err != nil {
				t.Fatal(err)
			}
			if err := projects.SetPerm(ctl.ID, "keeper@x.io", PermRead); err != nil {
				t.Fatal(err)
			}
			bad, _, err := projects.GetOrCreate(qProject, qOrg)
			if err != nil {
				t.Fatal(err)
			}
			if err := projects.SetPerm(bad.ID, qEmail, PermAdmin); err != nil {
				t.Fatal(err)
			}

			shares, err := NewShareDB(st.Shares())
			if err != nil {
				t.Fatal(err)
			}
			sh, err := shares.Create(bad.ID, qPath, qEmail, 0)
			if err != nil {
				t.Fatal(err)
			}

			devices, err := NewDeviceRegistry(st.Devices())
			if err != nil {
				t.Fatal(err)
			}
			devices.Observe(DeviceInfo{ID: qDevice, Name: `lap?top`, OS: `os?`, User: qEmail, IP: "1.2.3.4"})

			reads, err := NewReadLedger(st.Reads(), 0)
			if err != nil {
				t.Fatal(err)
			}
			reads.Record(bad.ID, qPath, ReadKindAgent, qActor)
			if err := reads.Close(); err != nil {
				t.Fatal(err)
			}
			if err := st.Close(); err != nil {
				t.Fatal(err)
			}

			st2 := be.open(t)
			defer st2.Close()

			projects2, err := NewProjectDB(st2.Projects())
			if err != nil {
				t.Fatal(err)
			}
			got, ok := projects2.Get(bad.ID)
			if !ok {
				t.Fatal("the project with `?` in its name did not survive")
			}
			if got.Name != qProject || got.Org != qOrg {
				t.Errorf("columns shifted: name=%q org=%q, want %q / %q", got.Name, got.Org, qProject, qOrg)
			}
			if got.Perms[normEmail(qEmail)] != PermAdmin {
				t.Errorf("the grant landed on the wrong row/column: %+v", got.Perms)
			}
			keep, ok := projects2.Get(ctl.ID)
			if !ok || keep.Name != "control" || keep.Perms["keeper@x.io"] != PermRead {
				t.Errorf("the control row was disturbed by a value containing `?`: %+v ok=%v", keep, ok)
			}
			if _, wrong := keep.Perms[normEmail(qEmail)]; wrong {
				t.Errorf("a grant bound to %s landed on the control project: %+v", bad.ID, keep.Perms)
			}

			shares2, err := NewShareDB(st2.Shares())
			if err != nil {
				t.Fatal(err)
			}
			gs, ok := shares2.Get(sh.Token)
			if !ok || gs.Path != qPath || gs.Project != bad.ID || gs.Creator != qEmail {
				t.Errorf("share columns shifted: %+v ok=%v", gs, ok)
			}

			devices2, err := NewDeviceRegistry(st2.Devices())
			if err != nil {
				t.Fatal(err)
			}
			gd, ok := devices2.Get(qDevice)
			if !ok || gd.Name != `lap?top` || gd.OS != `os?` || gd.User != qEmail {
				t.Errorf("device columns shifted: %+v ok=%v", gd, ok)
			}

			reads2, err := NewReadLedger(st2.Reads(), 0)
			if err != nil {
				t.Fatal(err)
			}
			if e := reads2.Heat(bad.ID, "", time.Time{})[qPath]; e.Agent != 1 {
				t.Errorf("read bucket columns shifted: %+v", e)
			}
			if e := reads2.Heat(ctl.ID, "", time.Time{}); len(e) != 0 {
				t.Errorf("a read bucket bound to %s was attributed to the control project: %+v", bad.ID, e)
			}
		})
	}
}
