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
  "Connect a folder…". Checks: `TestDesktopInspect|InitFounder|InitJoiner|
  InitRefusals`, `frontend/e2e/desktop-onboarding.spec.ts`, `desktop/smoke.sh`.
  Working notes and the matrix: `.claude/onboarding-goal.md`.
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
