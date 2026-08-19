---
name: mac-app
description: "Build, verify, and ship a test build of BearDrive Desktop (the Tauri mac app in desktop/ + the `bdrive desktop` sidecar): run the Go gates, rebuild the stamped universal .app via desktop/dist.sh, smoke-test it locally against real state, and Taildrop it to a test machine with the manual checklist. Use when asked to build the mac app, test the desktop app, ship a build to another machine, or verify a desktop change end to end. Args: [taildrop-target] (optional, e.g. macbook-pro-6)"
---

# Build & test BearDrive Desktop

The app is two pieces: the Go sidecar (`bdrive desktop`, all product logic —
`cmd/bdrive/desktop.go`) and a deliberately dumb Tauri shell
(`desktop/src-tauri`). Most changes are sidecar changes; test those as Go
first and treat the app build as packaging. Design + status:
`desktop/DESIGN.md`.

## 1. Gates (before any build)

- `go test ./cmd/bdrive -run TestDesktop` — the desktop integration tests
  (viewer, perms, control API, share proxy, session flow).
- Touched `internal/daemon`? `go test ./internal/daemon` too.
- Touched `internal/webapp/frontend/src`? Rebuild the committed assets
  (`npm run build` in the frontend dir — `npm ci` first in a fresh worktree)
  and verify with `internal/webapp/frontend/check-dist.sh`. The app embeds
  `static/`, so a stale build ships silently.

## 2. Build (+ optional ship)

    ./desktop/dist.sh [taildrop-target]

Builds the stamped universal .app and zip
(`BearDrive-<ver>-dev-g<sha>[.dirty].zip` under
`desktop/src-tauri/target/universal-apple-darwin/release/bundle/macos/`);
with a target arg it also `tailscale file cp`s the zip there (lands in that
machine's Downloads). Prereqs it assumes: `npm install` run once in
`desktop/`, rustup via Homebrew keg (script sets the PATH), x86_64 target
added (`rustup target add x86_64-apple-darwin`).

## 3. Local smoke (this machine, real state)

    osascript -e 'quit app "BearDrive"'; sleep 2
    open <bundle>/BearDrive.app; sleep 5
    curl -s http://127.0.0.1:8990/api/config          # desktop:true, mode hub
    curl -s http://127.0.0.1:8990/api/desktop/status  # real mounts, daemons running
    <bundle>/BearDrive.app/Contents/MacOS/bdrive version   # the stamp you just built

For UI checks, drive headless Chromium with the frontend's own Playwright
(`internal/webapp/frontend/node_modules`) against `http://127.0.0.1:8990` —
scripts must live IN that frontend dir for module resolution (copy in, run,
delete). Tray/menu checks: `osascript` System Events clicks + `screencapture
-R` region grabs (Screen Recording permission may prompt once; never click
system permission dialogs — leave them for Snow).

**Never test on this machine**: tray Sign out / `POST /api/desktop/logout`
(revokes Snow's real token; the Go test covers revocation), and
pause/resume on real mounts only if immediately restored.

## 4. Remote checklist (the test machine)

What only a second machine can prove — send this with the build:

1. Unzip in Downloads; Gatekeeper on an unsigned build: right-click → Open
   or `xattr -cr BearDrive.app`. (Taildrop/scp usually skip quarantine;
   test a browser transfer once per release cycle to see what users see.)
2. Menu-bar bear appears; tray shows THAT machine's state ("No synced
   folders yet" on a fresh one).
3. Tray "Sign in…" → browser flow → "Signed in as …".
4. A project (existing mount, or `…/BearDrive.app/Contents/MacOS/bdrive
   init` in a folder) appears in the app; file pages render with the Share
   button; Cmd+[ / Cmd+] navigate; History shows provenance.
5. Upgrade-in-place when replacing an older build: daemons keep running or
   self-heal on next touch.
6. Quit BearDrive → `curl 127.0.0.1:8990` fails (sidecar died with it).

Confirm the build identity on the remote machine with `bdrive version`
before filing any finding — an unstamped or mismatched build voids the run.
