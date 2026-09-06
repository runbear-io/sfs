package remote

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/runbear-io/beardrive/internal/config"
	"github.com/runbear-io/beardrive/internal/journal"
)

// maxListBytes caps the JSON listing a hub can make a device allocate.
const maxListBytes = 8 << 20

// maxJSONBytes caps every other JSON body a hub answers with. Round 7 bounded
// List and round 8 the journal/blob bodies, but `sign` — the call every blob
// push starts with — and `Exists` still decoded straight off the wire, so the
// hub chose the allocation on the device's hottest path. One constant, applied
// wherever this file decodes the hub.
const maxJSONBytes = 1 << 20

// maxKeySegment bounds one path component of a key the hub names. Listed keys
// become local file paths (syncer.pull → store.JournalPath) and tar member
// names (`bdrive export`); NAME_MAX is 255 everywhere beardrive runs, so a
// longer segment is not a filename, it is an error the OS reports as something
// other than "does not exist" — which is how one listed key hid every peer.
const maxKeySegment = 255

// httpBackend syncs one project through a bdrive web server instead of
// talking to an object store. The client device is storage-blind: it only
// knows the server URL and a project id (https://host:4173/p/<project-id>,
// written by `bdrive init`); the storage location and credentials live on
// the server. Blob uploads go directly to the object store through
// short-lived presigned URLs when the server can mint them, and are relayed
// through the server otherwise.
//
// The server exposes the project's store under /api/p/<id>/store/* (list,
// object, exists, sign). Key layout and semantics are identical to any other
// backend, so the whole sync machinery works unchanged.
type httpBackend struct {
	base    string // scheme://host[:port]
	project string
	token   string // device token from `bdrive login`; empty on open servers
	device  config.Device
	hc      *http.Client
}

// The id is whatever the hub minted (a UUID today, `p-xxxxxxxx` on older
// hubs), so this only checks the shape of a URL segment — the hub is the
// authority on which ids exist.
var projectPathRe = regexp.MustCompile(`^/p/([A-Za-z0-9._-]{4,64})/?$`)

func newHTTPBackend(raw string) (*httpBackend, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("server remote needs a URL like https://host:4173/p/<project-id>, got %q", raw)
	}
	m := projectPathRe.FindStringSubmatch(u.Path)
	if m == nil {
		return nil, fmt.Errorf("server remote %q has no project (want https://host:4173/p/<project-id>; run `bdrive init`)", raw)
	}
	base := (&url.URL{Scheme: u.Scheme, Host: u.Host}).String()
	dev, _ := config.LoadDevice()
	hc := &http.Client{Timeout: 5 * time.Minute, CheckRedirect: refuseOffOriginRedirect}
	return &httpBackend{base: base, project: m[1], token: deviceToken(base), device: dev, hc: hc}, nil
}

// deviceToken finds this device's credential for the server at base:
// BDRIVE_TOKEN wins (tests, CI), otherwise the token `bdrive login` stored in
// settings — but only for the server it was issued for.
//
// The remote URL comes from a folder's .bdrive/config.json, which travels with
// the folder: without the origin check, a folder someone shares with you
// chooses where your hub credential is sent, plaintext http included. The same
// binding covers `bdrive login <other-hub>`, after which every old mount would
// otherwise ship the new hub's token to the old host.
func deviceToken(base string) string {
	if t := os.Getenv("BDRIVE_TOKEN"); t != "" {
		return t
	}
	s, err := config.LoadSettings()
	if err != nil || s.Token == "" || !sameOrigin(base, s.Server) {
		return ""
	}
	return s.Token
}

// sameOrigin compares scheme+host, the only thing that decides who receives a
// bearer token. A bare host in settings is read as https, which is what
// `bdrive login` writes it as.
//
// The comparison is on the ORIGIN, not on the URL's spelling: the scheme's
// default port, the case of the host and an FQDN's trailing dot all name the
// same server. Comparing url.Host verbatim made "https://hub:443" a different
// server from "https://hub", so the token was silently dropped and every sync
// 401'd forever — and `bdrive login` could not fix it, because it writes the
// same string back. Fail-closed is still the direction: anything that does not
// parse, or has no host, matches nothing.
func sameOrigin(a, b string) bool { return SameOrigin(a, b) }

// SameOrigin is the exported form, for the CLI. It is the one rule that
// decides who may receive this device's bearer token, and it is needed at two
// doors: the sync backend's (here) and `bdrive share`/`init`'s HTTP client
// (cmd/bdrive). It used to be spelled twice — which is exactly how round 7's
// journal-path finding happened — so there is one copy and cmd/bdrive calls it.
func SameOrigin(a, b string) bool {
	x, y := originOf(a), originOf(b)
	return x != "" && x == y
}

func originOf(raw string) string {
	if raw != "" && !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	scheme := strings.ToLower(u.Scheme)
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if host == "" {
		return ""
	}
	if strings.Contains(host, ":") { // IPv6 literal
		host = "[" + host + "]"
	}
	port := u.Port()
	if (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
		port = ""
	}
	if port != "" {
		host += ":" + port
	}
	return scheme + "://" + host
}

// refuseOffOriginRedirect stops a hub's 3xx from taking this device
// anywhere but the hub itself.
func refuseOffOriginRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return fmt.Errorf("stopped after 10 redirects")
	}
	if len(via) > 0 && !sameOrigin(req.URL.String(), via[0].URL.String()) {
		// Refused, not followed with a smaller payload. Every endpoint this
		// backend calls is the hub's own store API, where a 3xx is not part of
		// the contract — and following one handed a third-party host this
		// device's id, machine name and OS (and, before round 4, its token:
		// net/http only strips Authorization when the HOSTNAME changes, so a
		// port change, an https->http downgrade or a sibling subdomain kept
		// it).
		return fmt.Errorf("refusing a redirect off %s to %s", via[0].URL.Host, req.URL.Host)
	}
	return nil
}

// do sends the request with this device's credential attached, plus the
// identity headers the server's device registry records for history (name,
// OS; the server observes the IP itself).
//
// It deliberately does NOT set Accept-Encoding. net/http adds `gzip` itself
// whenever the caller has not, and transparently inflates the response — which
// is the entire pull half of transport compression, free and backward
// compatible. Setting the header here turns that off silently: the hub would
// still answer `Content-Encoding: gzip`, nothing would inflate it, and every
// blob would fail its sha check while looking like a corrupt hub.
// PermsCapability is what a client sends to say it understands a hub that
// serves per-account filtered journals: it tracks the scope tag from the store
// listing and re-pulls from zero when it changes. A hub with folder rules
// refuses a client that does not send it, because such a client resumes from a
// byte offset into a stream whose shape it cannot know has moved.
const PermsCapability = "X-Bdrive-Perms"

func (b *httpBackend) do(req *http.Request) (*http.Response, error) {
	if b.token != "" {
		req.Header.Set("Authorization", "Bearer "+b.token)
	}
	req.Header.Set(PermsCapability, "1")
	if b.device.ID != "" {
		// A journal request already named its device (nameJournalDevice); the
		// name and OS describe this machine either way.
		if req.Header.Get("X-Bdrive-Device") == "" {
			req.Header.Set("X-Bdrive-Device", b.device.ID)
		}
		req.Header.Set("X-Bdrive-Device-Name", b.device.Name)
		req.Header.Set("X-Bdrive-Os", runtime.GOOS+"/"+runtime.GOARCH)
	}
	return b.hc.Do(req)
}

var journalKeyRe = regexp.MustCompile(`^journal/([A-Za-z0-9._-]+)\.jsonl$`)

// nameJournalDevice tells the hub which device a journal request is about.
// The hub holds one request to one device's journal — the one-writer
// invariant it can't otherwise check — and a session's device is not
// necessarily this process's identity file (the sync engine only ever writes
// its own journal, so the key is the authority here).
func nameJournalDevice(req *http.Request, key string) {
	if m := journalKeyRe.FindStringSubmatch(key); m != nil {
		req.Header.Set("X-Bdrive-Device", m[1])
	}
}

func (b *httpBackend) endpoint(name string, q url.Values) string {
	s := b.base + "/api/p/" + b.project + "/store/" + name
	if len(q) > 0 {
		s += "?" + q.Encode()
	}
	return s
}

// httpError turns a non-2xx response into an error carrying the server's
// message. A 403 additionally wraps ErrForbidden: only the hub's own
// endpoints go through here, so that status is always an authorization
// answer, never a storage hiccup.
func httpError(resp *http.Response) error {
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	err := fmt.Errorf("server: %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	if resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("%w: %w", ErrForbidden, err)
	}
	return err
}

func (b *httpBackend) List(ctx context.Context, prefix string) ([]Object, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		b.endpoint("list", url.Values{"prefix": {prefix}}), nil)
	if err != nil {
		return nil, err
	}
	resp, err := b.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, httpError(resp)
	}
	var out struct {
		Objects []Object `json:"objects"`
	}
	// Bounded like every other body this package reads (httpError caps at 512
	// bytes): List is the first call of every sync cycle on every device, and
	// the hub alone chooses how much JSON it answers with — including a hub the
	// user was merely handed the URL of. Unbounded, one listing is one
	// allocation of whatever size it likes, again on the next tick.
	//
	// ponytail: 8 MiB is ~95k blob entries. Only `bdrive export` ever lists
	// blobs; a project past that needs a paginated list endpoint, not a bigger
	// cap. Truncation surfaces as a decode error, which is the retry posture.
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxListBytes)).Decode(&out); err != nil {
		return nil, fmt.Errorf("read listing: %w", err)
	}
	// The hub names its own objects, and the device believes it: these keys
	// become local journal file names (syncer.pull) and tar member names
	// (`bdrive export`). Nothing downstream re-checks the shape, so a hostile
	// or compromised hub would be choosing paths on the victim's disk. Keys
	// that are not keys are dropped rather than fatal — one bad listing must
	// not stop the rest of the project syncing.
	//
	// The rule is journal.SafePath, the repo's single spelling of "a path a
	// stranger named" — already applied at both hub ingest doors and to every
	// peer op path. Spelling it a second time here is exactly how round 7's
	// journal-path finding happened, so this calls it.
	kept := out.Objects[:0]
	for _, o := range out.Objects {
		if !safeListedKey(o.Key) {
			continue
		}
		if o.Size < 0 {
			// Not a size. It is read as a memory bound (syncer.sizeBound) and
			// written straight into a tar header by `bdrive export`.
			o.Size = 0
		}
		kept = append(kept, o)
	}
	return kept, nil
}

// safeListedKey is journal.SafePath plus a length bound per component. SafePath
// refuses control bytes, absolute and non-Clean spellings and `..`; it says
// nothing about length, and length is what turns a key into an open() the OS
// refuses with something that is not IsNotExist.
func safeListedKey(key string) bool {
	if !journal.SafePath(key) {
		return false
	}
	for _, seg := range strings.Split(key, "/") {
		if len(seg) > maxKeySegment {
			return false
		}
	}
	return true
}

func (b *httpBackend) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		b.endpoint("object", url.Values{"key": {key}}), nil)
	if err != nil {
		return nil, err
	}
	resp, err := b.do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return nil, httpError(resp)
	}
	return resp.Body, nil
}

func (b *httpBackend) Exists(ctx context.Context, key string) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		b.endpoint("exists", url.Values{"key": {key}}), nil)
	if err != nil {
		return false, err
	}
	resp, err := b.do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, httpError(resp)
	}
	var out struct {
		Exists bool `json:"exists"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxJSONBytes)).Decode(&out); err != nil {
		return false, err
	}
	return out.Exists, nil
}

// Put asks the server how to upload this key first: "direct" carries a
// presigned URL and the bytes bypass the server entirely; "server" relays
// them through it. The reader is only consumed once the destination is known.
func (b *httpBackend) Put(ctx context.Context, key string, r io.Reader, size int64) error {
	plan, err := b.sign(ctx, key, size)
	if err != nil {
		return err
	}
	if plan.Mode == "direct" {
		// "Already stored" is the one boolean that lets push advance its
		// cursor without sending a byte, and the hub writes it. Believed on
		// its own it publishes an op naming content nothing holds — the
		// device never retries, because the op is behind the cursor forever,
		// which breaks "blobs are pushed before the journal". A lying hub
		// cannot be fully caught, but it now has to lie on a SECOND endpoint,
		// and the honest failure (a storage race, a half-deleted object) is
		// caught outright. Unconfirmed means upload, never skip.
		if plan.Exists {
			if ok, err := b.Exists(ctx, key); err == nil && ok {
				return nil
			}
		}
		if plan.URL != "" && directTargetOK(b.base, plan.URL) {
			return b.putDirect(ctx, plan, r, size)
		}
		// No usable destination: relay through the hub, which already holds
		// this device's credential and is the party it chose to trust.
	}
	return b.putViaServer(ctx, plan, key, r, size)
}

// directTargetOK decides whether this device will hand a file's bytes to the
// host a hub named. Round 4 read this as "not a new capability, the hub
// already holds the data" — but at the moment the hub names the destination it
// does NOT hold the data; that is what the upload is for, so one injected sign
// response was an exfiltration channel the hub itself never sees.
//
// The device has nothing local to check a bucket hostname against, so the rule
// it can enforce is transport: a presigned upload goes over TLS, or it does not
// leave this device. S3 and GCS presign https; a plaintext object store must
// relay through the hub instead. The hub's own origin is allowed as-is, since
// that is the party the user already pointed this folder at.
//
// ponytail: transport only. Closing "an https host the hub named" needs a
// device-side storage-host allowlist — a config surface and a product
// decision, not a defense this file can invent.
func directTargetOK(base, raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return false
	}
	if u.User != nil {
		return false // credentials in the URL are not part of any presign
	}
	return strings.EqualFold(u.Scheme, "https") || SameOrigin(base, raw)
}

type putPlan struct {
	Mode    string            `json:"mode"`
	Exists  bool              `json:"exists"`
	URL     string            `json:"url"`
	Method  string            `json:"method"`
	Headers map[string]string `json:"headers"`
	// AcceptEncoding is what the hub will accept on the relayed PUT body.
	// Push cannot be unilateral the way pull is: a gzipped body posted to a
	// hub that does not inflate is stored verbatim under the sha256 of its
	// PLAINTEXT — a 400 for a blob, and a silently mis-stored journal. So the
	// client compresses only when the hub says so, and an older hub says
	// nothing at all (absent field → nil → raw). sign() runs before every
	// single put, so this costs no extra round trip and needs no config flag.
	AcceptEncoding []string `json:"accept_encoding"`
}

func (p putPlan) acceptsGzip() bool {
	for _, enc := range p.AcceptEncoding {
		if strings.EqualFold(strings.TrimSpace(enc), "gzip") {
			return true
		}
	}
	return false
}

func (b *httpBackend) sign(ctx context.Context, key string, size int64) (putPlan, error) {
	var plan putPlan
	body, err := json.Marshal(map[string]any{"key": key, "size": size})
	if err != nil {
		return plan, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.endpoint("sign", nil), bytes.NewReader(body))
	if err != nil {
		return plan, err
	}
	req.Header.Set("Content-Type", "application/json")
	nameJournalDevice(req, key)
	resp, err := b.do(req)
	if err != nil {
		return plan, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return plan, httpError(resp)
	}
	err = json.NewDecoder(io.LimitReader(resp.Body, maxJSONBytes)).Decode(&plan)
	return plan, err
}

func (b *httpBackend) putDirect(ctx context.Context, plan putPlan, r io.Reader, size int64) error {
	method := plan.Method
	if method == "" {
		method = http.MethodPut
	}
	req, err := http.NewRequestWithContext(ctx, method, plan.URL, r)
	if err != nil {
		return err
	}
	req.ContentLength = size
	for k, v := range plan.Headers {
		req.Header.Set(k, v)
	}
	resp, err := b.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Deliberately not httpError: this response comes from the object
		// store, not the hub, and its 403 means an expired presigned URL —
		// mapping it to ErrForbidden would park the device in permanent
		// read-only over a transient signing problem.
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("direct upload: %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	return nil
}

// putViaServer relays the bytes through the hub, gzipping them when the hub
// advertised that it inflates (plan.AcceptEncoding) and the content is worth
// compressing. putDirect deliberately stays raw: a presigned upload lands in
// the object store under the sha256 of the plaintext, with no hub in the path
// to inflate it, so compressing that leg would corrupt content addressing at
// rest.
func (b *httpBackend) putViaServer(ctx context.Context, plan putPlan, key string, r io.Reader, size int64) error {
	gzipped := false
	if plan.acceptsGzip() {
		probed, worth, err := Compressible(r)
		if err != nil {
			return err
		}
		r, gzipped = probed, worth
	}
	if gzipped {
		pr, pw := io.Pipe()
		src := r
		go func() {
			gz := gzip.NewWriter(pw)
			_, err := io.Copy(gz, src)
			if cerr := gz.Close(); err == nil {
				err = cerr
			}
			pw.CloseWithError(err)
		}()
		r = pr
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut,
		b.endpoint("object", url.Values{"key": {key}}), r)
	if err != nil {
		return err
	}
	nameJournalDevice(req, key)
	req.ContentLength = size
	if gzipped {
		req.Header.Set("Content-Encoding", "gzip")
		// The compressed length is not knowable without compressing twice, so
		// the request goes out chunked. The hub's spool() already treats a -1
		// length as the normal case — it is why it measures the body instead of
		// believing a header.
		req.ContentLength = -1
	}
	resp, err := b.do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return httpError(resp)
	}
	return nil
}

// ReportReads sends the device's queued agent reads to the hub's read
// ledger, where they count as agent traffic (actor = this device).
func (b *httpBackend) ReportReads(ctx context.Context, reads []ReadEvent) error {
	body, err := json.Marshal(map[string]any{"reads": reads})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		b.base+"/api/p/"+b.project+"/reads", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return httpError(resp)
	}
	return nil
}

// Scope asks the hub what this account may write in this project. A hub that
// predates folder permissions answers 404, which is reported as ErrNoScope so
// the caller can tell "this hub has no opinion" from "the hub is unreachable"
// — the first means sync everything, the second means keep the last answer.
func (b *httpBackend) Scope(ctx context.Context) (Scope, error) {
	var sc Scope
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		b.base+"/api/p/"+b.project+"/scope", nil)
	if err != nil {
		return sc, err
	}
	resp, err := b.do(req)
	if err != nil {
		return sc, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return sc, ErrNoScope
	}
	if resp.StatusCode != http.StatusOK {
		return sc, httpError(resp)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&sc); err != nil {
		return sc, err
	}
	return sc, nil
}

func (b *httpBackend) Close() error { return nil }
