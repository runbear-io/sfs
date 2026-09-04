# Workspace root — independent validation, round 2

**Verdict: 6 findings (0 blocker, 3 should-fix, 3 nit).**

Scope: `git diff HEAD` on `desktop-app` @ `1672a13` plus the six untracked
source files, read against `.claude/workspace-root-goal.md` and
`desktop/DESIGN.md` §"Workspace root and projects". Round 1's report was read
for its claims only; every disposition below was re-derived from the code and
from the real binary. Gate re-run from scratch. Nothing in the repo was
modified — mutation experiments ran against a throwaway copy at `/tmp/v2tree`.

Two structural claims still hold and I could not break them:

- **The manifest is never a source of truth.** `config.LoadWorkspace` still has
  **zero** non-test production callers. The only production reads are
  `IsWorkspaceRoot` (returns a bool) and `RefreshWorkspace`/`InitWorkspace`,
  which rebuild from `ScanWorkspace` and never read the old file's contents
  into the new one. No volume dir, journal, mount id, remote or permission is
  derived from a manifest anywhere. `SaveProject`'s only two production callers
  (`init.go:371`, `desktop_onboard.go:468`) can never target a root.
- **The hook walk-up is correct and within budget in the shipped layout.**
  Reproduced by hand with real `/bin/sh`, the guard string lifted verbatim from
  a real `bdrive hooks install --agent claude`, `env -i` and a `PATH`
  containing only logging shims (§4).

---

## Findings

### V1 — should-fix — the manifest scan is unbounded, and this round put it on two blocking paths

`cmd/bdrive/desktop_onboard.go:478-482` (`InitWorkspace` on the user's chosen
root) and `internal/daemon/daemon.go:371` (`RefreshWorkspaceOf` on every daemon
start). Root cause: `config.ScanWorkspace` (`internal/config/workspace.go:119-146`)
does `os.ReadDir(root)` plus an `os.ReadFile` per child, with no bound.

**What is wrong.** `runDesktopInit`'s own comment states the rule this file
lives by (`desktop_onboard.go:412-414`): *"Bounded like inspect: a protected
folder blocks the syscall rather than refusing it, and a connect step that
hangs forever is worse than one that says what to do."* Every other syscall on
a user-chosen path in that file goes through `probe` — including
`handleDesktopInspect`'s `os.ReadDir(root)` at `:240`. The designation added
this round does the same `os.ReadDir(root)`, plus a read per child and a write,
**unprobed**. Separately, `daemon.Run`'s refresh sits between `ResolveMount`
and `holdLock`/`announce` — i.e. **inside the 10s window `daemon.Start` waits
on** (`daemon.go:265`, `startTimeout = 10 * time.Second`).

**Failure scenario A (desktop connect hangs forever, no error, no undo).** Any
child of the root whose `.bdrive/config.json` blocks on `open()` — a FIFO, a
device node, or the realistic case, a TCC-gated or wedged network path where
`os.Stat` succeeds and the read does not — wedges the connect at the
designation step. The phase never leaves `"connecting"`, `undo` never runs, the
hub project is orphaned and `<root>/<name>/.bdrive/config.json` is left behind.
On macOS this is exactly the hazard the two commits before this round
(`a64fffa`, `1672a13`) were hardening against.

**Failure scenario B (a healthy project's daemon never starts).** The same
blocking sibling stops `daemon.Run` before `store.Open`, so
`bdrive resume` reports `failed … daemon did not come up within 10s`,
`bdrive status` says `daemon: stopped`, and every attempt leaves an orphaned
`bdrive daemon run` process wedged forever. The login autostart runs
`bdrive resume` at every login, so they accumulate. The project itself is
perfectly healthy — the wedge is caused by an unrelated folder beside it.

**Failure scenario C (latency, no blocking needed).** A root with many children
burns the daemon-start budget: measured 20 000 sibling folders, warm cache,
Apple M1 → `bdrive resume` wall time **4.05s** vs **~0.2s** without. Cold cache
or a network volume crosses the 10s ceiling, at which point `bdrive resume`
reports failure for a daemon that is in fact coming up.

**How verified.** Scenario A: a Go test driven through the real
`/api/desktop/init` handler in `/tmp/v2tree` with `syscall.Mkfifo` at
`<root>/decoy/.bdrive/config.json` —

```
zz_probe_test.go:55: connect stuck in phase "connecting" after 25s;
                     mount config written=true, .bdriveignore=false
```

Control, same test with only the designation lines removed:
`terminal phase "done" reached … --- PASS (0.65s)`. Causality established.
Scenario B: real binary, isolated `HOME`/`BDRIVE_HOME`, `file://` remote
(`/tmp/v2_fifo.sh`) — `failed … daemon did not come up within 10s`,
`daemon: stopped`, manifest never refreshed, and pid 26559 still wedged eight
minutes later. Scenario C: `/tmp/v2_bigroot.sh`.

One fix covers all three: bound the scan (the `probe` helper already in this
tree), or move `RefreshWorkspaceOf` after `announce` so a slow scan cannot
cost the daemon its lock announcement.

---

### V2 — should-fix — F9 is half-fixed: four commands still send the user to `bdrive init` at a root

`cmd/bdrive/share.go:246` (`findProject`), reached by `bdrive share`
(`share.go:68,162,211`), `bdrive restore` (`restore.go:48`), `bdrive forget`
(`forget.go:40`) and `bdrive url` (`url.go:55`).

**What is wrong.** The disposition table records F9 as *"fixed — one shared
`notAProject` message"*. `notAProject` was wired into six sites, but
`findProject` — the *other* not-a-project message, which round 1 named
explicitly (`share.go:245`) and reproduced for `bdrive share --list` — was
never touched. Its message is the exact dead end F9 described.

A second-order effect: `restore.go:70`'s new `notAProject(root)` call is
**unreachable**. `findProject` has already returned a folder for which
`LoadProject` succeeded, so `!ok` cannot be true there (bar a TOCTOU). The one
site where `restore` actually meets a root emits `findProject`'s old message
instead.

**Failure scenario.** User at their workspace root:

```
$ bdrive share --list
Error: not inside a bdrive project (run `bdrive init` first)
$ bdrive init
Error: <root> is a BearDrive workspace root, not a project
```

Same for `bdrive restore <file>`, `bdrive forget <path>`, `bdrive url <file>`.
The round's own test (`TestCommandsAtARootDoNotAdviseInit`,
`cmd/bdrive/workspace_test.go:178`) covers only `log` and `grep` — exactly the
two shapes the fix reached — so nothing catches the other four.

**How verified.** Real binary against a hand-built root with one registered
child project (`/tmp/v2_root_cmds.sh`, `/tmp/v2_root_cmds2.sh`). Full command
sweep at the root: `log`, `scope`, `stale`, `grep`, `export`, `stop` all emit
the new workspace message; `share --list`, `restore`, `forget`, `url` all emit
`not inside a bdrive project (run bdrive init first)`.

Note `desktop/DESIGN.md`'s own "Not done" list still says *"commands other than
`init` still suggest 'run `bdrive init` there' at a root"* — the design file is
accurate here and the disposition table is the thing that overclaims.

---

### V3 — should-fix — `bdrive init` above a workspace root is not refused, and the root's "never touched" folders then sync to the whole team

`cmd/bdrive/init.go:199-204` (the refusal is only for the root itself) vs
`desktop/DESIGN.md` §Rules *"Roots do not nest, and a root is never inside a
project"* and *"Nothing at the root syncs … It is not in `ReservedDirs` because
it never lives inside a mount."*

**What is wrong.** `init` refuses a folder that IS a root, and refuses a folder
inside a mount (`init.go:274`). It does not refuse a folder that **contains**
a root. After `bdrive init <root's parent>`, the root is inside a project — the
state the design says never happens — and the parent mount's scanner treats
every non-project child of the root as ordinary content. The nested *mount*
(`<root>/team`) is correctly excluded by the syncer's `vNested` handling; the
folders the root exists to hold apart are not. The manifest itself survives
only by accident, because `.bdrive` is a `ReservedDir` — so DESIGN's stated
reason ("it never lives inside a mount") is false even where its conclusion
holds.

**Failure scenario.** User connects `~/Documents/Projects` in the Mac app; it
becomes a root holding `team/` (shared) and `private-stuff/` (deliberately not).
Later, on the CLI, they run `bdrive init ~/Documents` to share Documents with a
teammate. `private-stuff/secret.txt` is pushed to the hub on the next cycle,
with no warning at any point.

**How verified.** Real binary, `file://` remote (`/tmp/v2_above.sh`). `init` in
the parent ran all pre-flight guards and refused nothing; the following
`bdrive sync` produced one journal op:

```
  put  projects/private-stuff/secret.txt
```

i.e. the file the root exists to keep out. (The underlying nested-mount sync
behaviour predates this round; what is new is a documented construct whose
guarantee it silently voids, and a Rule enforced in only one direction —
`InitWorkspace` refuses a root inside a project, nothing refuses a project
above a root.)

---

### V4 — nit — README and the docs say the manifest lists children that *sync*; it lists project folders, syncing or not

`README.md` (*"It lists which immediate children sync, and nothing more"*),
`web/docs/src/content/docs/reference/project-files.md` (*"naming which of its
immediate children BearDrive syncs"*); `internal/config/workspace.go:139`.

`ScanWorkspace` indexes any immediate child for which `LoadProject` succeeds.
That includes a project **paused** with `bdrive stop`, and a project folder
copied from a teammate that this device has **never enrolled** (no registry
row, no volume store). Neither syncs. Since the manifest's whole stated purpose
is to answer "what on this machine is BearDrive" for a UI that has not been
built yet, a Mac app written against these two sentences would show a paused
and an unconnected project as syncing.

**How verified.** Real binary (`/tmp/v2_docs.sh`): a root with `team/` (live),
`paused/` (`bdrive stop`ped) and `never-inited/` (config only, absent from
`mounts.json`). After `bdrive resume` the manifest lists all three.

---

### V5 — nit — the F5 correction missed two places, one of them the test DESIGN.md cites as its proof

`internal/agenthooks/workspace_guard_test.go:23-24` and `desktop/DESIGN.md`
(the phase-list bullet, *"— the agent-hook walk-up cannot afford to read a file
per ancestor to tell a root from a mount"*).

F5's disposition says *"fixed as docs, in all four places. The deviation stands
on coupling, not cost."* The four places round 1 named were fixed. The same
refuted claim survives verbatim in two more, both inside this change set:

- the test file: *"telling the two apart in the walk-up means READING each
  ancestor's config, i.e. **a grep per level**, in the one guard budgeted at
  zero processes"*;
- DESIGN.md's own changelog bullet, ~100 lines below the paragraph that says
  *"That is affordable — `read` is a shell builtin … **The objection is not cost
  but coupling**"*.

So `desktop/DESIGN.md` now contradicts itself, and the file a future reader
opens to understand why the name was chosen (`TestHookGuardSkipsWorkspaceRoot`,
named in DESIGN.md as the thing that pins the decision) teaches the wrong
reason. Cost is not why; `read` is a builtin, which I re-confirmed in §4 below
by running the whole guard with zero external commands available beyond `grep`.

---

### V6 — nit — `sync_run.go`'s justification for the duplicate refresh names a case that does not exist

`cmd/bdrive/sync_run.go:33-39`.

*"it is repeated for the one gesture that has no daemon — init with
`--foreground`, or a daemon that fails to start"*. `--foreground` does not lack
a daemon: `startSync` line 67-68 is `if foreground { return daemon.Run(...) }`,
and `daemon.Run` refreshes. Only the second reason (a daemon that fails to
spawn before reaching `Run`) is real, and it is narrow. The extra call is
harmless — a second full scan and write of the root on every `bdrive init` —
but a reader deleting one of the two will trust the wrong sentence about which
one is redundant.

---

## Round 1's nine dispositions, confirmed or refuted

| # | Disposition claim | Verdict |
|---|---|---|
| F1 | fixed — refresh moved into `daemon.Run`, `bdrive resume` covered | **CONFIRMED.** Real binary: `bdrive resume` indexed a project added under the root behind its back within 0.4s; a rootless project's parent gained nothing; `TestWorkspaceRefreshOnDaemonStart` fails when the line is removed. But the *position* is wrong — see V1. |
| F2 | fixed as docs — a wrong entry self-heals, deleting the file un-roots | **CONFIRMED.** Deleting the manifest left it un-recreated across `sync` + `resume`; corrupting it un-roots too and `bdrive init` then proceeds to mount the folder (it reached the device-code login flow rather than refusing). Docs match. |
| F3 | fixed as docs — DESIGN states the collided layout is unguarded and names the line | **CONFIRMED.** DESIGN.md §"The name collision" says it plainly; `agenthooks.go:99-108` carries the same statement at the line in question. Reproduced by hand: the collided layout spawns `bdrive` in a non-BearDrive sibling, as documented. |
| F4 | fixed — `undo` un-roots a root THIS run created, keeps a pre-existing one minus the dead entry | **CONFIRMED, and the injected failure is real.** The failure is `seed the wiki template: mkdir …/team/sources: not a directory` — after designation, not before. Removing the `createdRoot` assignment makes `TestDesktopInitFailureUnroots` fail. I swept every other post-designation exit: `.bdriveignore` write, seed, and the deepest one (a non-`errDaemonStart` `startSync` failure, driven for real) all un-root correctly; `errDaemonStart` correctly keeps the root, since that branch leaves a working mount. Caveat: the two tests assert only `phase == "error"`, not *which* error, so a future change that fails earlier would make them vacuous silently. |
| F5 | fixed as docs, in all four places | **PARTIALLY REFUTED** — the four named places are fixed; two more in the same change set still carry the refuted claim. See V5. |
| F6 | fixed as docs — nested mounts exist, DESIGN Rules and `ScanWorkspace` say so | **CONFIRMED.** DESIGN.md §Rules and `workspace.go:111-115` both now state it. |
| F7 | acknowledged, valuable half kept | **CONFIRMED as acknowledged.** `TestWorkspaceRootNeverScanned` still asserts mostly feature-independent facts; the "a user's file named `workspace.json` keeps syncing" half is genuine and worth keeping. Recorded in the goal file's matrix preamble. |
| F8 | fixed — stat through the link | **CONFIRMED.** `workspace.go:133-138` stats through a symlink; asserted in `TestWorkspaceRescanCorrectsStaleEntry` (not `TestWorkspaceManifestRoundTrip` as the table says — the assertion is at `workspace_test.go:241-266`). |
| F9 | fixed — one shared `notAProject` message | **REFUTED as stated.** Six sites fixed, `findProject` not; four commands still dead-end. See V2. |

---

## What I checked and found clean

**Gate, run by me.** `go build ./...` OK, `go vet ./...` OK. Per package,
`-count=1`: `internal/config 1.9s`, `internal/agenthooks 14.9s`,
`internal/daemon 37.2s`, `internal/syncer 46.5s`, `internal/store 2.2s`,
`internal/journal 10.9s`, `internal/remote 6.3s`, `internal/autostart 1.9s`,
`internal/secrets 0.5s`, `internal/templates 0.6s`, `cmd/bdrive 107.4s` — all
`ok`.

**`internal/webapp` passes, and the stated environment fact is wrong.**
`go test ./internal/webapp/ -count=1 -timeout 5400s` → **`ok … 1953.396s`**
(32m 33s), uncached, on this tree. Round 1's `-timeout 1800s` was simply two
and a half minutes short of the package's real runtime, so both of its runs
died on the alarm rather than on anything about the code. The goal file's
*"`internal/webapp` cannot pass under any timeout I have tried"* and its
retreat to *"green, with `internal/webapp` from a warm build cache"* should
both be struck: **Done #2 is genuinely, fully green** — the package just needs
a timeout above ~33 minutes.

**`gofmt -l`** over all eighteen files this round touches (twelve modified, six
new): clean, no output.

**The daemon flake could not be reproduced.** Six runs: three sequential
(`-count=3`) under twelve spinning CPU hogs, and three in the reported shape —
`internal/daemon -v` in parallel with `syncer`, `journal`, `agenthooks` and
`store`+`remote`. All green; 57 passing test instances in the loaded run alone,
zero `--- FAIL`. The tightest deadlines in the package are 200ms and 300ms
(`sec_daemon_test.go:82`, `sec_audit4_test.go:152`), which is where a loaded
machine would bite. **This round's `daemon.Run` change is not implicated**: in
every daemon test the mount's parent is a `t.TempDir()`, so `RefreshWorkspaceOf`
is one `os.ReadFile` returning `ENOENT` and writes nothing — I confirmed no
daemon test creates a workspace root outside `workspace_test.go`'s own dirs.

**Every manifest read is index-only.** `grep` over the whole tree for
`LoadWorkspace|ScanWorkspace|IsWorkspaceRoot|RefreshWorkspace|InitWorkspace|SaveWorkspace|WorkspaceFile|WorkspaceKind`:
production hits are `desktop_onboard.go:443,448,478,479,481`, `helpers.go:98`,
`init.go:199`, `sync_run.go:40`, `daemon.go:371`, and the `configKind` guards in
`project.go`. `LoadWorkspace` appears only in tests. `RefreshWorkspace`
never reads the old manifest into the new one — `IsWorkspaceRoot` is a bool
gate, then `ScanWorkspace` builds from disk. Nothing resolves a path, id,
volume or permission from a manifest.

**Every `.bdrive/config.json` toucher, at a root / a project / a collided
root.** Swept with the real binary at a real root (`log`, `scope`, `stale`,
`grep`, `status`, `share`, `restore`, `sync`, `init`, `forget`, `url`, `export`,
`stop`) and in a plain sibling under a root. `status` correctly lists the child
project from the registry. `sync` at a root correctly syncs the children via
`syncTargets`/`mountsUnder` rather than the root. In a plain sibling under a
root, `log`/`sync` correctly give the ordinary "run `bdrive init` there"
advice — which is right there, since `bdrive init` in that folder works. Only
`findProject`'s message is wrong (V2). At a collided root
(manifest in `config.json`), `IsMount` and `LoadProject` both refuse, as
designed; the shell walk-up does not, as documented (F3).

**"not synced on this device yet" and "syncing is paused" survive the
`notAProject` change.** Both sit after the `!ok` branch, so `notAProject` cannot
shadow them. Verified with the real binary: a `bdrive stop`ped project under a
root gives *"syncing is paused for … (run `bdrive init` there to resume)"* from
both `sync` and `restore`; a project with a config but no registry row gives
*"is not synced on this device yet"* from both. No message was lost.

**The agent-hook guard, by hand.** Real `/bin/sh`, guard string lifted verbatim
from a real `bdrive hooks install --agent claude` into an isolated `HOME`,
`env -i` with `PATH` pointing at a shim directory only (logging `bdrive`,
logging `grep`, and logging wrappers for `head`/`tr`/`sed`/`cat`/`awk`/`ls`/`stat`
so any other external would fail loudly):

| session dir | bdrive spawns | greps | other externals |
|---|---|---|---|
| `root/plainfolder` | 0 ✅ | 1 | 0 |
| `root/plainfolder/src/deep` | 0 ✅ | 1 | 0 |
| `root/team` (a real project) | 1 ✅ | 0 | 0 |
| `root/team/docs/deep` | 1 ✅ | 0 | 0 |
| `root` itself | 1 ✅ (correct — a mount lives below) | 1 | 0 |
| collided root, `plainfolder/src` | 1 ❌ (documented, F3) | 0 | 0 |

One-grep budget respected, zero other processes before `command -v bdrive`, and
`internal/agenthooks` (which contains `sec_hooks_test.go`'s ≤1-grep pin and
`sec_audit4`) passes.

**Tests added this round are load-bearing.** Verified by mutating a throwaway
copy of the tree, not by reading: removing `sync_run.go:40` fails
`TestWorkspaceRefreshOnSyncStart`; removing `daemon.go:371` fails
`TestWorkspaceRefreshOnDaemonStart`; removing the `createdRoot` assignment fails
`TestDesktopInitFailureUnroots`; removing the designation entirely fails
`TestDesktopInitFounder`. `TestWorkspaceRefreshOnSyncStart` genuinely exercises
the `startSync` line rather than being covered by `daemon.Run` — `daemon.Start`
refuses to spawn from a test binary, so `Run` is never reached there. F7's row
10 remains the one test whose assertions mostly survive the feature's removal,
already acknowledged in the goal file.

**No existing test was weakened.** `TestDesktopInitFounder` is the only
modified one and it is strictly stronger: `os.Stat(root/.bdrive)`+`IsNotExist`
became four assertions (`!IsMount(root)`, no `config.json` at the root, every
entry in `root/.bdrive` must be exactly the manifest, and the manifest lists the
project just connected), and it fails if the manifest stops being written. The
six `notAProject` call sites broke no other assertion — the whole suite is
green, and I re-ran `cmd/bdrive` on its own.

**Repo invariants (CLAUDE.md).** Journal ownership, blob-before-journal,
scan-before-pull, `journal.Less`/`Replay`, materialize's dirty check and daemon
liveness-is-the-flock are all untouched. The one new state file goes through
`config.writeJSON` (`os.CreateTemp` + rename, temp prefix `.bdrive-tmp-`), so
the atomic-write invariant holds; the manifest lands at mode 0600. The guard
stays pure shell. Two notes that are not violations: the manifest refresh runs
outside the volume flock (it is not volume state, and the rename is atomic), and
`daemon.Run` performs it before `holdLock` — which is what makes V1's scenarios
B and C possible, but does not affect liveness or double-start safety, since
`announce` still happens only under the lock.

**`internal/webapp` is not attributable to this round.** `go list -deps` shows
`internal/config` is the only changed package in its non-test graph, and the
package's single touchpoint is `config.LoadProject` in
`cli_postsync_e2e_test.go:57` — no `config.IsMount`, no workspace call anywhere.

---

## Suspicions (unproven — NOT findings)

1. **`InitWorkspace` swallows its own failure in the connect flow.**
   `createdRoot = config.InitWorkspace(…) == nil` discards the error. A user
   who picks a root that is itself inside another root or inside a project gets
   a successful connect with no manifest and no mention of it; the outer root's
   index will not list the intermediate folder either (it has no `config.json`),
   so the folder is invisible to any future UI. Deliberate per the comment
   ("not fatal"), and nothing consumes the index yet.
2. **The manifest indexes projects this device does not sync** (V4's mechanism)
   — filed as a docs nit, but if the Mac app ever drives actions off the index
   it will offer to open a volume store that does not exist.
3. **`root == $HOME` is still permitted** (round 1's suspicion 5), and `undo`
   now calls `os.Remove($HOME/.bdrive)` on it. It fails on a populated bdrive
   home, so I could show no breakage — but the new code does now attempt an
   `rmdir` of `$BDRIVE_HOME`.
4. **Concurrent designation vs. undo.** `undo`'s `os.Remove(manifest)` is not
   atomic against a concurrent `RefreshWorkspace` from a daemon under the same
   root; the window between `IsWorkspaceRoot` and the rename could re-root a
   folder undo just un-rooted. Requires two simultaneous connects, which the
   `onboarding.running` flag appears to prevent.
5. **`ScanWorkspace` now stats through symlinks (F8's fix), so an entry can name
   a project living outside the root** with the foreign project's id. Inert
   while nothing reads the manifest; worth a thought for the first consumer,
   alongside round 1's suspicion 7 (`LoadWorkspace` validates nothing), which
   is correctly recorded in DESIGN's "Not done".
6. **A corrupt `workspace.json` is never cleaned up.** After the documented
   "corrupt it to un-root" gesture, `bdrive init` mounts the folder and the
   corrupt file stays in the new mount's `.bdrive` forever. Harmless (reserved,
   never synced), and if the user later repairs the JSON the folder is
   simultaneously a mount and a root — round 1's suspicion 6 from the other end.

---

## What I could not check

- **Clean HEAD's `internal/webapp` runtime.** I ran the package to completion
  on *this* tree only (32m 33s, pass). I did not re-run it on an extracted
  clean `1672a13` — unnecessary once it passes here, but it means I have no
  number to compare against.
- **The Mac app / Tauri shell.** Nothing under `desktop/` beyond `DESIGN.md`
  was read or run; no UI names the root yet.
- **A real TCC-gated root.** V1 scenario A's realistic trigger (`~/Documents`
  on an unsigned build) could not be exercised on this machine; I demonstrated
  the identical hang with a FIFO, which proves the call is unbounded but not
  that macOS specifically will block a `readdir` where `stat` succeeded.
- **Windows and Linux.** All shell work was macOS `/bin/sh`. `GOOS=windows
  go build ./...` does not pass in this repo anyway (CLAUDE.md).
- **`bdrive export`/`import` round-tripping a project under a root.** `export`
  at a root now gives the correct workspace message; I did not run a full
  archive round trip.
- **The frontend.** No `internal/webapp/frontend` file is touched by this diff.
