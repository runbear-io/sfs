# Desktop onboarding — shared goal

Every session on this goal reads this file first, and updates the work
matrix in place. It implements the onboarding storyboard decided on
2026-08-21 — canvas: https://claude.ai/code/artifact/d1836ae6-4878-4a11-8dd0-8e60707a008a
(view "Onboarding flow"; the copy and layout there are the source of truth,
and the key facts are restated here so the goal is self-sufficient). This
file follows the same rules as `.claude/desktop-goal.md` — read its
"Architecture rules" and "Hard invariants" sections; they all apply here.

## Mission

A brand-new Mac user goes from a DMG to a syncing shared folder through
interactive UI alone — no CLI, no agent required: install → sign up →
**a shared `team/` folder created INSIDE their existing Claude Code
project** → first sync → the app recedes to the menu bar. The opinionated
core (storyboard frame 5): pick the project root, type the shared-folder
name (default `team`), see the resulting tree live, one commit button. The
same screen serves the second teammate as a join state (create-or-join by
name is existing hub semantics).

## The product shape (decided — do not relitigate)

- The mount is `<root>/<name>` (e.g. `~/work/acme-app/team`). The root
  itself NEVER syncs. Teammates each get the same shared folder inside
  their own differently-named roots.
- Founder path: the folder is created (with starter structure
  `decisions/`, `notes/`, `README.md`) and a hub project is created.
  Joiner path: the org already has a project with that name → the screen
  flips to join mode ("you'll join it · N files sync down"), the folder is
  created empty and the first cycle pulls.
- Frame 5 controls: root picker (native dialog; drop-a-folder onto the
  window is a stretch goal), name input, Claude-integration toggle
  (default on), "Why this layout?" disclosure, CTA
  "Create team/ and start syncing" / "Join team/ and start syncing".
  Escape hatch link: share the whole folder (Advanced) — may ship as a
  doc link in v1, decide with the owner if it becomes UI.
- First-run (frame 2): when signed out AND zero mounts, the window shows
  Welcome ("Welcome to BearDrive" · one-liner · Get started) instead of
  the current signed-out empty state; Get started runs the existing
  sign-in flow, then lands on frame 5. An in-window coach mark points at
  the menu-bar bear once.
- Success (frame 9): "team/ is live · shared inside <root-name>" with
  exactly three actions — Open dashboard, Copy agent prompt (the paste
  prompt reworded for a shared folder inside a project), Copy invite link.
- Tray (frame 10) gains "Connect a folder…" → opens the window at the
  connect route. Mount rows read "<name> — in <root-name>".

## Work matrix

| Row | Status | Check / notes |
|---|---|---|
| Sidecar `GET /api/desktop/inspect?path=` — detection + join lookup | done | Reports: is-Claude-project (`.claude/`/`CLAUDE.md`/`.git` stat), top-level entries for the tree preview (bounded, e.g. first 8 names), whether the folder is already a mount or inside/above one, and whether the signed-in org already has a project named `<name>` (via the hub, with the token). Closed by `TestDesktopInspect` (markers, entries, target, join lookup, name refusals) |
| Sidecar `POST /api/desktop/init` + `GET /api/desktop/init/status` | done | Origin-guarded. Creates `<root>/<name>` (seeds starter files ONLY when creating, not joining), then runs the same code path as `bdrive init --yes` in that folder: create-or-join hub project by name, write `.bdrive/`, seed `.bdriveignore`, register agent hooks iff the toggle was on, enroll, initial cycle, start daemon. Status endpoint polls progress (phase + counts) for frame 8. Closed by `TestDesktopInitFounder` / `TestDesktopInitJoiner`. The founder path seeds the **LLM wiki template** (owner decision 2026-08-21) — `templates.Get("wiki")`, the same registry `bdrive init --template wiki` uses; joining never seeds over a teammate's content |
| Init safety | done | `name` must be a single clean path element (no `/`, `..`, empty; reject reserved names); refuse a root that is, contains, or is inside an existing mount (point at the existing one instead); never write outside `<root>/<name>`; a failed init must not leave a half-enrolled mount (clean up registry + `.bdrive` on error). Closed by `TestDesktopInitRefusals` + the nesting cases in `TestDesktopInitFounder`. Found while building: a daemon that fails to spawn must NOT unwind a successful init (it would delete the freshly seeded folder) — `errDaemonStart` marks that one survivable failure |
| Native folder picker | done | Sidecar endpoint that shows the macOS chooser via `osascript` (`choose folder`) and returns the POSIX path — no shell change, no new Rust. e2e can't drive a native dialog: the connect form must also accept a typed path (which is the test seam). Closed: `POST /api/desktop/choose-folder` (osascript `choose folder`, cancel → `{canceled:true}`); the typed-path branch is what the e2e walkthrough drives, and the 2026-08-21 real walkthrough used it |
| Frontend onboarding views | done | Desktop-gated, URL-owning routes (repo rule: every surface owns a path — e.g. `/setup`, `/setup/connect`, `/setup/done`): Welcome, connect form with LIVE tree preview (re-renders from the inspect payload + typed name; join banner state), progress view polling init/status, success view. Frame copy comes from the storyboard. One codebase — gate on `config.desktop`, no fork. Closed by `desktop-onboarding.spec.ts` (5 specs: routing, live preview, join mode, refusal, full walkthrough) |
| First-run Welcome replaces signed-out empty state | done | Shows only when `config.desktop && !config.me && mounts empty`; Get started → existing sign-in; after sign-in with zero mounts → connect route. Supersedes the current SignedOutBar-era empty state (update `desktop-signedout.spec.ts` — its old assertion text will change). Closed: desktop + signed out + zero projects redirects to `/setup`, which renders Welcome; `desktop-signedout.spec.ts` asserts the redirect and the storyboard copy |
| Tray "Connect a folder…" | done | Menu item (already mocked in the storyboard) → `open_window` at the connect route; mount rows "<name> — in <root-name>". Closed: tray item (signed-in only) opens `/setup/connect`; mount rows read "<name> — in <root> · <state>". `desktop/smoke.sh` asserts the item and passed on the real app |
| Success actions | done | Open dashboard (navigate), Copy agent prompt (clipboard — reuse the Installation page's prompt builder, reworded per storyboard), Copy invite link (org invite mint proxied to the hub like shares; owner-only hub-side — hide the card for non-owners). Closed by the `desktop-onboarding.spec.ts` success spec: Open dashboard, Copy prompt (clipboard asserted), and a real invite mint (`POST /api/orgs/{org}/invites` proxied; the hub owns the owner-only rule, so the button reports what it says). `/api/orgs` proxies with a local empty-list fallback so an unreachable hub cannot hang the window |
| DMG with drag-install background | done | `dist.sh` (or tauri bundler dmg config) produces a DMG whose window shows app → arrow → Applications (frame 1). Background asset to create (dark, house tokens). Closed 2026-08-21: `desktop/assets/dmg-background.png` (generated, house tokens) + `bundle.macOS.dmg` in tauri.conf; `npx tauri build --bundles dmg` produces it and the mounted volume shows app → arrow → Applications over the background (verified visually, matches frame 1). Note: bundle_dmg.sh drives Finder, so the DMG target needs an UNLOCKED session — it fails at the lock screen |
| Sidecar login identifies as "BearDrive Desktop" | done | The device name sent at token exchange (today it reads like a CLI). OSS hub's approval page then shows it; the cloud page is the cloud repo's copy (note it there, out of scope here). Closed: `appLabel` makes the sidecar sign in as "<host> (BearDrive Desktop)", asserted in `TestDesktopLoginBrowserFlow`. The hub records it per device, so History and the device registry name the app |
| Docs | done | README (desktop section), desktop/DESIGN.md status, the user install guide (desktop/INSTALL.md if committed), INSTALL_FOR_AGENTS.md only if the agent prompt wording changes. Closed: README's `bdrive desktop` row, `desktop/DESIGN.md` (Onboarding phase), and this file. INSTALL_FOR_AGENTS.md unchanged — the agent runbook still describes the CLI path, which this feature does not alter |

Owner decisions:
- **Starter content — DECIDED 2026-08-21: the LLM wiki template** (`wiki` in
  `internal/templates`: `sources/`, `wiki/`, `index.md`, `log.md`). Passed to
  the hub at create time AND seeded locally, exactly as `bdrive init
  --template wiki` does. Supersedes the storyboard's `decisions/ notes/
  README.md` sketch (empty dirs do not sync; the registry template is real).
- **Whole-folder escape hatch — shipped as a docs link** under the CTA
  ("Prefer to share the whole folder? Advanced" → docs.beardrive.ai), the
  smallest thing that keeps the screen one decision. Flag if you want it as
  real in-app UI; the CLI (`bdrive init`) already serves that case.

## Testing prompt (how every round verifies)

1. `go test ./cmd/bdrive -run TestDesktop` — all sidecar behavior including
   the new inspect/init tests; full `go test ./...` before finishing a round.
2. Frontend: rebuild committed assets (`npm run build`,
   `check-dist.sh` green), then `npx playwright test -c e2e` — the hub
   project must stay green untouched; the desktop project grows: a wizard
   walkthrough spec against a NEW signed-in-zero-mounts harness
   (`TestE2EDesktopOnboarding`, own port, fake hub for create-or-join —
   drive the typed-path seam, assert the live tree shows the typed name
   inside the root, the join banner on a name collision, progress, and the
   success screen's clipboard content), plus the revised signed-out spec.
3. `./desktop/smoke.sh` on a fresh build (gains the Connect-a-folder tray
   assertion), then the `mac-app` skill loop for what only the real .app
   proves: the DMG layout, the native picker opening, and one REAL
   walkthrough on this Mac with a scratch `BDRIVE_HOME` + scratch folder
   against a disposable local hub (`bdrive serve` seeded like the sandbox,
   NOT beardrive.ai — onboarding tests must not create real hub projects),
   with screenshots compared against storyboard frames 2, 5, 8, 9.
4. Storyboard fidelity: headline copy in the e2e assertions is taken
   verbatim from the storyboard frames, so drift fails a spec, not a vibe
   check.

## Exit criteria — the goal is reached when ALL hold

1. Every matrix row above is `done` or `wontfix-with-reason`, each backed
   by its named check, and both owner-decision rows are decided by the
   owner (never by a session).
2. `go test ./...`, the full Playwright suite (hub + desktop projects),
   `check-dist.sh`, and `desktop/smoke.sh` are all green on the final
   commit, and the committed `static/` matches `frontend/src`.
3. One real end-to-end walkthrough on this Mac — fresh scratch
   `BDRIVE_HOME`, DMG-installed build, disposable local hub — reaches the
   success screen with the daemon running and the shared folder syncing,
   evidenced by screenshots matching frames 2, 5, 8, 9, then cleaned up.
4. The hard invariants in `.claude/desktop-goal.md` still hold (spot-check:
   the desktop still never writes journals locally outside the init code
   path, loopback + origin guards intact, one frontend codebase).
5. Docs row closed, and everything committed on the `desktop-app` branch
   with the matrix updated to its final state.

## Round log

**2026-08-21 — implemented.** Every matrix row is `done`. Two bugs the new
tests found, both fixed at the root and pinned:

1. `daemon.Start` re-exec'd `os.Args[0]`, so a test binary spawned the whole
   suite recursively — a fork bomb that took this Mac to load 47. `Start` now
   refuses to spawn from a `*.test` binary
   (`TestStartRefusesToForkTheTestBinary`).
2. A daemon that fails to spawn must NOT unwind a successful init — that
   would delete the folder just seeded and orphan the hub project.
   `errDaemonStart` marks the one survivable failure; the connect step
   reports it and keeps the mount.

Also found and fixed in flight: proxying `/api/orgs` without a fallback hung
the window on "Loading…" whenever the hub was unreachable (offline-first is
the point of this app) — it degrades to an empty list now.

**Real walkthrough (2026-08-21)**, DMG-installed universal build, scratch
`BDRIVE_HOME`, disposable local hub (the seeded `TestE2EServe` hub on :8993 —
never beardrive.ai): Welcome → device sign-in → connect (typed root, live
preview off the real folder, Claude detection) → one click → `done` in
seconds → success screen. Verified after: the wiki template seeded in
`project/team`, the project ROOT untouched (no `.bdrive`, no `.bdriveignore`),
the daemon running, and the content on the hub. Screenshots match storyboard
frames 1, 2, 5, 9. Cleaned up: daemon stopped, hub killed, scratch removed.
