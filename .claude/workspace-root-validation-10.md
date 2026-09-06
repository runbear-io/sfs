# Workspace-root validation — round 10

**VERDICT: ZERO FINDINGS.**

Round 9's three fixes all hold, and all three of its new tests are load-bearing
(mutation-tested). The signature defect — an unbounded filesystem read placed
where blocking is not affordable — **is not present anywhere in this change
set.** The inventory below is complete, and it rests on one structural fact:
`IsWorkspaceRoot`, the only function that opens a manifest, is now reachable
from exactly one production caller (`RefreshWorkspace`), whose only production
caller is the daemon's own goroutine, where blocking is affordable by
construction. Every other workspace-root call site — `bdrive init`,
`notAProject`, `findProject`, `workspaceRootUnder`, `DesignateWorkspace`,
`CheckRootPlacement` — goes through `HasManifest`, a stat.

Proved rather than argued: a FIFO planted at **every** path the desktop connect
can reach returns in ≤7 ms; `bdrive init` at a FIFO-manifest folder refuses
instantly through the real binary; the 300-trial `rm -rf` race that produced 9
silent resurrections in round 9 now produces **0**; and the agent-hook guard,
hand-run under a real `/bin/sh` across 40 cases, never opens the manifest at all.

Worktree `/Users/snow/workspace/runbear/wt-desktop-app`, branch `desktop-app`,
HEAD `1672a13` + uncommitted work. Mutation sandbox `/tmp/r10repo` (tar copy —
**no `git stash`**). Nothing in the worktree was edited but this file.

**GATE, run by me end to end:** `go build ./...` OK, `go vet ./...` OK,
`go test ./... -count=1 -timeout 5400s` → **12/12 ok, EXIT=0**.
`gofmt -l` over the 19 touched Go files: **clean**. `-race` over the workspace
test set: **clean**.

Three things I demonstrated but am **not** calling findings are under "Notes";
four unproven leads are under "Suspicions".

## Findings

**None.** What I attacked, and how, is below. Round-9 verdicts: **F9-1 FIXED,
F9-2 FIXED, F9-3 FIXED** (table further down).

## Mutation tests of round 9's three new tests — all three guards are load-bearing

Sandbox `/tmp/r10repo` (tar copy of the worktree; **no `git stash`**). Each mutation
reverts exactly the round-9 guard and runs only that test.

| Mutation | Reverted to | Result |
|---|---|---|
| **M1** `DesignateWorkspace`: `HasManifest(root)` → `IsWorkspaceRoot(root)` | round-8 line | `TestDesignateWorkspaceNeverReadsTheManifestPath` **FAIL (10.03s)** — *"DesignateWorkspace opened the manifest path: the desktop connect would hang at connecting forever"* |
| **M2** `readManifest` → `os.ReadFile` (unbounded) | round-8 line | `TestManifestReadIsBounded` **FAIL** — *"read 4194304 bytes, want it capped at 1048576"* |
| **M3** `SaveWorkspace`: stat+`os.Mkdir` → `os.MkdirAll` | round-8 line | `TestSaveWorkspaceDoesNotResurrectADeletedRoot` **FAIL** — *"SaveWorkspace recreated a root the user deleted"* |

Sandbox restored byte-identical afterwards (`cmp` clean).

## (1) Attacking round 9's fixes — results

### 1a. Can `HasManifest` block? No, on every shape I can produce locally.

`/tmp/r10repo/internal/config/zz_r10_hasmanifest_test.go`, 5 s watchdog per call:

| shape at `<root>/.bdrive/workspace.json` | `HasManifest` |
|---|---|
| FIFO | **returned `true`** (the shape that hangs `open`) |
| symlink → FIFO (`os.Stat` follows) | **returned `true`** |
| symlink loop (a→b→a) | returned `false` (ELOOP) |
| 100-link symlink chain | returned `false` (ELOOP at 32) |
| dangling symlink | returned `false` (ENOENT) |
| `.bdrive` is a self-symlink | returned `false` |
| directory at the manifest path | returned `true` |
| symlink → `/dev/zero` | returned `true` |

None blocked. The residual is the one every existence check has: a *path lookup*
through a component on a stalled network mount. `DesignateWorkspace` inherits
two unproven lookups there (`.bdrive`, `workspace.json`) — the pre-round-9 code
did those same two **plus** an unbounded `open`+`read`. So the fix is at the
floor for this function.

Bonus the cap bought: `IsWorkspaceRoot` on a symlink to **`/dev/zero`** now
returns `false` in milliseconds. Unbounded `os.ReadFile` on `/dev/zero` grows
its buffer until the process dies.

### 1b. `HasManifest` coarseness — safe in every direction I could reach

| shape | `HasManifest` | `IsWorkspaceRoot` | `DesignateWorkspace` | `UndesignateWorkspace` clears it? |
|---|---|---|---|---|
| regular non-manifest file | true | false | no-op, no error | yes |
| empty file | true | false | no-op | yes |
| directory | true | false | no-op | yes |
| **non-empty directory** | true | false | no-op | **no — "directory not empty"** |
| dangling symlink | **false** | false | **creates** | yes |
| FIFO | true | (blocks) | no-op | yes |
| valid manifest | true | true | no-op | yes |
| foreign `kind` | true | false | no-op | yes |
| 2 MiB valid JSON | true | false | no-op | yes |

- Every coarse `true` errs toward **refusing to write** — `DesignateWorkspace`
  leaves an unknown file alone in all of them, which is what its comment claims.
- The one *finer* answer, the **dangling symlink**, is the interesting one: it
  reads as absent, so designation proceeds. It does **not** write through the
  link — `writeJSON` is `CreateTemp` + `os.Rename`, and `rename(2)` replaces the
  link itself. Verified with a symlink aimed at a victim file: the victim was
  never created, and with a *live* symlink `HasManifest` is true so nothing is
  written at all and the target keeps its bytes (`SECRET` intact).
  **No arbitrary-file-write.**
- The user-visible cost of the coarse `true` at `cmd/bdrive/init.go:199`,
  `helpers.go:134/149` and `share.go:250` is a **wrong error message** ("… is a
  BearDrive workspace root") for a folder that is not one. It cannot lose data,
  cannot sync anything, and the state is not reachable through any BearDrive
  code path — `.bdrive` is a `ReservedDir` so no sync can plant a file there,
  `writeJSON` is atomic so no crash leaves a partial one, and nothing but
  `SaveWorkspace` ever writes that name. A user has to create it by hand.
  Recovery is `rm <folder>/.bdrive/workspace.json`. **Not a finding.**

### 1c. The 1 MiB cap — exact at the boundary, and unreachable by real data

`readManifest` returns `min(size, 1 MiB)`. Boundary probed with valid JSON whose
total length is `cap-1`, `cap`, `cap+1` (`TestR10_CapBoundary`):

| file size | bytes read | `IsWorkspaceRoot` | `LoadWorkspace` |
|---|---|---|---|
| cap − 1 | 1048575 | **true** | ok |
| **exactly cap** | 1048576 | **true** | ok |
| cap + 1 | 1048576 | false | clean `parse …: unexpected end of JSON input` |

No off-by-one: a manifest of exactly `maxManifestBytes` is fully read and still
loads. One byte over degrades to "not a root" with a named error — never a
panic, never a partial `Workspace` value.

**Can a real root produce one?** `TestR10_ManifestCapAgainstAScannedRoot` built
a root with **3000 immediate child projects, each with a 255-char directory
name**, and ran the real `ScanWorkspace` + `SaveWorkspace` + `RefreshWorkspace`:
manifest = **933,045 bytes**, still under the cap, still `IsWorkspaceRoot=true`,
`LoadWorkspace ok`. It takes ~3,400 immediate child projects at the maximum
legal name length to cross it. Entries are one path component plus a 10-char
mount id, so nothing shorter gets there.

If a hand-made file did cross it: `HasManifest` stays true (so `bdrive init`
still correctly refuses the root), `DesignateWorkspace` still no-ops, and
`RefreshWorkspace` stops re-indexing. Nothing resolves from the manifest —
`LoadWorkspace` still has **zero non-test callers** (re-confirmed by grep) — so
the whole consequence is a stale index. **Not a finding.**

### 1d. `SaveWorkspace`'s stat-then-`Mkdir` — F9-3's silent case is gone; no legitimate case broken

**Race (`TestR10_SaveWorkspaceRaceWithRemoveAll`, the same 300-trial shape round 9 used):**

```
RemoveAll racing RefreshWorkspace, 300 trials:
  gone=286   survived-with-rm-error=14   SILENTLY-RESURRECTED=0
```

Round 9 measured 9/300 silent resurrections; now **0**. `os.Mkdir` cannot create
the parent, so once `<root>` is unlinked every ordering fails: delete between
`Stat` and `Mkdir` → `ENOENT` from `Mkdir`; delete between `Mkdir` and the write
→ `ENOENT` from `CreateTemp`. The residual 14/300 is a *different* thing — the
manifest lands while `RemoveAll` is still walking, so `rm -rf` **fails loudly**
with "directory not empty" and the user is told. A concurrent writer defeating
`rm -rf` visibly is not a defect this code can fix, and it is the opposite of
F9-3, where `rm -rf` reported success and the folder came back.

**Legitimate cases (`TestR10_SaveWorkspaceLegitimateCases`), all pass:**

| case | result |
|---|---|
| root exists, `.bdrive` deleted | created, root valid |
| root exists, `.bdrive` exists | rewrite ok (`EEXIST` swallowed) |
| root is a **symlink** to a directory | ok — manifest lands in the target |
| `.bdrive` is a symlink to a directory elsewhere | ok, `IsWorkspaceRoot` true |
| **read-only parent** of the root | ok (nothing is created above the root) |
| root is a regular file | clean refusal: *"is not a directory: not writing a workspace manifest into it"* |
| `.bdrive` is a regular file | fails at `CreateTemp` with `ENOTDIR` — same outcome `MkdirAll` gave, different message |

`MkdirAll(<root>/.bdrive)` and `Stat(root)`+`Mkdir(<root>/.bdrive)` differ **only**
when `<root>` itself is missing — which is precisely the case the fix exists for.

### 1e. Agent-hook guard, hand-run against a real `/bin/sh`

The branch's change to `mountGuard` is **comment-only**: `diff` of the
non-comment lines against `git show HEAD:internal/agenthooks/agenthooks.go` is
empty. Guard text dumped verbatim from `mountGuard()` and run under `/bin/sh`
(`/tmp/r10guardtest2.sh`, 40 cases):

- Root **not** registered: project=REACH, root=SKIP, sibling=SKIP, deep=SKIP.
- Project registered below the root (shipping state): project=REACH,
  root=REACH — the **pre-existing `syncTargets` fan-out** (`"$PWD/` matches a
  mount at any depth below), not a workspace-root behaviour; sibling and deep
  folders still SKIP.
- Registry naming the root exactly: SKIP (pattern has a trailing `/`).
- `CLAUDE_PROJECT_DIR` at project / root / sibling / `/` / a missing path: all correct.
- **17 hostile root names** (space, `'`, `"`, `$(whoami)`, backticks, `*`,
  `[a-z]`, `;`, `|`, tab, `dot.dot`, `--leading`, non-ASCII, `#`, `&`, `?`,
  double space): identical answers for every one.
- Newline in the root name: the `case "$PWD" in *"\n"*` guard bails before
  `grep`, and the walk-up still finds the project inside.
- **Injection canary**: a folder literally named `$(touch <canary>)` — the
  canary was never created.
- `sh -x` trace at the root, a sibling, a deep folder and the project:
  **`workspace.json` touched 0 times**, `grep` invoked **at most once**,
  `bdrive` spawned **0 times** inside the guard. Matches the invariant and the
  new comment's claim that the guard climbs past a root without knowing what
  one is.

### 1f. The desktop connect path is stat-only — proved by planting a FIFO at every path it can reach

`TestR10_DesignateBoundedAtEveryPathItTouches` runs the real `DesignateWorkspace`
with a wedged node at each path in turn, 10 s watchdog:

| hostile shape | returned in | outcome |
|---|---|---|
| FIFO at `<root>/.bdrive/workspace.json` | **0 ms** | `created=false` (no-op, file left alone) |
| FIFO at `<root>/.bdrive/config.json` | **0 ms** | refused: *"is a project folder: a workspace root is never itself a mount"* |
| `<root>/.bdrive` **is** a FIFO | **0 ms** | error from `CreateTemp` (`not a directory`) |
| `<root>/.bdrive` is a symlink to a FIFO | 1 ms | same |
| `$BDRIVE_HOME` is a FIFO | 1 ms | designated ok |
| 40-deep symlink chain above `$BDRIVE_HOME` | 7 ms | designated ok |
| **1 GiB sparse file at the manifest path** | **0 ms** | `created=false` — pre-round-9 this read 1 GiB |
| FIFO in a **child** of the root | 2 ms | designated ok (scan-free) |

If any call on this path were open/read-class on a caller-supplied path, one of
these would hang. None did; slowest was 7 ms.

---

## (2) Complete blocking-call inventory

**Classes.** *stat* = metadata/namespace only (`stat`, `lstat`, `mkdir`,
`unlink`, `rename`): cannot block on file content, cannot be made large.
*open/read* = opens a descriptor and/or reads bytes: `open()` blocks forever on
a FIFO / device node / stalled mount, and the read is as large as the file.

Enumerated by grepping every `os.*` / `filepath.EvalSymlinks` / `io.ReadAll` in
`internal/config/workspace.go` and following every production caller
transitively (`IsMount`, `LoadProject`, `Home`, `resolveExisting`, `writeJSON`,
`LoadMounts`, `resolvePath`, `mountsUnder`).

| Entry point (blocking budget) | Call | Syscall | Class | Path chosen by | Verdict |
|---|---|---|---|---|---|
| **E1 `POST /api/desktop/init`** — bare `go runDesktopInit`, no watchdog, no ctx; a wedge pins `onboarding.running` and 409s every retry for the sidecar's life. **Budget: must not block, ever.** | `DesignateWorkspace` → `HasManifest` | `os.Stat(<root>/.bdrive/workspace.json)` | stat | caller (root already probed by `validateShared`) | ok |
| | → `checkRootHere` → `IsMount` | `os.Stat(<root>/.bdrive/config.json)` | stat | caller | ok |
| | → `checkRootAllowed` → `Home()` | getenv / `os.UserHomeDir` | none | — | ok |
| | → `resolveExisting(<root>/.bdrive)` | `EvalSymlinks` = lstat per prefix | stat | caller | ok |
| | → `resolveExisting($BDRIVE_HOME)` | lstat per prefix | stat | app's own home | ok |
| | → `SaveWorkspace` | `os.Stat(<root>)` | stat | caller | ok |
| | | `os.Mkdir(<root>/.bdrive)` | write, no content | caller | ok |
| | | `os.CreateTemp` `O_CREAT\|O_EXCL`, random name | open of a name that cannot pre-exist | app | ok |
| | | `Write` / `Close` / `os.Rename` | write | app | ok |
| | `UndesignateWorkspace` (undo, `:444`) | `os.Remove(manifest)` | unlink | caller | ok |
| **E2 `bdrive init <folder>`** — interactive CLI, Ctrl-C works. Budget: instant. | `init.go:199` `HasManifest(folder)` | `os.Stat` | stat | caller | ok |
| | `workspaceRootUnder` → `resolvePath` | `EvalSymlinks` | stat | caller | ok |
| | → `mountsUnder` → `LoadMounts` | `os.ReadFile($BDRIVE_HOME/mounts.json)` | **open/read** | **app's own file**, size ∝ mount count | ok |
| | → `resolvePath(mount)` per registered mount | `EvalSymlinks` | stat | registry | ok |
| | → `helpers.go:134` `HasManifest(cur)` per intermediate dir | `os.Stat` | dirs between a synced mount and the named folder | ok — **F9-2's fix** |
| **E3 `notAProject(folder)`** — CLI error path of `sync`/`log`/`grep`/`stale`/`restore`. (NOT the agent-hook path: `sync --hook` returns at `cmds.go:126`, before this line.) | `helpers.go:149` `HasManifest` | `os.Stat` | stat | caller | ok |
| **E4 `findProject(dir)`** (`bdrive share`, `bdrive url`) — CLI, walks to `/`. | `share.go:250` `HasManifest(cur)` | `os.Stat` | stat | ancestors nobody named | ok |
| | `config.LoadProject(cur)` per ancestor | `os.ReadFile(cur/.bdrive/config.json)` | **open/read, unbounded** | ancestors nobody named | **pre-existing at HEAD, byte-identical in this branch**; CLI budget, Ctrl-C works. See Suspicions. |
| **E5 daemon refresh goroutine** (`daemon.go:405-412`) — own goroutine, never awaited, sync loop independent of it. **Budget: blocking is affordable by construction.** | `RefreshWorkspaceOf` → `IsWorkspaceRoot` ×2 | `os.Open` + `io.ReadAll(io.LimitReader, 1 MiB)` | **open/read, capped** | the root above the mount | ok — **the only production `open()` of a manifest anywhere** |
| | → `ScanWorkspace` | `os.ReadDir(root)` | open/read (dir) | root | ok |
| | | `os.Stat(child)` for non-dir entries | stat | root's children | ok |
| | | `LoadProject(child)` | `os.ReadFile`, **unbounded** | root's children | ok (goroutine) |
| | → `SaveWorkspace` | as E1 | | | ok |
| **E6 agent-hook guard** — pure shell, every tool call of every session. Budget: microseconds. | `[ ! -f "$d/.bdrive/config.json" ]` per level to `/` | `stat(2)`, shell builtin | stat | ancestors | ok |
| | `grep -qF "\"$PWD/" mounts.json` — **at most once** | open/read | app's own file | ok |
| **E7 `InitWorkspace`, `CheckRootPlacement`, `LoadWorkspace`** | — | — | — | — | **zero production callers** (grep over all non-test Go) |

**The inventory is complete and every open/read-class call is on a path that can
afford it.** Three facts do the work:

1. **`IsWorkspaceRoot` — the only function that opens a manifest — is reachable
   from exactly one production caller: `RefreshWorkspace`, and the only
   production caller of *that* is the daemon's goroutine.** Everything else
   (`init`, `notAProject`, `findProject`, `workspaceRootUnder`,
   `DesignateWorkspace`, `CheckRootPlacement`) now goes through `HasManifest`.
2. **Every manifest read is index-only.** `LoadWorkspace` has zero production
   callers, and grepping `\.Projects\b|WorkspaceProject\{|Workspace\{` over all
   non-test Go finds only `ScanWorkspace` (which *builds* the list) and
   `DesignateWorkspace` (which writes one entry) — every other `.Projects` hit
   is the hub's unrelated `webapp.ProjectDB`. **No path, id or permission is
   ever derived from the file.** Its content influences exactly one production
   bit: whether `RefreshWorkspace` rewrites it.
3. The two remaining unbounded `os.ReadFile`s in the inventory
   (`LoadMounts`, `LoadProject`) are both **pre-existing and unchanged by this
   branch**, and neither is on E1, the only path where blocking is terminal.

---

## End-to-end through the real binary

`/tmp/r10cli2.sh`, built from the sandbox (`go build -o /tmp/r10bdrive ./cmd/bdrive`),
isolated `$HOME` + `$BDRIVE_HOME`, a dead-port hub in `settings.json` so `init`
clears the login flow and then fails instantly — every guard runs before any
network call.

| gesture | result |
|---|---|
| `init` **at** a root | refused: *"is a BearDrive workspace root, not a project"* |
| `init` **above** a root that sits **two levels down** (`<X>/a` with the root at `<X>/a/b`) | refused, naming `/private/.../a/b` — F7-5/F8-1's per-level walk still works, now over stats |
| `init` at the deeper root itself | refused |
| **`init` at a folder whose manifest path is a FIFO** | **refused instantly — no hang.** This is F9-1's exact case at the CLI |
| `init` at a folder with a **stray non-JSON file** / a **directory** at the manifest path | refused with the workspace-root message (coarse, recoverable) |
| recovery: `rm .bdrive/workspace.json`, re-run `init` | guard stops firing, proceeds to the hub (dead port, expected) |
| `log`, `stale`, `stop` at the root | workspace-root message, not the dead-end *"run bdrive init"* |
| `url .`, `share f.md` from a **non-BearDrive sibling** under the root | walk-up reaches the root and reports it |
| `log`, `status` **inside** the project | normal output, no workspace-root message |

## GATE (run by me, end to end, in the worktree)

```
go build ./...   -> exit 0
go vet ./...     -> exit 0
go test ./... -count=1 -timeout 5400s
  ok cmd/bdrive 221.100s      ok internal/agenthooks 34.886s   ok internal/autostart 9.482s
  ok internal/config 3.674s   ok internal/daemon 41.846s       ok internal/journal 36.534s
  ok internal/remote 14.351s  ok internal/secrets 1.065s       ok internal/store 8.622s
  ok internal/syncer 97.295s  ok internal/templates 0.813s     ok internal/webapp 2707.183s
EXIT=0    -- 12/12 ok
```

`gofmt -l` over the 19 touched **Go** files (13 modified + 6 untracked): **clean**.
(The repo's pre-existing gofmt failures — `internal/remote/compress_test.go`,
`internal/webapp/undorun_test.go`, `internal/syncer/chunks.go`, others — are not
among them.)

`-race` over the workspace test set
(`internal/{config,daemon,agenthooks,syncer}`, `-run 'Workspace|Root|Designate|Mount|Project|HookGuard|Manifest'`):
all four **ok**, no data race.

## Round-9 findings — confirmed or refuted

| # | Verdict | Evidence |
|---|---|---|
| **F9-1** — `DesignateWorkspace` blocks forever on the desktop connect path (`IsWorkspaceRoot` is an unbounded `os.ReadFile`) | **FIXED, and the fix is load-bearing and complete** | `DesignateWorkspace` now calls `HasManifest` (a stat). Mutation **M1** (revert to `IsWorkspaceRoot`) ⇒ `TestDesignateWorkspaceNeverReadsTheManifestPath` **FAILS in 10.03 s** with round 9's exact symptom. Every **amplification site** round 9 listed was fixed too, not just the one: `init.go:199`, `helpers.go:134`, `helpers.go:149`, `share.go:250` and `CheckRootPlacement` all read `HasManifest` now (grep). Proved at the floor by planting a FIFO at **every** path `DesignateWorkspace` can reach (8 shapes incl. a 1 GiB file and a 40-link chain) — slowest return **7 ms**. The three shipped "one stat" claims are now true. Confirmed end-to-end: `bdrive init` at a FIFO-manifest folder **refuses instantly**. |
| **F9-2** — `workspaceRootUnder` hangs `bdrive init` on a FIFO in an intermediate directory | **FIXED** | `helpers.go:134` is `config.HasManifest(cur)`. Round 9's exact layout (`<folder>/a/b/team` enrolled, FIFO at `<folder>/a/b/.bdrive/workspace.json`) now returns in **0.04 s** and correctly reports `<folder>/a/b` as the root. `notAProject` on the same wedged path also returns. **Doc nit still open, not a finding:** the header at `helpers.go:97` still says *"one stat per registered mount"* — it is now correctly a *stat* (round 9's substantive half), but it is still one **per intermediate directory**, as the loop comment eleven lines below correctly describes. |
| **F9-3** — a refresh racing an `rm -rf` of the root recreated the root directory | **FIXED — the silent case is gone** | `SaveWorkspace` stats the root and uses `os.Mkdir` (which cannot create a parent). Mutation **M3** (revert to `MkdirAll`) ⇒ `TestSaveWorkspaceDoesNotResurrectADeletedRoot` **FAILS**. Same 300-trial race round 9 ran: **0 silent resurrections** (was 9/300). 14/300 the manifest lands while `RemoveAll` is still walking, so `rm -rf` **fails loudly** with "directory not empty" — the opposite of F9-3, where `rm -rf` reported success and the folder came back. No legitimate `SaveWorkspace` case is broken by `Mkdir` (7 cases checked incl. symlinked root, symlinked `.bdrive`, read-only parent, deleted `.bdrive`). |

## Repo invariants (CLAUDE.md §"Invariants — do not break these")

All intact.

- **Journals / blob-before-journal / scan-before-pull / `Less` + `Replay` / dirty-file
  materialize**: `git diff HEAD --name-only` shows **no production file** under
  `internal/{journal,syncer,store}` changed; the only addition there is
  `internal/syncer/workspace_test.go`.
- **Atomic state writes**: `SaveWorkspace` → `writeJSON` → `os.CreateTemp(dir,
  ".bdrive-tmp-*")` + `os.Rename`. Round 9's change touched only the directory
  creation, not the write. Prefix matches the scanner's ignore rule.
- **Agent-hook guard stays pure shell**: the branch's diff is comment-only
  (non-comment `diff` vs `HEAD` is empty), and the `sh -x` trace shows a stat
  per level plus at most one `grep`, never `bdrive`, and never an open of
  `workspace.json`.
- **Daemon liveness is the flock**: `internal/daemon/daemon.go`'s whole diff is
  the +21-line refresh goroutine; no liveness, pidfile or lock code is touched.
- **`Cycle` under the volume flock**: `internal/config/workspace.go` references
  no `store` package and takes no lock, so the refresh goroutine cannot
  contend with a cycle.
- **"never break sync, retry next cycle"**: the goroutine's only failure
  handling is `log.Printf`; it cannot fail a cycle or a daemon start.
- **`.bdrive/workspace.json` never syncs**: `internal/syncer/workspace_test.go`
  asserts `ReservedPath(".bdrive/workspace.json")`, and it passes in the gate.

---

## Notes (demonstrated, but not defects)

**N1 — `createdRoot` is not the iff its comment claims, and it does not matter.**
`desktop_onboard.go:479-482` says *"Synchronous and fast is the only shape where
`createdRoot` is true if and only if a manifest exists."* `DesignateWorkspace`
returns `(true, SaveWorkspace(...))`, so a write failure yields `created=true`
with **no manifest on disk**. Reproduced with an unwritable `.bdrive`
(stand-in for ENOSPC/EIO): `created=true err="… permission denied"`,
`HasManifest=false`. The iff fails only in the harmless direction — `createdRoot`
is read at exactly one place (`if createdRoot { UndesignateWorkspace(...) }`,
grep-confirmed) and `UndesignateWorkspace` on a missing manifest is a no-op
returning `nil` (verified). The dangerous inverse — `createdRoot=true` removing a
manifest this run did **not** create — is impossible, because `created=true`
requires `HasManifest` to have been false at entry.

**N2 — a non-empty *directory* at the manifest path is a state
`UndesignateWorkspace` cannot clear** (`os.Remove` → "directory not empty").
`bdrive init` then refuses the folder with the workspace-root message and the
daemon's refresh no-ops, forever. Nothing in BearDrive can create that state
(`.bdrive` is a `ReservedDir` so no sync reaches it; `writeJSON` is atomic so no
crash leaves one; only `SaveWorkspace` ever writes that name), and `rm -rf
<folder>/.bdrive/workspace.json` clears it. Same for a stray file — both
reproduced through the real binary, including the recovery.

**N3 — the residual `rm -rf` race is visible, not silent.** 14/300 trials the
manifest lands mid-`RemoveAll` and `rm -rf` returns "directory not empty". Any
concurrent writer does this to `rm -rf`; the user is told. `bdrive stop` before
deleting remains the reliable order, as `project-files.md` says.

## Suspicions (unproven)

- **`findProject`'s pre-existing per-ancestor `LoadProject` is the one unbounded
  read left on a caller-unnamed path.** `share.go`'s walk to `/` does
  `os.ReadFile(cur/.bdrive/config.json)` at every level. A FIFO at, say,
  `/Users/.bdrive/config.json` would hang `bdrive share` / `bdrive url`. It is
  **byte-identical at HEAD** — this branch only added a stat beside it — and the
  budget is a CLI where Ctrl-C works, so I am not calling it a finding. But it
  is the same shape as F9-1 one file over, and the same `readManifest` cap would
  close it for free. I did not attempt to reach it from anything with a tighter
  budget than a CLI.
- **`HasManifest` can, in principle, block on a *path lookup*** through a
  component sitting on a stalled network mount — `os.Stat` follows symlinks. I
  could not produce a stalled mount, only FIFOs (which it survives). This is the
  irreducible floor for any existence check, and the pre-round-9 code paid it
  too, plus an `open`.
- **A `>1 MiB` manifest permanently stops `RefreshWorkspace` re-indexing that
  root** while `HasManifest` keeps `bdrive init` refusing it. I measured that it
  takes ~3,400 immediate child projects at the maximum legal name length to get
  there (3,000 produced 933 KB), so I could not construct a realistic route —
  but I did not enumerate every way a manifest could grow.
- **`IsWorkspaceRoot` returns false on any read error** (EACCES, EIO), so a
  transient I/O error makes a root momentarily invisible to the daemon refresh.
  Safe for everything I could reach (the refresh no-ops); I did not enumerate
  every consequence. Carried forward unchanged from round 9.

## What I could not check

- **Windows and Linux.** Everything ran on macOS/APFS. `GOOS=windows go build ./...`
  still does not pass (pre-existing: `syscall.Flock`/`Kill`/`Setsid`), and the
  case-insensitive `underPath` fold is exercised only against APFS.
- **Real TCC prompts and real stalled network mounts.** FIFOs are the project's
  own stand-in and are what rounds 8 and 9 used; a consent dialog and a hung SMB
  mount block `open()` the same way, but I could not produce either. In
  particular I could not test whether a stalled mount makes `os.Stat` — and
  therefore `HasManifest` — block.
- **The Tauri desktop UI.** The connect path was exercised through
  `config.DesignateWorkspace` and by reading `handleDesktopInit` /
  `runDesktopInit`; I did not drive the app.
- **A real hub.** The CLI exercise used `file://` remotes plus a dead port so
  `init` fails immediately after the guards. Every guard runs before any network
  call, so this does not weaken the results.
- **Genuine ENOSPC on `SaveWorkspace`.** Simulated with an unwritable directory
  (fails at `CreateTemp`); a partial `Write` followed by ENOSPC takes a branch I
  could only read.
- **Concurrency between two machines** sharing a root over a network filesystem.

---

## Artifacts

- Sandbox: `/tmp/r10repo` (tar copy of the worktree — **no `git stash`**), restored
  byte-identical after each mutation and cleaned of `zz_r10_*` files before the
  `-race` run.
- Tests written for this round (in the sandbox only, never in the worktree):
  `internal/config/zz_r10_{hasmanifest,save,connectpath,createdroot}_test.go`,
  `cmd/bdrive/zz_r10_helpers_test.go`,
  `internal/agenthooks/zz_r10_dump_test.go`.
- Shell: `/tmp/r10guardtest2.sh` (40 guard cases against the verbatim
  `mountGuard()` text in `/tmp/r10guard.sh`), `/tmp/r10cli2.sh` (real binary).
- Nothing in the worktree was edited but this file.
