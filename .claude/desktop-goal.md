# Desktop parity — shared goal

Every session working on BearDrive Desktop reads this file first. It defines
what "done" means for the Mac app, the architecture rules that keep the work
from scattering, and the parity matrix that tracks progress. Update the
matrix in place as rows close; never delete a row.

## Mission

Bring BearDrive Desktop (the Tauri shell in `desktop/` + the `bdrive desktop`
sidecar in `cmd/bdrive/desktop.go`) to parity with the web application: a
person who knows the hub web app should be able to use the Mac app without
noticing missing capabilities — browser-grade shortcuts, full session
management (sign out, sign in as another account, sign up), and every viewer
feature the hub serves — while sharing one implementation with the web app
wherever a feature exists in both.

## What counts as done

1. Every row of the parity matrix is `done` or `wontfix` (with the reason on
   the row) — each `done` backed by the named automated check on the row.
2. One full pass of the audit harness (below) reports zero unlisted gaps —
   anything it finds either becomes a new row or is closed on the spot.

## The one rule

**A parity gap exists when there is a failing or missing automated check for
it; it is closed when that check passes.** Checks live in:

- `cmd/bdrive/desktop_test.go` (`TestDesktop*`) — sidecar API behavior.
- `frontend/e2e/desktop.spec.ts` (the `desktop` Playwright project, against
  the seeded `TestE2EDesktop` harness on :8994 — runs with `npm run e2e`) —
  anything a user does with keyboard/mouse in the window.
- The `mac-app` skill (`.claude/skills/mac-app`) — build → smoke → Taildrop
  loop for what only the real .app can prove (tray, menus, OS shortcuts,
  spawn/quit). OS-level behavior that Playwright cannot reach is verified by
  scripted screencapture evidence in the session, and listed on the row as
  `manual: <what to check>` for the ship checklist.

Fixing something without its check leaves the row open.

## Architecture rules (how, not just what)

The repo's standing rule — one implementation, one choke point — applied to
this app. SOLID, in this codebase's terms:

- **Single responsibility — three layers, and a feature lands in exactly
  one.** The Rust shell (`desktop/src-tauri`) only spawns, shows OS chrome
  (tray, app menu), and opens windows on the sidecar's URL. The sidecar owns
  all desktop product logic. The React frontend owns all UI. If a change
  touches two layers, one of them should only be growing a *pass-through*.
- **Open for extension via the existing seams, closed against forks.** One
  frontend codebase: desktop behavior extends through `/api/config`
  (`desktop`, and finer capability flags if behaviors diverge) — never a
  copied component tree, never `if (desktop)` sprinkled through components.
  Concentrate desktop divergence in one place per layer (the config hook /
  a capability object on the frontend; the `desktop*` handlers in the
  sidecar), the way `AuthProvider`/`QuotaProvider` concentrate managed-hub
  divergence.
- **Liskov — the sidecar is substitutable for a hub.** It serves the same
  `/api/*` contract; the frontend must work against either without knowing
  which, except through declared config. An endpoint the desktop implements
  behaves like the hub's or returns the same errors the hub would.
- **Interface segregation — capabilities, not platform sniffing.** The
  frontend asks "can I upload / share / see heat" from config, never "am I
  on the Mac app". New divergence = new small flag, not more meaning packed
  into `desktop: true`.
- **Dependency inversion — everything points at the API contract.** The
  frontend depends on `/api/*`; the shell depends on `/api/desktop/*`; only
  the sidecar knows about `$BDRIVE_HOME`, hubs, and tokens. Rust never
  parses settings.json; React never learns storage or token details.

**The write rule:** the desktop NEVER writes local volume stores or journals
(one journal, one writer — the daemon). A write feature comes to the desktop
as a **hub proxy** with the hub enforcing real permissions — the pattern
shares already use. `Server.Desktop` forcing `PermRead` guards the local
volume; proxied features bypass that by design and the hub's own perm
machinery is the gate.

## Hard invariants — do not break

- Loopback-only listener; the `X-Bdrive-Desktop` header guard on every
  side-effecting `/api/desktop/*` route (add the per-launch bearer token
  before widening anything).
- Logout revokes on the hub before clearing locally; a failed revocation is
  reported, never swallowed.
- One login flow at a time; PKCE loopback callback (`browserLogin`) is the
  only credential path — the sidecar never handles passwords.
- The tray template icon stays a transparent monochrome glyph
  (`icons/tray.png`); no tray action blocks the main thread.
- The committed `internal/webapp/static/` must match `frontend/src`
  (`check-dist.sh`) — the app embeds it.

## Parity matrix

Status as of 2026-08-19. Check = the named test/tour step that closes it.

| Row | Status | Check / notes |
|---|---|---|
| Browse, render, history, versions, dashboard, palette | done | `TestDesktopServer` + `desktop.spec.ts` (browse/history/palette/deep-link specs) |
| Read heat (hub proxy) | done | `TestDesktopServer` heat step + `desktop.spec.ts` dashboard spec |
| Share create/list/revoke (hub proxy) | done | `TestDesktop*` share proxy tests + `desktop.spec.ts` share spec |
| Sign out (hub revocation first) | done | `TestDesktopSessionFlow` |
| Sign in / switch account (browser flow, `{"server"}` switches hubs) | done | `TestDesktopLoginBrowserFlow` drives the real PKCE loopback flow end to end (stubbed browser) |
| Sign up | done | Rides the same flow: the opened page is the hub's `/auth/*` (where signup/invite redemption lives), and `/join/<token>` links in the webview open in the default browser via the nav handler. Check: `TestDesktopLoginBrowserFlow`; manual: click an invite link on an unlocked machine |
| Cmd+R reload, Cmd+[ / Cmd+] back/forward | built | App menu (View → Reload CmdOrCtrl+R; History → Back/Forward). Menu structure verified via accessibility on the running .app; manual: press the keys on an unlocked machine (was at the lock screen this round) |
| Cmd+C/V/X/A, Cmd+Z (Edit menu roles) | done | Edit submenu with predefined roles (macOS provides behavior once the menu exists); verified present via accessibility. Manual: paste into palette |
| Cmd+W close window (keep tray), Cmd+Q quit, Cmd+M minimize | done | Window menu (close_window/minimize) + ExitRequested prevention; Reopen handler restores the window |
| Zoom (Cmd+= / Cmd+- / Cmd+0) | built | View menu items → `apply_zoom` (`set_zoom`, level survives reopen). Manual: visual check pending unlock |
| Cmd+K palette, Esc, arrow nav inside the page | done | UI tour palette step |
| Downloads from the webview (Download button saves a file) | built | `on_download` saves to ~/Downloads with collision suffixes. Manual: click Download, check ~/Downloads |
| Copy-link / clipboard buttons | done | `desktop.spec.ts` share spec asserts the minted link lands on the clipboard |
| Uploads (hub proxy) | done | Owner decision 2026-08-20: `upload/init\|content\|commit` proxied (streaming — 3 MiB body pinned in `TestDesktopServer`), with default-hub fallback for projects that have no local mount yet. Note the web app itself has no drag-drop upload UI; uploads serve the create/template flow |
| Restore / undo-remove (hub proxy) | done | `restore`/`remove`/`undo-run` proxied with origin guard (`TestDesktopServer` restore step + `desktop.spec.ts` restore spec); `canRestore` takes the same desktop exception as `canShare` |
| Project create / templates | done | Owner decision 2026-08-20: `POST /api/projects` proxies to the signed-in hub; `upload.enabled: true` in desktop config offers the dialog (`TestDesktopServer` create step + `desktop.spec.ts` dialog spec). Known gap: the new project isn't browsable in the app until `bdrive init` links a folder |
| Permissions view (read-only) | done | `GET /permissions` proxied to the hub (`TestDesktopServer` permissions step); grant edits stay hub-web-only |
| Org/admin surfaces | wontfix | Hub web app's job; the app links out |
| External links open in the default browser (not the webview) | built | `on_navigation`: non-sidecar URLs → `open`. Known gap: `target=_blank` doesn't route through this handler. Manual: click a web link in a markdown file |
| Window state restore (size/position across launches) | built | `tauri-plugin-window-state`. Manual: resize, quit, relaunch |

Rows may be added, never silently dropped. When the audit harness finds a
web-app behavior not listed here, add it as a row first, then decide.

`built` = implemented + compile/structure-verified, manual smoke pending
(2026-08-20 round: the machine sat at the lock screen, so keystroke/visual
checks were unreachable — the listed `manual:` items are the ship checklist).

## How a round runs

1. Read this file and `desktop/DESIGN.md`. Run the audit: `go test
   ./cmd/bdrive -run TestDesktop`, the UI tour, and — for shell rows — a
   build via the `mac-app` skill.
2. Pick the highest open row (order above ≈ priority; shortcuts and signup
   lead because a user hits them in the first minute).
3. Implement **at the layer the architecture rules dictate**, extending an
   existing seam. If you find yourself copying a component or adding a
   second `if desktop` to a file that has one, stop and restructure.
4. Close the row with its check, update the matrix and `desktop/DESIGN.md`,
   and leave the build shippable (`mac-app` skill gates green).

Frontend changes additionally require the rebuilt committed `static/` and a
green `check-dist.sh`; shell changes require a real .app smoke on this
machine before the row closes.
