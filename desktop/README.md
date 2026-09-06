# BearDrive Desktop (macOS)

A menu-bar Mac app that shows this machine's synced BearDrive projects
through the same web UI a hub serves — browsing, rendered markdown, per-file
history — read from the local volume stores, offline-capable, read-only. The
tray menu shows each mount's sync state with pause/resume/sync-now. Design
and status: [DESIGN.md](DESIGN.md).

Two pieces:

- **Sidecar** — `bdrive desktop` (hidden CLI command, `cmd/bdrive/desktop.go`):
  loopback-only server over `$BDRIVE_HOME`, plus the `/api/desktop/*` sync
  control API. All product logic lives here, in Go.
- **Shell** — this directory: a Tauri 2 app (`src-tauri/`) that spawns the
  sidecar, shows the tray menu, and opens a window on the sidecar's URL.
  Deliberately dumb; anything smarter belongs in the sidecar.

## Build

Test builds: `./desktop/dist.sh [taildrop-target]` builds a stamped
universal .app + zip and optionally Taildrops it to a test machine — the
`mac-app` skill (`.claude/skills/mac-app`) wraps the full build → smoke →
ship → checklist loop. Manual steps below.

Prereqs: Go, Node, and a Rust toolchain (`brew install rustup && rustup
default stable`; Homebrew's shims live at `/opt/homebrew/opt/rustup/bin`).

```sh
cd desktop
npm install                        # @tauri-apps/cli (prebuilt binary)
mkdir -p src-tauri/binaries        # bundle the sidecar (gitignored)
go build -o src-tauri/binaries/bdrive-aarch64-apple-darwin ../cmd/bdrive
npx tauri build --bundles app      # → src-tauri/target/release/bundle/macos/BearDrive.app
```

Dev loop without bundling: run `bdrive desktop` yourself, then
`npx tauri dev` (the shell finds an already-running sidecar on :8990 and a
`bdrive` on PATH).

Unsigned build; signing + notarization are wired into the Tauri pipeline when
we ship (set `bundle.macOS.signingIdentity`, notarize via `tauri build`'s
APPLE_* env vars).
