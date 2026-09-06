package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/runbear-io/beardrive/internal/config"
	"github.com/runbear-io/beardrive/internal/daemon"
	"github.com/runbear-io/beardrive/internal/remote"
	"github.com/runbear-io/beardrive/internal/store"
	"github.com/runbear-io/beardrive/internal/webapp"
)

// bdrive desktop — the BearDrive Desktop sidecar: a loopback-only server that
// renders THIS machine's mounts through the same web frontend a hub serves.
// Browsing and history come from the local volume stores (journals + blobs),
// so it works offline and needs no credentials; the read-heat dashboard is the
// one hub-backed surface, proxied per project with the saved device token.
//
// The server runs the ordinary hub mode (Root + Projects) with Auth, Dir,
// Devices, Shares and Reads all nil and Server.Desktop set: every project
// resolves to PermRead, so all write routes refuse through the existing perm
// gate and the frontend hides its write affordances off the perm it is told.
// Projects are keyed by their HUB project id (parsed from each mount's
// remote), which keeps projectIDRe valid, keeps URLs identical to the hub's,
// and makes the heat proxy a same-path pass-through.
func desktopCmd() *cobra.Command {
	var addr string
	cmd := &cobra.Command{
		Use:    "desktop",
		Short:  "Serve the local desktop viewer (loopback only)",
		Hidden: true, // spawned by the BearDrive Desktop app, not typed by users
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			host, _, err := net.SplitHostPort(addr)
			if err != nil || host == "" || !net.ParseIP(host).IsLoopback() {
				return fmt.Errorf("desktop serves loopback only; got --addr %q", addr)
			}
			ln, err := net.Listen("tcp", addr)
			if err != nil {
				return err
			}
			fmt.Printf("BearDrive Desktop on http://%s\n", ln.Addr())
			startReadReporter(cmd.Context())
			return http.Serve(ln, desktopHandler())
		},
	}
	cmd.Flags().StringVar(&addr, "addr", "127.0.0.1:8990", "loopback listen address")
	return cmd
}

// desktopHandler builds the full handler: the hub-mode webapp over the local
// volume stores, with GET /api/p/<id>/heat intercepted and proxied to the
// project's own hub.
// routeMode says where the desktop answers a per-project hub route.
type routeMode int

const (
	// routeLocal falls through to the embedded webapp.Server and is answered
	// from this machine's own state. Requires a written reason.
	routeLocal routeMode = iota
	// routeProxy forwards to the project's own hub with the device token.
	routeProxy
)

// desktopRoutes classifies every per-project route the hub serves, and the
// routeProxy rows are THE registration — desktopHandler ranges over this table
// rather than repeating it, so "classified as proxied" and "actually proxied"
// cannot drift apart.
//
// It exists because the desktop mux falls through to a local server, which
// makes forgetting a route invisible: the local registry answers plausibly and
// WRONGLY. That has shipped twice — an empty grants list (/permissions), then
// a project whose folder rules all vanished (/folders, /scope). Neither looked
// like a failure from the app.
//
// TestDesktopRoutesClassified fails when the hub grows a per-project route
// that is not listed here, so the next person gets a red build instead of a
// quiet wrong answer. It cannot tell you a routeLocal call was WRONG, only
// that somebody made it — but both misses so far were nobody deciding at all.
var desktopRoutes = []struct {
	pattern string
	mode    routeMode
	why     string // required for routeLocal: why local is the right answer
}{
	// Browsing and history are the whole point of the app: they read this
	// machine's working folder and volume store, and keep working offline.
	{"GET /api/projects/{project}", routeLocal, "name and created come from mounts.json; the level is PermRead by design"},
	{"GET /api/p/{project}/tree", routeLocal, "the local working folder"},
	{"GET /api/p/{project}/resolve", routeLocal, "wikilink resolution over the local tree"},
	{"GET /api/p/{project}/file", routeLocal, "local bytes"},
	{"GET /api/p/{project}/download", routeLocal, "local bytes"},
	{"GET /api/p/{project}/render", routeLocal, "local bytes, rendered here"},
	{"GET /api/p/{project}/history", routeLocal, "replayed from the local volume store's journals"},
	{"GET /api/p/{project}/blob", routeLocal, "old versions come from local blobs"},
	{"GET /api/p/{project}/store/list", routeLocal, "the local volume store"},
	{"GET /api/p/{project}/store/object", routeLocal, "the local volume store"},
	{"GET /api/p/{project}/store/exists", routeLocal, "the local volume store"},

	// Writes never happen locally: one journal, one writer, and the daemon is
	// it. These reach the hub, which journals under this account and applies
	// its real permission; the local RemoteSource picks the change up on
	// refresh like any other peer edit.
	{"PUT /api/p/{project}/store/object", routeLocal, "refused, deliberately: the daemon is the volume store's only writer"},
	{"POST /api/p/{project}/store/sign", routeLocal, "refused: the local backend presigns nothing, and a device syncing this Mac talks to its hub directly"},
	{"POST /api/p/{project}/restore", routeProxy, ""},
	{"POST /api/p/{project}/remove", routeProxy, ""},
	{"POST /api/p/{project}/undo-run", routeProxy, ""},
	{"PATCH /api/projects/{project}", routeProxy, ""},
	{"DELETE /api/projects/{project}", routeProxy, ""},
	{"POST /api/p/{project}/upload/init", routeProxy, ""},
	{"PUT /api/p/{project}/upload/content", routeProxy, ""},
	{"POST /api/p/{project}/upload/commit", routeProxy, ""},

	// Hub-owned metadata. Every one of these is a list the local registry
	// would answer as empty, which reads as "nothing to see" rather than as a
	// failure — the exact shape of both misses so far.
	{"GET /api/p/{project}/heat", routeProxy, ""},
	{"GET /api/p/{project}/shares", routeProxy, ""},
	{"POST /api/p/{project}/shares", routeProxy, ""},
	{"GET /api/p/{project}/permissions", routeProxy, ""},
	{"PUT /api/p/{project}/permissions", routeProxy, ""},
	{"PUT /api/p/{project}/permissions/{email}", routeProxy, ""},
	{"DELETE /api/p/{project}/permissions/{email}", routeProxy, ""},
	{"GET /api/p/{project}/scope", routeProxy, ""},
	{"GET /api/p/{project}/folders", routeProxy, ""},
	{"PUT /api/p/{project}/folders", routeProxy, ""},
	{"DELETE /api/p/{project}/folders", routeProxy, ""},

	// Live surfaces. Hub state for the same reason every write is: the desktop
	// never journals locally, so nothing here would ever publish, and a stream
	// served from local state would be a connection that is open and silent
	// forever. /collab carries the shared editing document — without it the
	// app's editor opens on a document that never arrives, because it mounts
	// on the relay's first frame.
	{"GET /api/p/{project}/events", routeProxy, ""},
	{"POST /api/p/{project}/presence", routeProxy, ""},
	{"GET /api/p/{project}/collab", routeProxy, ""},
	{"POST /api/p/{project}/collab", routeProxy, ""},

	{"POST /api/p/{project}/reads", routeLocal, "the sync client posts these straight to the hub, never through here; the app's own viewer reads go out through desktop_reads.go instead, as human traffic"},
}

func desktopHandler() http.Handler {
	// Sign-ins from here are the app's, not the CLI's: the hub records
	// "<host> (BearDrive Desktop)" for the device.
	appLabel = "BearDrive Desktop"
	srv := &webapp.Server{
		Root:     &volumesBackend{stores: map[string]*store.Store{}},
		Projects: mustProjects(),
		Volume:   "BearDrive",
		Refresh:  2 * time.Second,
		Desktop:  true,
		// Reported in /api/config so the frontend offers project creation
		// (canCreate); the create and its uploads are hub proxies below.
		// Local write routes stay refused through the PermRead resolver.
		Upload: webapp.UploadConfig{Enabled: true},
		// Viewer reads: this server keeps no ledger, so they go to the
		// project's own hub as human traffic (desktop_reads.go). Without it a
		// person reading in the Mac app was counted nowhere.
		ReportRead: spoolRead,
		DesktopMe: func() (string, string) {
			s, _ := config.LoadSettings()
			if s.Token == "" {
				return "", "" // signed out: no me, whatever stale fields remain
			}
			return s.Email, s.Name
		},
	}
	mux := http.NewServeMux()
	// Every per-project hub route is classified in desktopRoutes below, and the
	// proxies are registered FROM it — see the table for why that matters.
	for _, rt := range desktopRoutes {
		if rt.mode != routeProxy {
			continue // falls through to srv, answered from this machine
		}
		h := http.HandlerFunc(proxyProject)
		if !strings.HasPrefix(rt.pattern, "GET ") {
			h = originGuard(h) // every proxied write, exactly as before
		}
		mux.HandleFunc(rt.pattern, h)
	}
	// Project creation and its template seeding are hub writes (2026-08-20
	// owner decision): create goes to the signed-in hub, uploads go to the
	// project's hub — for a just-created project that's the default-hub
	// fallback in proxyProject. Known gap: the new project has no local
	// folder until `bdrive init` links one, so the app can't browse it yet.
	mux.HandleFunc("POST /api/projects", originGuard(proxyDefaultHub))
	// Orgs live on the hub: the local registry has none, so the empty list the
	// local handler returns would hide the user's real org (and with it the
	// invite link the onboarding success screen offers). But the app must keep
	// working with no hub at all — the whole point of local-first — and the
	// shell blocks on this list, so an unreachable hub answers "no orgs"
	// rather than hanging the window on Loading…
	mux.HandleFunc("GET /api/orgs", proxyOrEmpty)
	mux.HandleFunc("POST /api/orgs/{org}/invites", originGuard(proxyDefaultHub))
	mux.HandleFunc("PATCH /api/shares/{token}", originGuard(proxyDefaultHub))
	mux.HandleFunc("DELETE /api/shares/{token}", originGuard(proxyDefaultHub))
	mux.HandleFunc("GET /api/desktop/status", desktopStatus)
	mux.HandleFunc("GET /api/desktop/session", desktopSession)
	mux.HandleFunc("POST /api/desktop/login", guarded(desktopLogin))
	mux.HandleFunc("POST /api/desktop/logout", guarded(desktopLogout))
	// Onboarding (desktop_onboard.go): inspect a candidate folder, then
	// create-and-connect a shared folder inside it. The writes are guarded
	// like every other side-effecting desktop route.
	mux.HandleFunc("GET /api/desktop/inspect", handleDesktopInspect)
	mux.HandleFunc("GET /api/desktop/init/status", handleDesktopInitStatus)
	mux.HandleFunc("POST /api/desktop/init", guarded(handleDesktopInit))
	mux.HandleFunc("POST /api/desktop/choose-folder", guarded(handleDesktopChooseFolder))
	mux.HandleFunc("POST /api/desktop/p/{project}/pause", guarded(desktopPause))
	mux.HandleFunc("POST /api/desktop/p/{project}/resume", guarded(desktopResume))
	mux.HandleFunc("POST /api/desktop/p/{project}/sync", guarded(desktopSync))
	mux.Handle("/", srv.Handler())
	return mux
}

// guarded refuses POSTs that lack the X-Bdrive-Desktop header. Loopback is
// reachable by any web page the user's browser has open — a cross-origin form
// POST fires without a preflight, so a drive-by page could pause syncing. A
// custom header forces the preflight, which this server never approves; the
// tray and the frontend set it trivially.
func guarded(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Bdrive-Desktop") == "" {
			http.Error(w, "missing X-Bdrive-Desktop header", http.StatusForbidden)
			return
		}
		h(w, r)
	}
}

// mountOf resolves the {project} path value, answering the request on a miss.
func mountOf(w http.ResponseWriter, r *http.Request) (desktopMount, bool) {
	m, ok := desktopMounts()[r.PathValue("project")]
	if !ok {
		http.Error(w, "no such project", http.StatusNotFound)
	}
	return m, ok
}

func writeDesktopJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// desktopStatus reports every mount's sync state: daemon liveness is the
// daemon.lock flock (never the pidfile), pause is the store's paused marker.
func desktopStatus(w http.ResponseWriter, _ *http.Request) {
	type mountStatus struct {
		Project string `json:"project"`
		Name    string `json:"name"`
		Path    string `json:"path"`
		// Server is the project's hub base URL — what turns a desktop route
		// into a shareable web URL (hub and desktop use identical paths).
		Server  string `json:"server"`
		Running bool   `json:"running"`
		Pid     int    `json:"pid,omitempty"`
		Paused  bool   `json:"paused"`
	}
	out := []mountStatus{}
	for _, m := range desktopMounts() {
		pid, running := daemon.Running(m.volDir)
		out = append(out, mountStatus{
			Project: m.hubID, Name: filepath.Base(m.path), Path: m.path, Server: m.server,
			Running: running, Pid: pid, Paused: store.Paused(m.volDir),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	writeDesktopJSON(w, map[string]any{"mounts": out})
}

// desktopSession reports this device's saved sign-in, for the tray.
func desktopSession(w http.ResponseWriter, _ *http.Request) {
	s, _ := config.LoadSettings()
	server := s.Server
	if server == "" {
		server = config.DefaultServer
	}
	writeDesktopJSON(w, map[string]any{
		"server": server, "signed_in": s.Token != "", "email": s.Email, "name": s.Name,
	})
}

// loginMu serializes sign-ins: a second click while a browser flow waits
// would open a second tab racing the first for settings.json.
var loginMu sync.Mutex

// desktopLogin runs the same loopback-callback browser flow as `bdrive
// login`: it opens the user's default browser on the hub's sign-in page and
// blocks until the callback lands (browserLogin bounds the wait at 5
// minutes). The tray calls it from a background thread.
func desktopLogin(w http.ResponseWriter, r *http.Request) {
	if !loginMu.TryLock() {
		http.Error(w, "a sign-in is already waiting in the browser", http.StatusConflict)
		return
	}
	defer loginMu.Unlock()
	var req struct {
		Server string `json:"server"`
	}
	json.NewDecoder(io.LimitReader(r.Body, 1<<12)).Decode(&req) // empty body = remembered server
	settings, _ := config.LoadSettings()
	server := strings.TrimSuffix(req.Server, "/")
	if server == "" {
		server = settings.Server
	}
	if server == "" {
		server = config.DefaultServer
	}
	if u, err := url.Parse(server); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		http.Error(w, fmt.Sprintf("invalid server URL %q", server), http.StatusBadRequest)
		return
	}
	cfg, err := fetchServerConfig(server)
	if err != nil {
		http.Error(w, "cannot reach bdrive server at "+server+": "+err.Error(), http.StatusBadGateway)
		return
	}
	if !cfg.Auth.Enabled {
		if err := config.SaveSettings(config.Settings{Server: server}); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeDesktopJSON(w, map[string]any{"ok": true, "signed_in": false, "server": server})
		return
	}
	loginPath := cfg.Auth.CLILogin
	if loginPath == "" {
		loginPath = "/auth/cli"
	}
	token, user, err := browserLogin(server, loginPath)
	if err != nil {
		http.Error(w, "sign-in failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	if err := config.SaveSettings(config.Settings{Server: server, Token: token, Email: user.Email, Name: user.Name}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeDesktopJSON(w, map[string]any{"ok": true, "signed_in": true, "server": server, "email": user.Email, "name": user.Name})
}

// desktopLogout is `bdrive logout` without --forget: end the token on the hub
// first, clear it locally either way, keep the remembered server. A failed
// revocation is reported, never swallowed — the token then lives on
// server-side until the next logout reaches the hub.
func desktopLogout(w http.ResponseWriter, _ *http.Request) {
	settings, err := config.LoadSettings()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	out := map[string]any{"ok": true}
	if settings.Token != "" && settings.Server != "" {
		if err := revokeOnServer(settings.Server, settings.Token); err != nil {
			out["revoke_error"] = err.Error()
		}
	}
	settings.Token, settings.Email, settings.Name = "", "", ""
	if err := config.SaveSettings(settings); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeDesktopJSON(w, out)
}

// desktopPause is `bdrive stop` without --forget: stop the daemon, and leave
// a pause that outlives it so agent hooks don't silently resume.
func desktopPause(w http.ResponseWriter, r *http.Request) {
	m, ok := mountOf(w, r)
	if !ok {
		return
	}
	stopped, err := daemon.Stop(m.volDir)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := store.SetPaused(m.volDir, true); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeDesktopJSON(w, map[string]any{"ok": true, "stopped": stopped})
}

// desktopResume clears the pause and restarts the daemon — the same consent
// gesture as re-running `bdrive init` in the folder. The daemon start is
// best-effort: syncing is unpaused either way, and the next bdrive command in
// the folder starts a daemon regardless (self-heal on next touch).
func desktopResume(w http.ResponseWriter, r *http.Request) {
	m, ok := mountOf(w, r)
	if !ok {
		return
	}
	if err := store.SetPaused(m.volDir, false); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	out := map[string]any{"ok": true}
	if _, running := daemon.Running(m.volDir); !running {
		if pid, err := daemon.Start(m.path, m.volDir, 3*time.Second, 10*time.Second); err != nil {
			out["daemon_error"] = err.Error()
		} else {
			out["pid"] = pid
		}
	}
	writeDesktopJSON(w, out)
}

// desktopSync runs one sync cycle now, exactly like `bdrive sync` in the
// folder: refused while paused, degrades to offline rather than failing.
func desktopSync(w http.ResponseWriter, r *http.Request) {
	m, ok := mountOf(w, r)
	if !ok {
		return
	}
	proj, ok, err := config.LoadProject(m.path)
	if err != nil || !ok {
		http.Error(w, "folder is not a beardrive project any more", http.StatusConflict)
		return
	}
	if blocked := syncBlocked(proj); blocked != "" {
		http.Error(w, "syncing is "+blocked+" for this folder", http.StatusConflict)
		return
	}
	sess, _, err := openSession(r.Context(), m.path, true)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer closeSession(sess)
	res, err := sess.Cycle(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeDesktopJSON(w, map[string]any{
		"ok": true, "offline": res.Offline, "local_ops": res.LocalOps,
		"pulled_ops": res.PulledOps, "materialized": res.Materialized,
	})
}

func mustProjects() *webapp.ProjectDB {
	db, err := webapp.NewProjectDB(desktopProjects{})
	if err != nil {
		panic(err) // desktopProjects.Load never fails hard; see below
	}
	return db
}

// desktopMount is one entry of mounts.json resolved into hub-project shape.
type desktopMount struct {
	hubID  string // project id on the hub, from the mount's remote URL
	server string // hub base URL, from the same remote
	path   string // working folder (display name source)
	volDir string // ~/.bdrive/volumes/<mount-id>
}

// desktopMounts enumerates this machine's mounts, keyed by hub project id.
// Mounts whose remote is not a hub URL (nothing writes those any more) or
// whose folder config is unreadable are skipped, not fatal: the viewer shows
// what it can.
func desktopMounts() map[string]desktopMount {
	mounts, err := config.LoadMounts()
	if err != nil {
		return nil
	}
	out := make(map[string]desktopMount, len(mounts))
	for id, mi := range mounts {
		proj, ok, err := config.LoadProject(mi.Path)
		remoteURL := mi.Remote
		if err == nil && ok && proj.Remote != "" {
			remoteURL = proj.Remote // folder config is the fresher truth
		}
		server, hubID, err := splitHubRemote(remoteURL)
		if err != nil {
			continue
		}
		volDir, err := config.VolumeDir(id)
		if err != nil {
			continue
		}
		out[hubID] = desktopMount{hubID: hubID, server: server, path: mi.Path, volDir: volDir}
	}
	return out
}

// desktopProjects is a read-only webapp.ProjectRepo over mounts.json. It
// re-enumerates on every Load (ProjectDB re-Loads a non-Versioned repo per
// read), so a `bdrive init` run while the app is open shows up without a
// restart — a couple of file reads per request, all local.
type desktopProjects struct{}

func (desktopProjects) Load() ([]webapp.Project, error) {
	var out []webapp.Project
	for _, m := range desktopMounts() {
		p := webapp.Project{ID: m.hubID, Name: filepath.Base(m.path)}
		if fi, err := os.Stat(m.path); err == nil {
			p.Created = fi.ModTime()
		}
		out = append(out, p)
	}
	return out, nil
}

func (desktopProjects) Put(webapp.Project) error { return errors.New("desktop projects are read-only") }
func (desktopProjects) Delete(string) error      { return errors.New("desktop projects are read-only") }

// volumesBackend maps the hub storage layout the webapp reads
// (<project-id>/journal/<device>.jsonl, <project-id>/blobs/<sha>) onto the
// machine's local volume stores, which shard blobs as blobs/<aa>/<sha>.
// Strictly read-only: Put refuses, and nothing here ever touches a journal —
// the daemon stays the volume store's only writer.
type volumesBackend struct {
	mu     sync.Mutex
	stores map[string]*store.Store // hub project id → opened store
}

func (b *volumesBackend) store(hubID string) (*store.Store, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if st, ok := b.stores[hubID]; ok {
		return st, nil
	}
	m, ok := desktopMounts()[hubID]
	if !ok {
		return nil, fs.ErrNotExist
	}
	st, err := store.Open(m.volDir)
	if err != nil {
		return nil, err
	}
	b.stores[hubID] = st
	return st, nil
}

// split carves "<hub-id>/<kind>/<rest>" into its store and in-store key.
func (b *volumesBackend) split(key string) (*store.Store, string, error) {
	hubID, rest, ok := strings.Cut(key, "/")
	if !ok {
		return nil, "", fs.ErrNotExist
	}
	st, err := b.store(hubID)
	if err != nil {
		return nil, "", err
	}
	return st, rest, nil
}

func (b *volumesBackend) Get(_ context.Context, key string) (io.ReadCloser, error) {
	st, rest, err := b.split(key)
	if err != nil {
		return nil, err
	}
	switch {
	case strings.HasPrefix(rest, "journal/") && strings.HasSuffix(rest, ".jsonl"):
		dev := strings.TrimSuffix(strings.TrimPrefix(rest, "journal/"), ".jsonl")
		return os.Open(st.JournalPath(dev))
	case strings.HasPrefix(rest, "blobs/"):
		return st.OpenBlob(strings.TrimPrefix(rest, "blobs/"))
	}
	return nil, fs.ErrNotExist // no chunks/ or manifests/ locally
}

func (b *volumesBackend) List(_ context.Context, prefix string) ([]remote.Object, error) {
	hubID, rest, _ := strings.Cut(prefix, "/")
	if rest != "journal/" {
		return nil, nil // the webapp only ever lists journals
	}
	st, err := b.store(hubID)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	devs, err := st.Devices()
	if err != nil {
		return nil, err
	}
	var out []remote.Object
	for _, dev := range devs {
		fi, err := os.Stat(st.JournalPath(dev))
		if err != nil {
			continue // vanished mid-list; next refresh catches up
		}
		out = append(out, remote.Object{
			Key: hubID + "/journal/" + dev + ".jsonl", Size: fi.Size(), Modified: fi.ModTime(),
		})
	}
	return out, nil
}

func (b *volumesBackend) Exists(_ context.Context, key string) (bool, error) {
	st, rest, err := b.split(key)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	if sha, ok := strings.CutPrefix(rest, "blobs/"); ok {
		return st.HasBlob(sha), nil
	}
	rc, err := b.Get(context.Background(), key)
	if err != nil {
		return false, nil
	}
	rc.Close()
	return true, nil
}

func (b *volumesBackend) Put(context.Context, string, io.Reader, int64) error {
	return errors.New("the desktop viewer is read-only")
}

func (b *volumesBackend) Close() error { return nil }

// originGuard refuses browser writes that did not come from our own page.
// Loopback is reachable by any website the user has open, and a cross-site
// fetch/form POST fires without a preflight; browsers stamp those with the
// attacker's Origin. Absent Origin = not a browser (curl, the tray) = fine.
func originGuard(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if o := r.Header.Get("Origin"); o != "" {
			if u, err := url.Parse(o); err != nil || u.Host != r.Host {
				http.Error(w, "cross-origin write refused", http.StatusForbidden)
				return
			}
		}
		h(w, r)
	}
}

// proxyProject forwards a per-project request to that project's own hub,
// authenticated with this device's saved token. Same path both sides — the
// desktop keys projects by hub id — so only host and Authorization change,
// and the hub enforces the caller's real permission on its side.
func proxyProject(w http.ResponseWriter, r *http.Request) {
	if m, ok := desktopMounts()[r.PathValue("project")]; ok {
		proxyHub(w, r, m.server)
		return
	}
	// Not one of this machine's mounts — but it may still be the caller's
	// project on the signed-in hub (a project just created from the app has
	// no local folder yet; template seeding uploads into it immediately).
	// The hub enforces membership either way.
	proxyDefaultHub(w, r)
}

// proxyDefaultHub forwards a token-scoped request (share revoke/expiry —
// nothing in the path names a project) to the signed-in hub.
// ponytail: a mount syncing through a DIFFERENT hub than the signed-in one
// can't manage its shares from the app; per-token server resolution needs a
// share→project index the desktop doesn't have.
// proxyOrEmpty forwards a read to the signed-in hub and degrades to an empty
// list when that is not possible (signed out, offline, hub error). Sync itself
// degrades to offline rather than failing; a read the UI blocks on must too.
func proxyOrEmpty(w http.ResponseWriter, r *http.Request) {
	settings, _ := config.LoadSettings()
	if settings.Server == "" || settings.Token == "" {
		writeDesktopJSON(w, map[string]any{"orgs": []any{}})
		return
	}
	rec := httptest.NewRecorder()
	proxyHub(rec, r, settings.Server)
	if rec.Code != http.StatusOK {
		writeDesktopJSON(w, map[string]any{"orgs": []any{}})
		return
	}
	for k, v := range rec.Header() {
		w.Header()[k] = v
	}
	w.WriteHeader(rec.Code)
	w.Write(rec.Body.Bytes())
}

func proxyDefaultHub(w http.ResponseWriter, r *http.Request) {
	settings, _ := config.LoadSettings()
	if settings.Server == "" {
		http.Error(w, "not signed in to a hub", http.StatusBadGateway)
		return
	}
	proxyHub(w, r, settings.Server)
}

func proxyHub(w http.ResponseWriter, r *http.Request, server string) {
	settings, _ := config.LoadSettings()
	token := os.Getenv("BDRIVE_TOKEN")
	if token == "" {
		token = settings.Token
	}
	u := server + r.URL.Path
	if r.URL.RawQuery != "" {
		u += "?" + r.URL.RawQuery
	}
	// Streamed, not buffered: upload/content bodies are whole files, and the
	// sha the hub checks is a property of the exact bytes.
	req, err := http.NewRequestWithContext(r.Context(), r.Method, u, r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	req.ContentLength = r.ContentLength
	if ct := r.Header.Get("Content-Type"); ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	if token != "" && tokenGoesTo(server) {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	// A long-lived stream cannot use initClient: its 10s whole-request timeout
	// is right for every JSON call here and fatal for a connection whose whole
	// job is to stay open with nothing to say.
	client := initClient
	if streaming(r) {
		client = streamClient
	}
	resp, err := client.Do(req)
	if err != nil {
		http.Error(w, "hub unreachable: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
	w.WriteHeader(resp.StatusCode)
	if isEventStream(resp) {
		// io.Copy alone buffers: an SSE frame is ~100 bytes and the response
		// writer holds 4 KiB, so events would sit here until something else
		// filled the buffer — which, on an idle project, is never.
		copyFlushing(w, resp.Body)
		return
	}
	io.Copy(w, resp.Body)
}

// streaming reports whether this request is for a long-lived response, which
// is a property of the ROUTE (the client asks for one) rather than of the
// answer — the client has to be chosen before the answer exists.
func streaming(r *http.Request) bool {
	return strings.HasSuffix(r.URL.Path, "/events") ||
		(r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/collab")) ||
		strings.Contains(r.Header.Get("Accept"), "text/event-stream")
}

func isEventStream(resp *http.Response) bool {
	return strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream")
}

// streamClient has no whole-request timeout. Everything else about it matches
// initClient, redirect policy included: a stream must not carry this device's
// token off-origin either.
var streamClient = &http.Client{CheckRedirect: dropTokenOffOrigin}

func copyFlushing(w http.ResponseWriter, r io.Reader) {
	rc := http.NewResponseController(w)
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if ferr := rc.Flush(); ferr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}
