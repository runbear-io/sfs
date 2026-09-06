# Workspace root — validation round 3

**VERDICT: 8 findings.** Two are release-blocking (V1 and V3 each introduced a
new failure worse than the one they closed), one is a regression in the core
sync loop that nobody has looked at, and one reintroduces F4 through the very
bound V1 added.

Method: I formed the picture from `git diff HEAD` + the untracked files, then
attacked with the **real binary** (`/tmp/v3bdrive`, built from this tree) and
with **mutation tests** in a throwaway copy at `/private/tmp/v3mut` (`tar`
copy — no `git stash`, worktree never edited except this file). Every claim
below is backed by a command I ran, not by reading.

---

## Gate — run by me, not accepted

```
go build ./...                       OK
go vet ./...                         OK
go test ./... -count=1 -timeout 5400s  EXIT=0, all 12 packages
```

| package | s |
|---|---|
| cmd/bdrive | 159.0 |
| internal/agenthooks | 30.4 |
| internal/autostart | 10.9 |
| internal/config | 5.3 |
| internal/daemon | 43.7 |
| internal/journal | 31.1 |
| internal/remote | 10.9 |
| internal/secrets | 4.8 |
| internal/store | 2.9 |
| internal/syncer | 78.8 |
| internal/templates | 0.8 |
| **internal/webapp** | **1434.6** |

The implementer's timeout claim is **confirmed**: `internal/webapp` at 1434.6s
on my machine (their 1307s idle / 1953s loaded brackets it) — always well past
Go's 600s default, so `-timeout` is mandatory. `EXIT=0`.

`gofmt -l` on the 19 touched files (13 modified + 6 untracked `.go`): **empty**.
Nothing new joins the repo's pre-existing gofmt failures.

Extra: `go test ./internal/daemon ./internal/config ./cmd/bdrive -count=2 -race`
→ **all ok, no data race** (cmd/bdrive 619s under race).

---

## Findings

### V3-1 — CRITICAL — `bdrive init` refuses inside its own project under a root, stranding it

*File:* `cmd/bdrive/helpers.go:102-108` (`workspaceRootUnder`), reached from
`cmd/bdrive/init.go:214`.

`mountsUnder(folder)` includes `folder` itself when `folder` is a registered
mount (`root := resolvePath(folder)+"/"`, and `resolvePath(mi.Path)+"/"` then
matches that prefix exactly). `workspaceRootUnder` takes `filepath.Dir(m)` of
every such mount, so for a project directly under a workspace root it hands
back the root — the project's **parent**. The `resolvePath(parent) !=
resolvePath(folder)` guard compares the wrong pair: it excludes *folder is the
root*, not *the mount is folder*.

That is the shipping desktop layout (`<root>/<name>`), so it hits **every**
desktop-created project.

Failure scenario, driven end to end with the real binary:

```
$ bdrive stop  /private/tmp/v3fifo/root/team
no sync daemon running for .../team; syncing paused (run `bdrive init` to resume)
$ bdrive init  /private/tmp/v3fifo/root/team
Error: /private/tmp/v3fifo/root/team contains the BearDrive workspace root at /private/tmp/v3fifo/root
       that root holds folders you chose NOT to sync, ...
```

`stop` tells the user to run `init` to resume; `init` refuses. There is no
other CLI route back — `init` is *the* documented resume gesture (README,
`init --help`, `INSTALL_FOR_AGENTS.md`, and the repair gesture
`config.EnrollMount`'s own comment names). The only escape is deleting
`.bdrive/workspace.json`, i.e. un-rooting the whole workspace.

The message is also self-evidently wrong: a project folder does not *contain*
its parent.

*Verified:* real binary, twice (`/private/tmp/v3a`, `/private/tmp/v3fifo`);
control — deleting `workspace.json` and re-running the identical command
prints `resuming ... (project team)`. Plus a failing Go test on this tree:
`TestV3InitResumesAProjectUnderARoot` (`/private/tmp/v3mut/cmd/bdrive/zz_v3_init_test.go`).

*Why no test caught it:* `cmd/bdrive/workspace_test.go` covers init **at** a
root and **above** a root. Nothing runs `initCmd` at a project **under** one.

---

### V3-2 — CRITICAL — the daemon now starts, reports healthy, and never syncs

*File:* `internal/daemon/daemon.go:409`.

V1 moved `RefreshWorkspaceOf` after `announce`. It is still called
**synchronously on the main goroutine, before the `for` loop**, and
`ScanWorkspace` is still unbounded. So a sibling under the root that blocks a
read no longer fails the start — it silently kills the daemon's entire working
life while every liveness signal says "running".

```
$ bdrive resume
  started  /private/tmp/v3fifo/root/team (pid 49956)
resumed 1, already running 0, skipped 0, failed 0
$ echo new > .../team/after-resume.txt ; sleep 20 ; bdrive status
  daemon:   running (pid 49956)
  local:    1 change(s) not yet scanned (1 new, 0 edited, 0 removed)
```

`kill -QUIT` on the daemon:

```
goroutine 1 ... [syscall]:
config.LoadProject(...)
config.ScanWorkspace(...)
config.RefreshWorkspace(...)
config.RefreshWorkspaceOf(...)
internal/daemon.Run  daemon.go:409
```

Decoy: `mkfifo <root>/decoy/.bdrive/config.json` (round 2's own shape). After
12s with `--scan-interval 1s`: zero blobs in the remote, empty journal, the
project's file never committed.

This is strictly worse than what V1 replaced. Before: a visible start failure
the user can act on. Now: `bdrive resume` says success, `daemon.Running` (the
flock) says true, `bdrive status` says running, and background sync is dead
forever. It violates CLAUDE.md's *"never break sync, retry next cycle"* — a
blocked syscall is neither a degrade to `Offline` nor a retry — and it hollows
out the flock-is-liveness invariant: the flock now proves *announced*, not
*working*.

Blast radius is bounded in one respect I checked: `bdrive sync` on the same
volume still completes while the daemon is wedged (the wedge sits outside the
cycle lock), and `bdrive stop` still kills it. So it is a silent outage, not a
lockup.

*Verified:* real binary + goroutine dump, `/private/tmp/v3fifo`.

---

### V3-3 — HIGH — V1(a) is not fixed: the connect and `bdrive init` still hang forever

*File:* `cmd/bdrive/sync_run.go:40` — `_ = config.RefreshWorkspaceOf(folder)`,
unbounded, at the head of `startSync`.

V1 wrapped the *designation* in `probe`. `startSync`, called ~30 lines later in
the same function (`runDesktopInit`) and by `bdrive init`, calls the same
unbounded scan on the same root with no bound at all.

`bdrive init` in a project under a wedged root (real binary):

```
resuming /private/tmp/v3fifo/root/team (project team)
   <no further output; still running after 75s>
```
`kill -QUIT` →
```
config.LoadProject / config.ScanWorkspace / config.RefreshWorkspaceOf
main.startSync
```

Desktop connect, when the root already carries a manifest (i.e. the second
connect, and every connect thereafter — the steady state):
`TestV3ConnectStillHangsOnWedgedSibling` →
`connect step never finished` after 60s, at phase `syncing`.

So the round-2 symptom survives verbatim; it only moved one phase later.
(On the *first* connect it self-masks: the designation probe times out, so
`IsWorkspaceRoot(root)` is still false when `startSync` runs and
`RefreshWorkspace` short-circuits.)

*Verified:* real binary with full stack; Go test in the copy.

---

### V3-4 — HIGH — `IsMount` stat→ReadFile put an unbounded read in the scan loop; `bdrive sync` now hangs where HEAD completes

*File:* `internal/config/project.go:234-243`. Hit per directory at
`internal/syncer/walk.go:56` and `internal/syncer/ignore.go:87`.

`IsMount` was `os.Stat` — a call that never blocks. It is now `os.ReadFile`,
and the syncer calls it for **every non-pruned directory of every scan**. Any
`<dir>/.bdrive/config.json` whose *open* blocks now hangs the cycle.

```
$ mkfifo /private/tmp/v3scan/proj/sub/.bdrive/config.json
$ bdrive sync /private/tmp/v3scan/proj          # this tree
   <hangs; SIGQUIT →>
config.IsMount(...)
syncer.(*Session).scan.walkFolder.func3   internal/syncer/walk.go:56
$ bdrive sync /private/tmp/v3scan/proj          # binary built from HEAD (1672a13)
  uploading 1 files (3 B) ... remote: pushed        (<1s)
```

A FIFO is the demonstrable trigger; the realistic one is the same class the
repo already treats as real — a `.bdrive/config.json` on a stalled network
mount, or (per `desktop_onboard.go:48-57`'s own note) a TCC-gated path opened
by the sidecar, where "the syscall does not fail: it blocks forever".

The change buys nothing on the real layout: `IsMount`'s own comment says "a
stat would already answer correctly", and the `kind` read exists only for a
hand-written collided config. A cheap primitive on the hottest path was traded
for a blocking one to guard a layout nothing writes.

Cost is *not* the problem — an `open` that misses costs about what a `stat`
does. Blocking is.

*Verified:* real binary vs. a HEAD binary built from `git archive HEAD`, same
directory, same command.

---

### V3-5 — MEDIUM — the probe timeout re-opens F4: a FAILED connect converts the user's folder into a root

*File:* `cmd/bdrive/desktop_onboard.go:488-497` (designation) and `441-454`
(undo).

`probe` returns on its deadline but **does not cancel the goroutine**. When the
designation times out, `createdRoot` stays `false` while `InitWorkspace(root)`
keeps running. `undo` then takes the *"it was already a root"* branch, calls
`RefreshWorkspace(root)`, finds no manifest yet, and no-ops. Seconds later the
leaked goroutine finishes its scan and writes `workspace.json`.

Result: the connect screen says the connect failed, and the user's folder is
permanently a workspace root — which is exactly F4, and nothing un-designates
one (there is no CLI for it, and `bdrive init` refuses a root).

```
--- FAIL: TestV3ProbeTimeoutLeavesALateRoot (4.02s)
    a FAILED connect converted <root> into a workspace root after undo ran
```
(decoy FIFO under the root, a writer opening it at t=4s — i.e. the user
clicking Allow on the macOS prompt a moment after the 1.5s deadline, which is
the exact scenario `fsProbeTimeout` exists for.)

Secondary: each timed-out `probe` leaks a goroutine blocked on the open for the
life of the sidecar; and the `createdRoot` branch of `undo` is the one
filesystem sequence in this file *not* wrapped in `probe`.

*Verified:* Go test in the copy against the unmodified branch code.

---

### V3-6 — MEDIUM — V3's refusal is registry-only, and the "nothing to leak" claim is false

*File:* `cmd/bdrive/helpers.go:94-101` (the doc comment),
`desktop/DESIGN.md` §Workspace root ("The second is found through the mount
registry, so it cannot see a root with no enrolled project inside it; that case
has nothing to leak"), and `.claude/workspace-root-goal.md` V3 row.

`workspaceRootUnder` sees only mounts in `mounts.json`. A root whose project
folders exist on disk but are not in *this device's* registry is invisible, and
`bdrive init` above it is allowed:

```
# <root>/.bdrive/workspace.json present, <root>/team/.bdrive/config.json present,
# mounts.json == {}
$ bdrive init /private/tmp/v3b/a/b --yes --server http://127.0.0.1:9
Error: cannot reach bdrive server ...          # reached the NETWORK; no refusal
```

The claim that such a root "holds no shared folders" is wrong — it holds the
project folders *and* the private folders the root exists to keep apart, which
is precisely V3's leak. Reachable by: a wiped/restored `$BDRIVE_HOME` (a
gesture CLAUDE.md itself documents), or a workspace copied from another machine
before `init` runs.

The enrolled case is genuinely fixed and robust — I confirmed refusal from the
root's parent, from three levels up, and under both `/tmp` and `/private/tmp`
spellings in *both* directions (registry spelled one way, `init` invoked the
other).

*Verified:* real binary.

---

### V3-7 — MEDIUM — `$HOME` is an accepted connect root, so the manifest lands inside `$BDRIVE_HOME`

*File:* `cmd/bdrive/desktop_onboard.go:88-126` (`validateShared`) +
`internal/config/workspace.go:workspaceConfigPath`.

`validateShared` checks only that **`<root>/<name>`** and the beardrive home do
not contain one another. With `root = $HOME`, `target = $HOME/team` — neither
contains `~/.bdrive` — so `$HOME` is accepted, and `InitWorkspace($HOME)`
writes the manifest to `$HOME/.bdrive/workspace.json`, i.e. **into
`$BDRIVE_HOME`**, beside `settings.json`, `device.json`, `mounts.json` and
`volumes/`.

```
--- FAIL: TestV3RootAtHomeCollidesWithBdriveHome
    the workspace manifest was written INSIDE $BDRIVE_HOME (.../001/.bdrive):
    .../001/.bdrive/workspace.json
```

Two namespaces with different lifecycles now share one directory. `undo`'s
`os.Remove(root/.bdrive)` then targets `$BDRIVE_HOME` (harmless only because it
is non-empty by then — incidental, not designed).

It also makes V3-2/V3-3 mainstream rather than exotic: with `$HOME` as the
root, every daemon start runs `LoadProject` over all of `$HOME`'s immediate
children — including `~/Desktop`, `~/Documents`, `~/Downloads`, which this very
file documents as blocking forever for the sidecar.

*Verified:* Go test in the copy against unmodified branch code.

---

### V3-8 — LOW — `sync_run.go`'s comment still mis-scopes the call it justifies (V6 half-fixed)

*File:* `cmd/bdrive/sync_run.go:31-38`.

The comment reads as if the call is scoped to one narrow case — *"this call is
for the one case Run never reaches — a daemon that fails to spawn"*. The call
sits at the **top of `startSync`, unconditionally**, above the `foreground`
branch. On `--foreground` and on the normal path it runs, and `daemon.Run` then
runs it again: two full unbounded scans of the root per `bdrive init`. V6
removed the impossible case from the comment but not the mis-scoping, and the
duplicate scan is real work (and, per V3-3, real blocking).

*Verified:* by reading `startSync` end to end (the refresh precedes
`if foreground { return daemon.Run(...) }`), and by V3-3's stack, which shows
`startSync` blocking in it on the non-foreground path.

---

## Round-1 dispositions — confirmed or refuted

| # | Claim | Verdict |
|---|---|---|
| F1 | refresh now in `daemon.Run`, reached by `resume` | **CONFIRMED.** Mutation: deleting the two-line refresh makes `TestWorkspaceRefreshOnDaemonStart` fail (`timed out waiting for the daemon to re-index`). Placement is after `announce`, before the `for` loop → runs exactly once per daemon start, on every start. **But the fix created V3-2.** |
| F2 | docs say a stale *entry* self-heals, deleting the file un-roots | **CONFIRMED.** README + `project-files.md` + DESIGN Rules all say it; I verified `IsWorkspaceRoot` is false on a corrupt body (`configKind` → `""`) so `init` will then mount the folder, exactly as documented. |
| F3 | `kind` guards are Go-only; DESIGN states the collided layout is unguarded in the hook path and names the line | **CONFIRMED.** DESIGN §"The name collision" says it in as many words; the `agenthooks.go` comment names the walk-up line. |
| F4 | `undo` un-roots what this run created, keeps a pre-existing root minus the dead entry | **PARTIALLY REFUTED.** Fixed on the fast path — mutation M1 (never set `createdRoot`) makes `TestDesktopInitFailureUnroots` fail. Broken on the `probe`-timeout path → **V3-5**. |
| F5 | `read` is a builtin; the deviation's reason is coupling, not cost | **CONFIRMED**, and V5's two extra sites are present: `internal/config/workspace.go`'s header comment and `workspace_guard_test.go`'s doc comment both now say it. |
| F6 | nested mounts exist; DESIGN + `ScanWorkspace` say so | **CONFIRMED**, both places. |
| F7 | row 10's test asserts little the feature owns | **CONFIRMED as acknowledged.** The valuable half is real: `write(t, mount, "workspace.json", ...)` + the `ReservedName/ReservedPath` assertions would catch any name-based rule. |
| F8 | `ScanWorkspace` stats through symlinked children | **CONFIRMED.** Code stats through; `TestWorkspaceRescanCorrectsStaleEntry` asserts `by-symlink` is indexed with the right id (and `t.Skipf`s where symlinks are unavailable). |
| F9 | six commands share `notAProject` | **CONFIRMED.** All six call sites replaced (`cmds.go` ×2, `grep.go`, `restore.go`, `stale.go`, `helpers.go/mustProject`). |

Suspicion 7 (`LoadWorkspace` validates nothing) — **still true**, still with
zero non-test callers, and still recorded in DESIGN "Not done". `ScanWorkspace`
can only ever emit single-segment `Path`s (`e.Name()`), so only a hand-edited
manifest could carry traversal.

## Round-2 dispositions — confirmed or refuted

| # | Claim | Verdict |
|---|---|---|
| V1 | wedged sibling no longer hangs the connect; no longer blocks a daemon start | **REFUTED on all three sub-claims.** (a) connect still hangs forever once the root carries a manifest → V3-3. (b) the daemon start "succeeds" and the daemon then never runs its loop → V3-2, worse than the bug it replaced. (c) the timeout path leaks a root → V3-5. |
| V2 | `findProject` reports the root it walked past | **CONFIRMED.** Real binary: `share` at the root, and `url` / `forget` / `restore` from a non-project folder under it, all print the workspace-root message and never `bdrive init`. A project three levels deep still resolves unchanged (`bdrive url f.txt` → correct hub URL). Cost is one `os.ReadFile` attempt per ancestor, only above the level where no project was found, and it stops at the first root (`wsRoot == "" &&`). Nothing pathological. |
| V3 | `bdrive init` refuses a folder containing a root | **CONFIRMED for the enrolled case** (parent, 3 levels up, both symlink spellings, both directions) — **but it introduced V3-1 (critical false positive)** and its stated blind spot is not harmless → V3-6. |
| V4 | README/docs say the manifest lists project folders incl. paused/unenrolled | **CONFIRMED**, both surfaces, in those words. |
| V5 | the two missed F5 sites corrected | **CONFIRMED.** |
| V6 | `sync_run.go` comment fixed | **PARTIALLY.** The impossible case is gone; the mis-scoping remains → V3-8. |

**Round 2's `TestDesktopInitFailure*` caveat: CONFIRMED CLOSED.** Mutation M2
(force a failure *before* designation) makes **both** tests fail on the new
`strings.Contains(msg, "template")` pin — they no longer pass vacuously on an
earlier failure.

---

## Other checks, all clean

- **Manifest reads are index-only.** Every non-test read is
  `IsWorkspaceRoot` (bool) or a refresh: `desktop_onboard.go:441,450,488-493`,
  `helpers.go:104,116`, `init.go:199`, `share.go:250`, `sync_run.go:40`,
  `daemon.go:409`. `LoadWorkspace` has **zero non-test callers**. No path,
  volume, mount id or permission is ever derived from an entry.
- **`.bdrive/config.json` at a root / project / collided root.** Real root:
  `IsMount` false, `LoadProject` `(false, nil)`. Collided root: `IsMount` false,
  `LoadProject` errors "workspace root, not a project". Project: unchanged.
  `SaveProject` is unreachable at a root (init refuses; the desktop target is
  always `<root>/<name>`). `writeJSON` is temp+rename with the `.bdrive-tmp-`
  prefix, so concurrent refreshes cannot tear the manifest.
- **Agent-hook guard, by hand with a real `/bin/sh`** (guard extracted verbatim
  from `mountGuard()`, fake `bdrive` on `PATH` logging any spawn):
  non-BearDrive sibling under a root → no spawn; three levels deep in that
  sibling → no spawn; real project under the root → spawn; project subfolder →
  spawn; at the root itself → spawn (registry half, intended). `sh -x` grep
  count: **1** in the sibling, **0** in the project. Budget met.
- **Repo invariants.** Journal ownership, blob-before-journal, scan-before-pull,
  `Less`/`Replay`, dirty-file protection, atomic writes, pure-shell guard,
  cycle-under-flock: all untouched and verified where touched.
  **Two are damaged:** *"never break sync, retry next cycle"* (V3-2, V3-4 —
  a blocked syscall is not a degrade), and the spirit of *"liveness is the
  flock"* (V3-2 — the flock now means announced, not working).
- **New daemon test is load-bearing and does not leak or flake.** Mutation
  proves it fails without the refresh; cleanup removes `.bdrive` and waits ≤5s
  while the loop re-reads config every 25ms; the 10s poll window against a
  refresh that runs immediately after `announce` is not tight.
  `TestWorkspaceRefreshNeverCreatesARoot` passes with the daemon call deleted —
  correct for a "never" test — and mutation M3 (removing
  `RefreshWorkspace`'s `IsWorkspaceRoot` guard) proves it is load-bearing for
  what it actually guards.
- **No existing test weakened.** The only modified test file is
  `desktop_onboard_test.go`. `TestDesktopInitFounder`'s replaced assertion is
  strictly stronger than the one it retired (`!IsMount(root)`, no `config.json`,
  *nothing* in `root/.bdrive` but the manifest, manifest content pinned to
  `["team"]`, `.bdriveignore` still absent) — a fair re-expression.
- **Daemon flake: not reproduced.** `internal/daemon` ×6 sequentially while a
  second ×4 run and a `-race` run competed for the machine, plus ×2 under
  `-race`: all green. I could not produce a reproducible daemon flake. Nothing
  in this round's changes touches a timing-dependent daemon path other than the
  one refresh, whose test is deterministic. The round-1 event stays unexplained.

---

## Suspicions (unproven)

1. **TCC makes V3-2/V3-3 routine on real Macs.** `desktop_onboard.go:48-57`
   states as found-in-the-field fact that a TCC-gated path blocks the sidecar's
   syscall forever. `ScanWorkspace` calls `LoadProject` on every immediate child
   of the root. Any root holding `Desktop`/`Documents`/`Downloads` (V3-7 makes
   `$HOME` one such root) would wedge on every daemon start. I could not
   reproduce TCC blocking from this test harness.
2. **Two daemons under one root can transiently drop an entry.** Two projects
   starting daemons concurrently both `ScanWorkspace` then `SaveWorkspace`; the
   loser can write a list computed before the winner's project existed. Writes
   are atomic so nothing tears, and the next start heals it. Benign for an
   index; would not be if anything ever read it as truth.
3. **Registry pruning can silently reopen V3-6.** `init.go:590` and
   `desktop_onboard.go:139` drop registry rows whose path is no longer a mount.
   A root whose projects were pruned that way becomes invisible to
   `workspaceRootUnder` again.
4. **A symlinked child pointing outside the root is indexed** with a `Path`
   relative to the root it does not live in. Harmless while the manifest is an
   index; a landmine for Suspicion 7's first caller.

## What I could not check

- **macOS TCC behaviour** (suspicion 1) — needs a signed sidecar and a gated
  folder with the prompt suppressed. My blocking decoy was a FIFO.
- **The desktop UI.** No Tauri build was run; I exercised the sidecar's
  `/api/desktop/*` HTTP surface only. Whether the connect screen's own timeout
  masks V3-3 for a real user is unknown — the API stays at `phase: syncing`
  indefinitely.
- **Windows.** `GOOS=windows go build ./...` does not pass at HEAD for reasons
  CLAUDE.md documents, so I did not attempt it. `IsMount`'s stat→ReadFile change
  (V3-4) is platform-independent but its blocking behaviour on Windows named
  pipes is untested.
- **`internal/webapp` in detail.** It is green and untouched by this diff; I did
  not audit it beyond the gate.
- **A real multi-device convergence run with a workspace root**
  (`internal/syncer`'s harness covers one root, one mount). Nothing in the diff
  touches journal or replay, so I judged the risk low rather than proving it.
