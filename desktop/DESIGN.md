# BearDrive Desktop (macOS) — v1 design

Decisions locked in the 2026-08-18 interview (Snow):

## What it is

A shipped, distributable Mac app that duplicates the hub web app's viewer
experience over the machine's **local** BearDrive state — the folders this
device already syncs — instead of a remote hub. Menu bar icon (sync status at
a glance, pause/resume, sync-now) plus a main window with the full browser UI.

## Architecture

```
Tauri shell (menu bar + window)
   │ spawns as sidecar, localhost only
   ▼
bdrive desktop-serve  (Go, reuses internal/webapp + journal + store + config)
   │ serves the SAME /api/* contract the hub serves
   ▼
local state: mounts.json → projects; working folders → file browse;
             ~/.bdrive/volumes/<mount-id>/ journals+blobs → history;
             hub API (saved device token) → read heat only
```

- **Shell**: Tauri 2. Menu bar (tray) + main window pointed at the sidecar's
  localhost URL. No webview-side logic beyond spawn/health/quit.
- **Sidecar**: a Go server mode in this repo reusing `internal/webapp`. Each
  entry in `mounts.json` appears as a "project"; browsing reads the working
  folder; history replays the mount's local volume-store journals and serves
  old versions from local blobs; the read-heat dashboard proxies
  `GET /api/p/<id>/heat` to the signed-in hub with the device token.
- **Frontend**: the existing `internal/webapp/frontend` — one codebase. A
  `desktop`/local flag in `/api/config` switches off hub-only affordances
  (uploads, shares management, org/admin) instead of forking.
- **Binding**: loopback only, random port, and a per-launch bearer token the
  shell passes to the webview — localhost is not an auth boundary on a
  multi-user machine.

## Workspace root and projects (decided 2026-09-01, Snow)

A machine's BearDrive state hangs off a **workspace root** the user connects
once, holding any number of project folders beside folders BearDrive never
touches:

```
project-root/
  .bdrive/workspace.json       # workspace manifest — the root is NOT a mount
  non-beardrive-folder-1/      # untouched, never scanned
  non-beardrive-folder-2/
  beardrive-folder-1/
    .bdrive/config.json        # mount identity: m-xxxxxxxx
  beardrive-folder-2/
    .bdrive/config.json        # mount identity: m-yyyyyyyy
```

This extends the onboarding decision below (the mount is `<root>/<name>`, the
root never syncs) rather than replacing it. What is new: the root carries a
manifest, and it holds MANY projects rather than one.

### The manifest is an index, not the source of truth

Identity stays where it is today — each project folder's own
`.bdrive/config.json` (`ID`, `Volume`, `Remote`, `Include`, `PostSync`),
unchanged. The root manifest only lists which children are projects:

```json
{
  "kind": "workspace",
  "projects": [
    {"path": "beardrive-folder-1", "id": "m-xxxxxxxx"},
    {"path": "beardrive-folder-2", "id": "m-yyyyyyyy"}
  ]
}
```

Implemented as `internal/config/workspace.go`: `Workspace`, `LoadWorkspace`,
`SaveWorkspace`, `ScanWorkspace`, `RefreshWorkspace(Of)`, `InitWorkspace`.

Index and not truth, because a project folder that carries its own id keeps
the property `internal/config/project.go` states today: copy the folder to
another machine and `bdrive init` resumes the same project. Move identity up
to the root and a folder on its own means nothing. Paths are relative to the
root, so the root can be renamed or moved.

**On disagreement, the folder wins.** The manifest is rebuilt from a scan of
the root's immediate children; a stale or hand-edited entry is corrected,
never obeyed. No volume store, journal, or permission is ever keyed off it.

### The name collision, and why the manifest got its own name

This section first specified the manifest AS `.bdrive/config.json`,
distinguished by its `kind`. That does not survive contact with the agent
hooks (below), so the manifest is `.bdrive/workspace.json` — a root and a
project never share a file name, and every existing reader is correct
without knowing that workspaces exist.

The `kind` field stays as the manifest's self-description, and one Go guard
uses it: `LoadProject` refuses a config carrying `kind: workspace` and says so
("workspace root, not a project") rather than reading it as a legacy pre-id
project. That check is free there — LoadProject has already read the bytes.

`IsMount` deliberately does NOT check the kind. It runs per directory in the
syncer's walk, and reading each config to classify it puts an unbounded read
where a stat was: one wedged or unreadable config hangs a whole scan. It was
tried and reverted. The agent-hook walk-up matches on the file name for the
same reason, so **Go and shell agree** — a manifest hand-planted in
`config.json` reads as a mount to both, and neither has to know what a
workspace is.

That is the stated limit of the collision guards: they cover the case where
such a file is *loaded as a project*, not the case where its mere presence is
counted. Nothing writes that layout; it is reachable only by hand.

### The agent-hook walk-up — the constraint that set the file name

`internal/agenthooks` locates a project by walking up for
`$d/.bdrive/config.json` and stopping at the first hit. Had the manifest
lived there, the walk would succeed from inside `non-beardrive-folder-1` — a
folder that does not sync — and every tool call in it would spawn `bdrive`.

Telling the two apart in that walk means inspecting each ancestor's config.
That is affordable — `read` is a shell builtin, so it need not cost a process,
and the guard's budget is one `grep`, of `mounts.json` (CLAUDE.md;
`sec_hooks_test.go` pins it). The objection is not cost but coupling: it makes
correctness in the machine's hottest guard depend on knowing what a workspace
is. A distinct file name removes the question — the walk climbs past a root
untouched and falls through to the `mounts.json` check, which is the right
answer for a session opened at the root, without having heard of workspaces.
`TestHookGuardSkipsWorkspaceRoot` fails if the manifest is moved back into
`config.json`.

### Rules

- A workspace root is never itself a mount. `bdrive init` at a root refuses
  and points at a child folder.
- Roots do not nest, and a root is never inside a project. (Nested *mounts*
  are a different matter — they exist and the syncer handles them; the
  manifest simply indexes immediate children and leaves them alone.)
  **Enforced by no shipped code today.** Answering either rule needs a read
  per ancestor directory, and one wedged ancestor would hang the desktop
  connect at "connecting" with no cancel and no undo — so the connect applies
  `config.checkRootHere` (stat and path arithmetic only) and skips them. They
  live in `CheckRootPlacement`, whose only caller is `InitWorkspace`, which
  nothing in the product calls: `bdrive init` never designates a root, it only
  refuses to mount one. So two connects can produce a root inside a root — one
  folder with two indexes, a cosmetic fault in a file nothing resolves state
  from. The rules are kept, and tested, for a future "designate this existing
  tree" gesture that can afford to block.
- Nothing at the root syncs, the manifest included — a root is not a mount, so
  no scan ever reaches it. And if one ever did (a root inside a project, which
  nothing shipped prevents — see above), the manifest still would not sync:
  it lives in `.bdrive`, which IS in `ReservedDirs`, at any depth. Belt and
  braces, in that order.
- Deleting the manifest un-roots the folder. Nothing recreates it, because
  nothing can tell "this was a root" from "this never was" — the desktop
  connect flow is the only thing that designates one.

### Migration

None. A project folder with no root above it is exactly today's layout and
keeps working — no manifest, everything reads as it does now. A manifest is
written by the desktop connect flow (the one gesture where the user picks a
root) and re-indexed on every daemon start — `daemon.Run`, so `bdrive resume`
after a reboot counts, not just `bdrive init`.

That re-index runs in its own goroutine, and that placement is load-bearing.
The scan reads a directory the user chose plus one config per child, neither
bounded, so a wedged network path or a TCC-gated sibling blocks it forever.
Inline before `announce` it fails a healthy project's daemon; inline after
`announce` it is worse — the flock says "running" while the sync loop never
begins. **Sync never waits on the index.**

The rule the hard way, four times over: **`ScanWorkspace` may only be called
where blocking forever is harmless.** Today that is exactly one place, the
daemon's goroutine. It was tried in `startSync` (hung `bdrive init` and the
connect's sync step), in `init`'s containing-root guard (hung `bdrive init`),
and in the connect flow (hung the UI at "connecting", with no undo). Bounding
it with the `probe` helper does not work either: `probe` abandons a slow call
without cancelling it, so the write still lands, after the undo that was
supposed to remove it.

So the connect flow does not scan at all. `DesignateWorkspace` writes a
manifest holding the one project it just created — one stat, one atomic
write, over a directory the flow has already proven reachable — and the
daemon's refresh discovers everything else later, where it can afford to
block. That also makes un-designation safe: the write is synchronous, so
`undo` cannot lose a race with it.

### What it buys

The root becomes the thing a user connects and the Mac app can name: one
place that answers "what on this machine is BearDrive, and what isn't".
Today that answer exists only in `~/.bdrive/mounts.json` — a machine-global
registry with no relationship to the user's own folder structure.

### Status

Implemented (`internal/config/workspace.go`, `internal/daemon/daemon.go`,
`cmd/bdrive/{init,sync_run,desktop_onboard}.go`). `bdrive init` refuses a root
and names it; `daemon.Run` re-indexes the root above a project on every daemon
start, `bdrive resume` included; the desktop connect flow designates the folder
the user picked, and un-designates it if the connect then fails. Checks:
`TestWorkspace*`/`TestLoadProjectRefusesWorkspace`/
`TestIsMountFalseAtWorkspaceRoot`/`TestDesignateWorkspaceIsScanFree`/
`TestWorkspaceRootRefusalCoversTheWholeHome` (`internal/config`),
`TestHookGuardSkipsWorkspaceRoot`/`TestHookGuardStaysPureShell`
(`internal/agenthooks`), `TestWorkspaceRefreshOnDaemonStart`/
`TestWorkspaceRefreshNeverStallsTheDaemon`/
`TestWorkspaceRefreshNeverCreatesARoot` (`internal/daemon`),
`TestInitRefusesWorkspaceRoot`/`TestInitInAProjectUnderARootStillWorks`/
`TestInitNeverScansForRoots`/`TestSyncStartNeverScansTheWorkspaceRoot`/
`TestLegacyProjectUnchanged`/`TestDesktopInitFounder`/
`TestDesktopInitFailure*` (`cmd/bdrive`), `TestWorkspaceRootNeverScanned`
(`internal/syncer`). Working notes and four independent validation passes:
`.claude/workspace-root-goal.md`, `.claude/workspace-root-validation*.md`.

Not done: no UI names the root yet (the Mac app still lists mounts from
`mounts.json`); no command designates a root outside the desktop connect flow,
nor un-designates one (`config.DesignateWorkspace`/`UndesignateWorkspace` are
the seams, and deleting `.bdrive/workspace.json` is the manual undo — do it
with syncing stopped, since a refresh already in flight can rewrite it once);
and `LoadWorkspace` validates
nothing in what it returns — the first caller that builds a path from an entry
must check `Path` for traversal and `ID` with `ValidMountID`, the way
`LoadProject` does. Note also that the manifest indexes project FOLDERS, which
includes ones this device has paused or never enrolled: a UI that reads it as
"what is syncing" would be wrong.

`init` refuses a root in both directions — the root itself, and a folder that
contains one (which would sweep up everything the root exists to hold apart).
The second is answered from the mount registry alone: for every enrolled
project below the named folder it walks up to that folder looking for a root,
so a project nested any distance under its root (`<root>/team/wiki`) is found.
Only paths this device already syncs are read.
**Known gap:** a root whose projects this device never enrolled — a tree
copied from another machine — is invisible, so `init` above it is not refused.

That gap is deliberate. Closing it means reading folders the user did not
name, and the one attempt at that hung `bdrive init` forever on a single FIFO
or wedged network child. A guard that can hang the command it guards is worse
than the gap it closes.

A root is never `$HOME` or anything whose `.bdrive` contains (or sits inside)
the beardrive home — that directory holds the device token, the identity and
every project's journals, and an index has no business in it. Containment
rather than equality, and case-insensitively, since the filesystems here fold
case. Both sides are canonicalised as far as they exist (`resolveExisting`):
either the folder or the home can be spelled through a symlink, and the last
component of each — a `.bdrive` about to be created, a `$BDRIVE_HOME` on a
machine that has never synced — routinely does not exist, which is precisely
when a naive `EvalSymlinks` gives up and the alias slips through.

## Scope (v1)

In: file browser + markdown viewer (wikilinks), per-file history + old
versions, sync status per mount (daemon.lock liveness) with pause/resume/
sync-now, read-heat dashboard (hub-proxied), in-app sign-in via the existing
loopback-callback login flow when no token is saved.

Out (explicitly): local writes — no in-app editing, no uploads, not even
open-in-Finder (revisit; likely the first post-v1 ask). No org/admin
surfaces. No Windows/Linux (Tauri keeps the door open). Share links were
originally out too; Snow reversed that on 2026-08-19 — shares are HUB
state, so the sidecar proxies them (create/list/revoke/expiry) to the
project's hub, which enforces the caller's real permission; the frontend
shows Share when `config.desktop` regardless of the local perm ("read").
Browser-originated writes to the proxy pass an Origin check (any website
can POST to loopback).

## Status

- **Phase 1 — done**: `bdrive desktop` (hidden command, `cmd/bdrive/desktop.go`)
  serves the full existing frontend over local state. `webapp.Server.Desktop`
  forces every project to `PermRead` (frontend hides write affordances off the
  perm it is told; server refuses writes through the normal gate) and reports
  `desktop: true` + `reads.enabled` in `/api/config`. Local volume stores are
  adapted to the hub storage layout by `volumesBackend` (flat `blobs/<sha>` →
  sharded `blobs/<aa>/<sha>`), so `RemoteSource` — and with it history, blob
  versions and provenance — works unmodified. Projects are keyed by **hub
  project id** (parsed from each mount's remote), so URLs match the hub's and
  the heat proxy is a same-path pass-through with the saved device token.
  Integration test: `cmd/bdrive/desktop_test.go`.
- **Phase 2 — done**: the sync control API (`/api/desktop/status` +
  `pause|resume|sync` per project, custom-header CSRF guard on the POSTs —
  any web page can fire cross-origin POSTs at loopback) and the Tauri 2 shell
  (`desktop/src-tauri`): spawns the bundled sidecar, tray menu with per-mount
  state and pause/resume/sync-now, main window on the sidecar's URL, window
  close keeps the tray alive, Quit kills the sidecar. Build: see README.md.
- **Sign-in/out — done**: `/api/desktop/session` + `login` + `logout`.
  Login reuses `bdrive login`'s loopback-callback browser flow verbatim
  (`browserLogin`: PKCE, 5-minute bound, opens the default browser), one
  flow at a time (409 while one waits); logout mirrors `bdrive logout` —
  revoke on the hub FIRST, clear locally either way, keep the remembered
  server, report a failed revocation in the response. The tray shows
  "Signed in as …"/Sign out or "Sign in…", and every tray action runs off
  the main thread and refreshes the menu when it completes.
- **Onboarding — done 2026-08-21** (storyboard:
  https://claude.ai/code/artifact/d1836ae6-4878-4a11-8dd0-8e60707a008a):
  a fresh install goes DMG → Welcome (`/setup`) → browser sign-in → connect
  screen → first sync → success, with no CLI. The opinionated core: the mount
  is `<root>/<name>` (default `team`) INSIDE the user's own project — the root
  never syncs — seeded from the LLM wiki template, create-or-join by name so
  the same screen serves the second teammate. Sidecar owns it
  (`cmd/bdrive/desktop_onboard.go`: inspect / init / init-status / native
  chooser); the frontend adds desktop-gated `/setup*` routes; the tray gains
  "Connect a folder…", and **"New project" IS this flow on desktop** (2026-08-21
  owner decision): a hub project with no local folder can never appear in a
  list built from this machine's mounts, so the + routes to /setup/connect
  instead of the hub-only create dialog, which no longer renders on the Mac.
  Checks: `TestDesktopInspect|InitFounder|InitJoiner|InitRefusals`,
  `frontend/e2e/desktop-onboarding.spec.ts`, `desktop/smoke.sh`.
  Working notes and the matrix: `.claude/onboarding-goal.md`.
- **macOS privacy (TCC), known and partly worked around**: the sync daemon is
  detached (`Setsid`, so syncing outlives the app), which takes it out of the
  app's responsible-process chain — macOS sees an unsigned helper, not
  BearDrive. Consequences: a folder inside Desktop/Documents/Downloads/iCloud
  prompts for permission on EVERY arriving file, and a UI-less read of one can
  block outright. Handled today by bounding every probe with a clear message,
  priming access from the GUI process, and warning at connect time before a
  gated folder is chosen. The real fix is Developer ID signing, after which
  grants stick and the helper rides the app's identity — see phase 3.
- **Workspace root — done 2026-09-01**: the root manifest and its guards, see
  §Workspace root and projects. The manifest is `.bdrive/workspace.json`, not
  the `config.json` that section first specified — a separate name keeps the
  agent-hook walk-up correct without it having to know what a workspace is.
- **Phase 3 — remaining for "shipped"**: signing + notarization + updater;
  per-launch bearer token between shell and sidecar (loopback is not an
  auth boundary on multi-user machines); frontend `config.desktop` polish
  (hide the Installation page and the project-create "+", which today
  no-op/404 harmlessly); goreleaser/CI wiring for the app artifact.

## Repo layout

`desktop/` (Tauri app) in this repo; sidecar mode lives in `cmd/bdrive` +
existing internal packages. Release: goreleaser builds the sidecar binary;
Tauri build bundles it; signing + notarization via the standard Tauri macOS
pipeline.
