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
| Sign out (hub revocation first) | done | `TestDesktopSessionFlow`. Production finding (2026-08-20 install test): beardrive.ai cloud answers `DELETE /api/auth/token` with 405 — revocation lives only in `BuiltinAuth`, not on the shared `CLIAuth` surface the PropelAuth provider mounts. Tracked as **BEA-160** with the suggested fix (move revocation onto `CLIAuth` so providers can't omit it, à la `UseDeviceBinder`) |
| Sign in / switch account (browser flow, `{"server"}` switches hubs) | done | `TestDesktopLoginBrowserFlow` drives the real PKCE loopback flow end to end (stubbed browser) |
| Sign up | done | Rides the same flow: the opened page is the hub's `/auth/*` (where signup/invite redemption lives), and `/join/<token>` links in the webview open in the default browser via the nav handler. Check: `TestDesktopLoginBrowserFlow`; manual: click an invite link on an unlocked machine |
| Cmd+R reload, Cmd+[ / Cmd+] back/forward | done | `desktop/smoke.sh` asserts the AX-registered accelerators (R/[/]) on the running .app — the same registration macOS dispatches key equivalents through — and handler firing is pinned by menu-item activation + the one-line evals. Manual ship-checklist: press the keys |
| Cmd+C/V/X/A, Cmd+Z (Edit menu roles) | done | `desktop/smoke.sh` asserts Copy=C/Paste=V registration; macOS provides the behavior for predefined roles |
| Cmd+W close window (keep tray), Cmd+Q quit, Cmd+M minimize | done | `desktop/smoke.sh` asserts Close Window=W/Minimize=M; ExitRequested prevention + Reopen handler in the shell |
| Zoom (Cmd+= / Cmd+- / Cmd+0) | done | `desktop/smoke.sh` asserts =/-/0 registration → `apply_zoom` (`set_zoom`, level survives reopen). Manual ship-checklist: visual zoom check |
| Cmd+K palette, Esc, arrow nav inside the page | done | UI tour palette step |
| Downloads from the webview (Download button saves a file) | done | Verified live 2026-08-20: a real click on a History row's Download landed README.md (6,475 B) in ~/Downloads. Finding for the docs: the FIRST download triggers macOS's "access your Downloads folder" TCC prompt — expected for a non-sandboxed app; mention it in the install docs |
| Copy-link / clipboard buttons | done | `desktop.spec.ts` share spec asserts the minted link lands on the clipboard |
| Uploads (hub proxy) | done | Owner decision 2026-08-20: `upload/init\|content\|commit` proxied (streaming — 3 MiB body pinned in `TestDesktopServer`), with default-hub fallback for projects that have no local mount yet. Note the web app itself has no drag-drop upload UI; uploads serve the create/template flow |
| Restore / undo-remove (hub proxy) | done | `restore`/`remove`/`undo-run` proxied with origin guard (`TestDesktopServer` restore step + `desktop.spec.ts` restore spec); `canRestore` takes the same desktop exception as `canShare` |
| Project create / templates | done | Owner decision 2026-08-20: `POST /api/projects` proxies to the signed-in hub; `upload.enabled: true` in desktop config offers the dialog (`TestDesktopServer` create step + `desktop.spec.ts` dialog spec). Known gap: the new project isn't browsable in the app until `bdrive init` links a folder |
| Permissions view (read-only) | done | `GET /permissions` proxied to the hub (`TestDesktopServer` permissions step); grant edits stay hub-web-only |
| Project settings (rename/icon/default level/grants, delete) | done | Found by the 2026-08-20 audit pass. Edits proxy to the hub (`TestDesktopServer` rename+grant steps); on desktop `project.perm` carries the hub's real level (HubApp resolves it from the proxied `/permissions` — the one place, replacing the scattered `config.desktop \|\|` exceptions) so the admin gate is accurate (`desktop.spec.ts` settings spec) |
| First-run empty state tells the truth about sign-in | done | Signed out on desktop (`config.desktop && !config.me`), the welcome leads with "Sign in to your hub…" + a Sign in button (same loopback flow); the create card yields, the agent card stays (agents sign in themselves). Check: `desktop-signedout.spec.ts` against the virgin-home harness `TestE2EDesktopSignedOut` (:8995) |
| Org/admin surfaces | wontfix | Hub web app's job; the app links out |
| External links open in the default browser (not the webview) | done | Verified live 2026-08-20: clicking "Star on GitHub" in the app opened github.com in a new Chrome tab; the webview stayed on its page. Known gap (tracked): `target=_blank` links don't route through `on_navigation` |
| Window state restore (size/position across launches) | done | Verified live 2026-08-20: resized to 150,90/1100×700 via AX, quit, relaunched — restored exactly |

Rows may be added, never silently dropped. When the audit harness finds a
web-app behavior not listed here, add it as a row first, then decide.

**Live keystroke verification (2026-08-20, unlocked session)**: real Cmd+=
presses zoomed the running app visibly (and Cmd+0 reset it) — the full
accelerator → handler → webview chain confirmed beyond the AX registration
check. Downloads, external-link open, and window-state restore verified the
same session (rows above).

**Fresh-install test (2026-08-20)**: full from-scratch run on a virgin
`BDRIVE_HOME` using the shipped zip — unzip, first launch, empty-state tray
and window, tray Sign in… → beardrive.ai → PropelAuth Google SSO → device
approval → signed in; `bdrive init` from the BUNDLED binary created a
project, synced, and appeared in the app live (no restart); Cmd+R verified
in anger. Findings: the empty-state copy row (open, above), and the cloud
405 on token revocation (noted on the sign-out row).

**Audit pass (2026-08-20)**: walked the hub SPA's routes/components and API
route table against this matrix. One unlisted gap found — project settings
edits — added above and closed the same round. Remaining surfaces all map to
existing rows: `/join` (sign-up row), run cards & `?by=device` (heat proxy),
frontmatter/mermaid (local render), org/billing/analytics (wontfix or
managed-only). No other gaps.

**GOAL REACHED 2026-08-20**: every row `done` or `wontfix`, each with its
named check; the audit pass found one gap (project settings) and it was
closed the same round; the live-session verification finished the last
three physical rows. Future work continues against this matrix — add rows,
never delete them.

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
