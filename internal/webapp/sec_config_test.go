package webapp

// Round 3: the structural blind spot of rounds 1 and 2 — every hub they
// attacked was built by a test fixture, so no hub with a real DSN, a real
// SMTP password, TrustProxy, allowed_domains or an admin list had ever been
// probed. This file builds a hub the way production builds one (the real
// `bdrive serve -c config.json` path through cmd/bdrive/web.go) and attacks
// it, plus the three surfaces the CISO listed as never exercised: GET /
// (Server.frontend), the login rate limiter's effectiveness, and timing-based
// user enumeration.
//
// Helper prefix: seccfg.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Part 1 — a hub built through the real configuration path.
// ---------------------------------------------------------------------------

// Sentinels planted in the config file. Each stands for a credential that only
// exists on a real hub: the object-store URL, the metadata DSN, the SMTP
// password. Any of them appearing in a response (or in the server's own
// output) is a leak.
const (
	seccfgStorageSecret = "STORAGECRED7QX"
	seccfgDBSecret      = "DBSECRET7QX"
	seccfgSMTPSecret    = "SMTPPASS7QX"
	seccfgAdminEmail    = "admin@example.com"
	seccfgAdminPass     = "password1"
)

var (
	seccfgBinOnce sync.Once
	seccfgBinPath string
	seccfgBinErr  error
)

// seccfgBinary builds cmd/bdrive once for the whole package run.
func seccfgBinary(t *testing.T) string {
	t.Helper()
	seccfgBinOnce.Do(func() {
		dir, err := os.MkdirTemp("", "seccfg-bin")
		if err != nil {
			seccfgBinErr = err
			return
		}
		bin := filepath.Join(dir, "bdrive")
		out, err := exec.Command("go", "build", "-o", bin, "github.com/runbear-io/beardrive/cmd/bdrive").CombinedOutput()
		if err != nil {
			seccfgBinErr = fmt.Errorf("go build: %v\n%s", err, out)
			return
		}
		seccfgBinPath = bin
	})
	if seccfgBinErr != nil {
		t.Fatal(seccfgBinErr)
	}
	return seccfgBinPath
}

// seccfgFreePort grabs a port the hub can bind.
func seccfgFreePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// seccfgRealHub starts `bdrive serve -c config.json` — the only code path on
// which a DSN, an SMTP password and a storage credential exist at all — and
// returns its base URL plus a reader for everything the process has printed.
func seccfgRealHub(t *testing.T) (base string, serverOutput func() string) {
	t.Helper()
	if testing.Short() {
		t.Skip("builds and execs the bdrive binary; skipped with -short")
	}
	bin := seccfgBinary(t)
	state := t.TempDir()
	home := filepath.Join(state, "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	// The object-store root, the metadata DSN and the SMTP password each
	// carry a distinct sentinel.
	storage := filepath.Join(state, "storage-"+seccfgStorageSecret)
	if err := os.MkdirAll(storage, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := map[string]any{
		"remote":      "file://" + storage,
		"upload":      true,
		"trust_proxy": false,
		"database": map[string]any{
			"driver": "sqlite",
			"dsn":    filepath.Join(state, "hub-"+seccfgDBSecret+".db"),
		},
		"auth": map[string]any{
			"allow_signup":    false,
			"allowed_domains": []string{"example.com"},
			"admins":          []string{seccfgAdminEmail},
			"brand":           "Sec Round 3",
			// A hub with smtp must name its public origin: a mailed link may
			// not be built from a requester's Host header.
			"base_url": "https://hub.example",
			"smtp": map[string]any{
				// Port 1 refuses instantly, so nothing here ever blocks.
				"host": "127.0.0.1", "port": 1,
				"user": "mailer@example.com", "pass": seccfgSMTPSecret,
				"from": "hub@example.com",
			},
		},
	}
	cfgPath := filepath.Join(state, "config.json")
	data, _ := json.MarshalIndent(cfg, "", "  ")
	if err := os.WriteFile(cfgPath, data, 0o600); err != nil {
		t.Fatal(err)
	}

	port := seccfgFreePort(t)
	cmd := exec.Command(bin, "serve", "-c", cfgPath, "--addr", fmt.Sprintf("127.0.0.1:%d", port))
	cmd.Env = append(envWithout("HOME", "BDRIVE_HOME"),
		"HOME="+home, "BDRIVE_HOME="+filepath.Join(home, ".bdrive"))
	var out seccfgBuf
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
	})

	base = fmt.Sprintf("http://127.0.0.1:%d", port)
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(base + "/api/config")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return base, out.String
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("hub never came up on %s:\n%s", base, out.String())
	return "", nil
}

// seccfgBuf is a concurrency-safe sink for the child process's output.
type seccfgBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *seccfgBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *seccfgBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// seccfgAdminClient bootstraps the config's admin account (a fresh
// invite-only hub lets exactly the configured admins create the first
// account) and returns a client holding its session.
func seccfgAdminClient(t *testing.T, base string) *http.Client {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar, Timeout: 20 * time.Second}
	resp, err := c.PostForm(base+"/auth/signup", url.Values{
		"email": {seccfgAdminEmail}, "name": {"Admin"}, "password": {seccfgAdminPass},
	})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	u, _ := url.Parse(base)
	if len(jar.Cookies(u)) == 0 {
		t.Fatalf("admin bootstrap signup left no session cookie: %d\n%s", resp.StatusCode, body)
	}
	return c
}

func seccfgGet(t *testing.T, c *http.Client, target string) (int, string) {
	t.Helper()
	resp, err := c.Get(target)
	if err != nil {
		t.Fatalf("GET %s: %v", target, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// TestSec_Leak_RealConfigPathKeepsSecretsOffTheWire probes row 12 on the only
// kind of hub that has secrets to leak: one configured by cmd/bdrive/web.go
// with a metadata DSN, an SMTP password and a storage URL.
func TestSec_Leak_RealConfigPathKeepsSecretsOffTheWire(t *testing.T) {
	base, serverOut := seccfgRealHub(t)
	admin := seccfgAdminClient(t, base)

	// A project, so the per-project routes (and their error bodies) are live.
	body, _ := json.Marshal(map[string]string{"name": "wiki"})
	resp, err := admin.Post(base+"/api/projects", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	var created struct {
		Project Project `json:"project"`
	}
	json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	if created.Project.ID == "" {
		t.Fatal("could not create a project on the real-config hub")
	}
	id := created.Project.ID

	anon := &http.Client{Timeout: 20 * time.Second}
	type probe struct {
		what   string
		client *http.Client
		target string
	}
	probes := []probe{
		{"anonymous /api/config", anon, base + "/api/config"},
		{"admin /api/config", admin, base + "/api/config"},
		{"admin /api/projects", admin, base + "/api/projects"},
		{"admin /api/orgs", admin, base + "/api/orgs"},
		{"admin /api/admin/policy", admin, base + "/api/admin/policy"},
		{"admin /api/admin/pending", admin, base + "/api/admin/pending"},
		{"admin project get", admin, base + "/api/projects/" + id},
		{"admin tree", admin, base + "/api/p/" + id + "/tree"},
		{"missing file error", admin, base + "/api/p/" + id + "/file?path=nope.md"},
		{"missing blob error", admin, base + "/api/p/" + id + "/blob?sha=" + strings.Repeat("a", 64)},
		{"missing store object error", admin, base + "/api/p/" + id + "/store/object?key=blobs/" + strings.Repeat("b", 64)},
		{"unknown project error", admin, base + "/api/p/00000000-0000-0000-0000-000000000000/tree"},
		{"anonymous SPA shell", anon, base + "/"},
		{"sign-in page", anon, base + "/auth/login"},
	}
	secrets := map[string]string{
		"storage root":   seccfgStorageSecret,
		"metadata DSN":   seccfgDBSecret,
		"SMTP password":  seccfgSMTPSecret,
		"admin password": seccfgAdminPass,
	}
	for _, p := range probes {
		code, out := seccfgGet(t, p.client, p.target)
		for name, s := range secrets {
			if strings.Contains(out, s) {
				t.Errorf("%s (%d) leaks the %s: %q appears in the response\n%s",
					p.what, code, name, s, out)
			}
		}
	}

	// The hub's own output is the other surface an operator (or a log
	// shipper, or a support bundle) sees. Neither true secret belongs there.
	logs := serverOut()
	for _, s := range []string{seccfgDBSecret, seccfgSMTPSecret, seccfgAdminPass} {
		if strings.Contains(logs, s) {
			t.Errorf("the hub printed %q to its own output:\n%s", s, logs)
		}
	}
}

// TestSec_Admin_PolicyCannotWidenServerOwnedAccess checks the claim
// CLAUDE.md makes about the real config path: allow_signup, allowed_domains
// and admins are server-config-owned, so a browser session — even a hub
// admin's — must not be able to widen them.
func TestSec_Admin_PolicyCannotWidenServerOwnedAccess(t *testing.T) {
	base, _ := seccfgRealHub(t)
	admin := seccfgAdminClient(t, base)

	code, before := seccfgGet(t, admin, base+"/api/admin/policy")
	if code != 200 {
		t.Fatalf("admin cannot read the policy: %d %s", code, before)
	}
	var pol SignupPolicy
	if err := json.Unmarshal([]byte(before), &pol); err != nil {
		t.Fatalf("policy is not a SignupPolicy: %v\n%s", err, before)
	}
	if pol.AllowSignup {
		t.Fatalf("fixture wrong: config said allow_signup false, hub reports %v", before)
	}

	// Everything a session might try to widen, in one body.
	widen, _ := json.Marshal(map[string]any{
		"require_verification": false,
		"require_approval":     false,
		"allow_signup":         true,
		"allowed_domains":      []string{"example.com", "evil.test"},
		"admins":               []string{seccfgAdminEmail, "attacker@evil.test"},
		"mailer":               true,
	})
	resp, err := admin.Post(base+"/api/admin/policy", "application/json", bytes.NewReader(widen))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	_, after := seccfgGet(t, admin, base+"/api/admin/policy")
	var got SignupPolicy
	if err := json.Unmarshal([]byte(after), &got); err != nil {
		t.Fatal(err)
	}
	if got.AllowSignup {
		t.Errorf("a browser session turned self-signup ON through /api/admin/policy: %s", after)
	}
	sort.Strings(got.AllowedDomains)
	if strings.Join(got.AllowedDomains, ",") != "example.com" {
		t.Errorf("a browser session rewrote allowed_domains: %v", got.AllowedDomains)
	}
	sort.Strings(got.Admins)
	if strings.Join(got.Admins, ",") != seccfgAdminEmail {
		t.Errorf("a browser session rewrote the hub admin list: %v", got.Admins)
	}

	// And the functional check the booleans stand for: signup is still shut.
	anon := &http.Client{Timeout: 20 * time.Second}
	_, page := seccfgGet(t, anon, base+"/auth/signup")
	if !strings.Contains(page, "invite-only") {
		t.Errorf("self-signup opened after the policy POST:\n%s", page)
	}
}

// ---------------------------------------------------------------------------
// Part 2 — GET / (Server.frontend): the only route with no TestSec coverage.
// ---------------------------------------------------------------------------

// seccfgRaw sends a request with a literal (uncleaned) request target, so a
// traversal attempt reaches the handler exactly as an attacker typed it.
func seccfgRaw(t *testing.T, h http.Handler, target string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", "http://hub.test/", nil)
	u, err := url.Parse(target)
	if err != nil {
		t.Fatalf("bad target %q: %v", target, err)
	}
	req.URL = u
	req.RequestURI = target
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestSec_Frontend_FallbackServesOnlyEmbeddedAssets attacks the SPA fallback
// with every shape of escape: a hub host's files must never come back, and a
// reserved prefix must never be masked by the app shell.
func TestSec_Frontend_FallbackServesOnlyEmbeddedAssets(t *testing.T) {
	h, _, _, _ := permHub(t)

	// Something recognisable on the host, one and two levels above cwd, so a
	// successful escape has a proof string.
	host := filepath.Join(t.TempDir(), "hostfile.txt")
	if err := os.WriteFile(host, []byte("SECCFG-HOST-FILE"), 0o644); err != nil {
		t.Fatal(err)
	}

	escapes := []string{
		"/../../../../etc/passwd",
		"/..%2f..%2f..%2fetc%2fpasswd",
		"/%2e%2e/%2e%2e/etc/passwd",
		"/assets/../../../../etc/passwd",
		"/assets/..%2f..%2f..%2f..%2fgo.mod",
		"/./../go.mod",
		"/go.mod",
		"/../server.go",
		"/static/../../go.mod",
		"//etc/passwd",
		"/....//....//etc/passwd",
		"/" + strings.TrimPrefix(host, "/"),
		"/..\\..\\go.mod",
	}
	for _, target := range escapes {
		rec := seccfgRaw(t, h, target)
		body := rec.Body.String()
		for _, proof := range []string{"root:x:", "SECCFG-HOST-FILE", "module github.com/runbear-io/beardrive", "package webapp"} {
			if strings.Contains(body, proof) {
				t.Errorf("GET %s escaped the embedded FS (%d): body contains %q", target, rec.Code, proof)
			}
		}
		// Anything that resolves outside the app is either a 404 or the app
		// shell — never a 200 with a non-HTML content type.
		if rec.Code == 200 {
			if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
				t.Errorf("GET %s returned 200 %s — the fallback served a non-shell body:\n%s",
					target, ct, body[:min(len(body), 300)])
			}
		}
	}

	// Reserved prefixes must 404 rather than be masked by the shell, so a
	// mistyped API URL can't be mistaken for a working page.
	for _, target := range []string{"/api/nope", "/auth/nope", "/s/nope-token-that-does-not-exist"} {
		rec := seccfgRaw(t, h, target)
		if rec.Code == 200 && strings.Contains(rec.Body.String(), "<div id=\"root\">") {
			t.Errorf("GET %s was masked by the SPA shell", target)
		}
	}
}

// TestSec_Frontend_ShellCarriesFramingAndSniffingDefenses: the hub UI holds a
// session cookie and renders user-supplied markdown, so the document it ships
// must not be frameable by another origin and must not be MIME-sniffed.
func TestSec_Frontend_ShellCarriesFramingAndSniffingDefenses(t *testing.T) {
	h, _, _, _ := permHub(t)

	for _, target := range []string{"/", "/index.html", "/some-project-id/dashboard"} {
		rec := seccfgRaw(t, h, target)
		if rec.Code != 200 {
			t.Fatalf("GET %s: %d (expected the app shell)", target, rec.Code)
		}
		hdr := rec.Header()
		if got := hdr.Get("X-Content-Type-Options"); !strings.EqualFold(got, "nosniff") {
			t.Errorf("GET %s: X-Content-Type-Options = %q, want nosniff — the shell is MIME-sniffable", target, got)
		}
		csp := hdr.Get("Content-Security-Policy")
		xfo := hdr.Get("X-Frame-Options")
		framed := strings.Contains(strings.ToLower(csp), "frame-ancestors") ||
			strings.EqualFold(xfo, "DENY") || strings.EqualFold(xfo, "SAMEORIGIN")
		if !framed {
			t.Errorf("GET %s: no frame-ancestors CSP and no X-Frame-Options (CSP=%q XFO=%q) — "+
				"any origin can frame the signed-in hub UI and clickjack its controls", target, csp, xfo)
		}
	}
}

// TestSec_Frontend_ImmutableCacheOnlyOnRealAssets: the "assets/* is immutable"
// rule is keyed on the URL prefix, not on whether an asset actually exists, so
// a miss under assets/ is answered with the app shell marked cacheable for a
// year. A shared cache then pins that body at that URL past every upgrade.
func TestSec_Frontend_ImmutableCacheOnlyOnRealAssets(t *testing.T) {
	h, _, _, _ := permHub(t)

	rec := seccfgRaw(t, h, "/assets/there-is-no-such-asset.js")
	cc := rec.Header().Get("Cache-Control")
	ct := rec.Header().Get("Content-Type")
	if strings.Contains(cc, "immutable") && strings.HasPrefix(ct, "text/html") {
		t.Errorf("GET /assets/<missing> returned the app shell (%s) with %q — "+
			"a body that is not the asset it claims to be, cached for a year", ct, cc)
	}
	// A directory under assets/ is the same shape.
	rec = seccfgRaw(t, h, "/assets/")
	cc = rec.Header().Get("Cache-Control")
	ct = rec.Header().Get("Content-Type")
	if strings.Contains(cc, "immutable") && strings.HasPrefix(ct, "text/html") {
		t.Errorf("GET /assets/ returned the app shell (%s) with %q", ct, cc)
	}
}

// ---------------------------------------------------------------------------
// Part 3 — the login rate limiter, both TrustProxy postures.
// ---------------------------------------------------------------------------

// seccfgLogin posts a (wrong) credential from a chosen connection address and
// optional X-Forwarded-For, and reports the status.
func seccfgLogin(t *testing.T, h http.Handler, remoteAddr, xff string) int {
	t.Helper()
	form := url.Values{"email": {"nobody@x.io"}, "password": {"wrong"}}
	req := httptest.NewRequest("POST", "http://hub.test/auth/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = remoteAddr
	if xff != "" {
		req.Header.Set("X-Forwarded-For", xff)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

// TestSec_RateLimit_LoginLimiterIgnoresSpoofedForwardedFor: with trust_proxy
// off (the default, and the only safe setting on a directly-reachable hub) a
// client-supplied X-Forwarded-For must not mint a fresh bucket per request.
func TestSec_RateLimit_LoginLimiterIgnoresSpoofedForwardedFor(t *testing.T) {
	h, srv, _, _ := permHub(t)
	if srv.TrustProxy {
		t.Fatal("fixture wrong: TrustProxy should default to off")
	}
	// The limiter is 10/min with a burst of 10, so the 11th attempt from one
	// client must be refused however the client decorates the request.
	blocked := false
	for i := 0; i < 40; i++ {
		code := seccfgLogin(t, h, "203.0.113.9:5555", fmt.Sprintf("10.0.0.%d", i%250))
		if code == http.StatusTooManyRequests {
			blocked = true
			break
		}
	}
	if !blocked {
		t.Error("40 failed logins from one connection all went through while rotating X-Forwarded-For: " +
			"the brute-force limiter is bypassable by a header the client controls")
	}
}

// TestSec_RateLimit_TrustedProxyUsesTheHopItAdded: with trust_proxy ON the
// hub must key on the hop its own proxy appended — the LAST value — not the
// first one, which the client wrote itself. X-Forwarded-For grows
// left-to-right, so a client that sends "X-Forwarded-For: <anything>" has its
// value preserved at the head of the list by every downstream proxy.
func TestSec_RateLimit_TrustedProxyUsesTheHopItAdded(t *testing.T) {
	h, srv, _, _ := permHub(t)
	srv.TrustProxy = true
	h = srv.Handler() // rebuild so the flag is in force for this handler

	// One real client behind the operator's proxy at 10.1.1.1. The proxy
	// appends the address it saw; the client prepends a new lie each time.
	const proxy = "10.1.1.1"
	blocked := false
	for i := 0; i < 40; i++ {
		xff := fmt.Sprintf("198.51.100.%d, %s", i%250, proxy)
		if seccfgLogin(t, h, proxy+":9999", xff) == http.StatusTooManyRequests {
			blocked = true
			break
		}
	}
	if !blocked {
		t.Error("with trust_proxy on, 40 failed logins arriving through the same proxy hop all went through: " +
			"the hub keys the limiter on the FIRST X-Forwarded-For entry, which the client prepends, " +
			"so turning trust_proxy on disables the brute-force limiter instead of fixing it")
	}
}

// ---------------------------------------------------------------------------
// Part 4 — timing-based user enumeration.
// ---------------------------------------------------------------------------

// seccfgSlowSMTP is a listener that accepts and then stalls, standing in for
// any real mail server: dialing it costs measurable, observable time.
func seccfgSlowSMTP(t *testing.T, stall time.Duration) (host string, port int) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				time.Sleep(stall)
				c.Close()
			}()
		}
	}()
	a := l.Addr().(*net.TCPAddr)
	return "127.0.0.1", a.Port
}

// seccfgMailHub is permHub's shape with SMTP configured — the posture a real
// hub runs in, and the one no fixture ever built.
func seccfgMailHub(t *testing.T, mail *Mailer) (http.Handler, *BuiltinAuth) {
	t.Helper()
	srv, _, _ := newHub(t, true, nil)
	auth, err := OpenBuiltinAuth(filepath.Join(t.TempDir(), "auth.json"), true, mail)
	if err != nil {
		t.Fatal(err)
	}
	srv.Auth = auth
	orgs, err := OpenOrgDB(filepath.Join(t.TempDir(), "orgs.json"))
	if err != nil {
		t.Fatal(err)
	}
	srv.Dir = LocalDirectory{OrgDB: orgs}
	return srv.Handler(), auth
}

func seccfgPostForm(t *testing.T, h http.Handler, target, remoteAddr string, form url.Values) (int, time.Duration) {
	t.Helper()
	req := httptest.NewRequest("POST", "http://hub.test"+target, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = remoteAddr
	rec := httptest.NewRecorder()
	start := time.Now()
	h.ServeHTTP(rec, req)
	return rec.Code, time.Since(start)
}

// TestSec_Leak_ResetTimingDoesNotEnumerateAccounts is the enumeration oracle
// that only exists on a hub with SMTP configured: pageReset sends mail when
// the address is known and does nothing at all when it isn't, so the response
// time — bodies are byte-identical, round 1 checked that — says which. Nothing
// throttles /auth/reset either (rateLimitAuth covers only login and signup),
// so the oracle is queryable at full speed.
func TestSec_Leak_ResetTimingDoesNotEnumerateAccounts(t *testing.T) {
	const stall = 700 * time.Millisecond
	host, port := seccfgSlowSMTP(t, stall)
	h, _ := seccfgMailHub(t, &Mailer{Host: host, Port: port, User: "u", Pass: "p", From: "hub@x.io"})

	known := signupAndSession(t, h, "victim@x.io", "Victim", "password1")
	if known == nil {
		t.Fatal("fixture wrong: no session for the known account")
	}

	codeKnown, dKnown := seccfgPostForm(t, h, "/auth/reset", "203.0.113.1:1", url.Values{"email": {"victim@x.io"}})
	codeUnknown, dUnknown := seccfgPostForm(t, h, "/auth/reset", "203.0.113.2:1", url.Values{"email": {"ghost@x.io"}})
	if codeKnown != codeUnknown {
		t.Fatalf("reset status differs: known %d vs unknown %d", codeKnown, codeUnknown)
	}

	gap := dKnown - dUnknown
	if gap < 0 {
		gap = -gap
	}
	// The mail dial is worth `stall`; anything close to it is a clean signal
	// an attacker can read off a single request.
	if gap > stall/3 {
		t.Errorf("POST /auth/reset takes %v for a known address and %v for an unknown one (gap %v): "+
			"the response time enumerates accounts on any hub with SMTP configured, "+
			"and /auth/reset is not rate limited", dKnown, dUnknown, gap)
	}
}

// TestSec_Password_LoginTimingDoesNotEnumerateAccounts verifies the bcrypt
// time-burn in verifyPassword actually equalizes: a wrong password for a real
// account and a wrong password for an address that does not exist must cost
// the same. Samples are interleaved so machine drift hits both arms, compared
// on the median, and each request uses a fresh connection address so the
// 10/min login limiter never truncates the run.
func TestSec_Password_LoginTimingDoesNotEnumerateAccounts(t *testing.T) {
	if testing.Short() {
		t.Skip("runs ~30 bcrypt comparisons; skipped with -short")
	}
	h, _ := seccfgMailHub(t, nil)
	if c := signupAndSession(t, h, "real@x.io", "Real", "password1"); c == nil {
		t.Fatal("fixture wrong: no session for the known account")
	}

	const samples = 15
	knownD := make([]time.Duration, 0, samples)
	ghostD := make([]time.Duration, 0, samples)
	n := 0
	for i := 0; i < samples; i++ {
		for _, arm := range []struct {
			email string
			into  *[]time.Duration
		}{{"real@x.io", &knownD}, {"ghost@x.io", &ghostD}} {
			n++
			code, d := seccfgPostForm(t, h, "/auth/login",
				fmt.Sprintf("198.51.100.%d:1", n), url.Values{
					"email": {arm.email}, "password": {"definitely-wrong"},
				})
			if code == http.StatusTooManyRequests {
				t.Fatalf("login limiter truncated the timing run at sample %d", i)
			}
			*arm.into = append(*arm.into, d)
		}
	}
	mk, mg := seccfgMedian(knownD), seccfgMedian(ghostD)
	lo, hi := min(mk, mg), max(mk, mg)
	// Both arms run exactly one bcrypt cost-10 comparison, so they should be
	// within noise. A 3x bound only fires if one arm skips the work entirely.
	if hi > 3*lo {
		t.Errorf("login median for a known address is %v and for an unknown one %v: "+
			"the bcrypt burn in verifyPassword does not equalize, so response time enumerates accounts", mk, mg)
	}
}

func seccfgMedian(d []time.Duration) time.Duration {
	c := append([]time.Duration(nil), d...)
	sort.Slice(c, func(i, j int) bool { return c[i] < c[j] })
	return c[len(c)/2]
}

// TestFrontendRootDottedPathsAre404: a root-level path shaped like a file
// (/llms.txt, /robots.txt) that matches no embedded asset must 404 rather than
// answer the app shell — a soft 200 of login HTML told every crawler probing a
// conventional path that the file exists. Deeper dots are real client routes.
func TestFrontendRootDottedPathsAre404(t *testing.T) {
	h, _, _, p := permHub(t)

	for _, target := range []string{"/llms.txt", "/robots.txt", "/sitemap.xml", "/nope.json"} {
		rec := seccfgRaw(t, h, target)
		if rec.Code != 404 {
			t.Errorf("GET %s: %d, want 404 (got %s) — the SPA fallback still masks a missing root file",
				target, rec.Code, rec.Header().Get("Content-Type"))
		}
	}

	// A root-level dotted path that IS an embedded asset keeps being served:
	// the rule is "no embedded asset", not "has a dot", because the check runs
	// after the asset lookup. share-mermaid.js is the live case (the /s/ share
	// pages import it); a favicon.ico would be the next one, with no allowlist
	// to remember to update.
	rec := seccfgRaw(t, h, "/share-mermaid.js")
	if rec.Code != 200 {
		t.Errorf("GET /share-mermaid.js: %d, want 200 — the dotted-path 404 is deciding on the URL "+
			"instead of on whether the asset exists, and share pages just lost their mermaid renderer", rec.Code)
	}

	// Everything that is a genuine client route still resolves to the shell.
	// /index.html is here because the asset block skips it deliberately and
	// falls through: without the explicit exclusion the rule above would 404 it.
	for _, target := range []string{"/", "/index.html", "/" + p.ID, "/" + p.ID + "/dashboard", "/" + p.ID + "/notes/index.md"} {
		rec := seccfgRaw(t, h, target)
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), "<div id=\"root\">") {
			t.Errorf("GET %s: %d — a client route stopped resolving to the app shell", target, rec.Code)
		}
	}
}

// TestFrontendVolumeModeServesRootFiles: the dotted-root-path 404 above is
// hub-only reasoning. The plain-folder viewer shares this handler, and there
// /README.md IS the route for a file (router.ts parsePath, non-hub branch), so
// the rule is gated on hub mode. THIS TEST FAILS IF THAT GATE IS REMOVED —
// ungated, the fix 404s every top-level file in every `bdrive serve <dir>`.
func TestFrontendVolumeModeServesRootFiles(t *testing.T) {
	h := dirServer(t, map[string]string{"README.md": "# Local"})

	rec := seccfgRaw(t, h, "/README.md")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "<div id=\"root\">") {
		t.Errorf("GET /README.md in volume mode: %d — the hub-mode gate on the dotted-path 404 is gone; "+
			"every root-level file in a plain-folder viewer is now unreachable", rec.Code)
	}
}
