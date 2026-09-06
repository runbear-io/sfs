# Workspace root — validation round 5

**Verdict: 8 findings.** Two medium (both reproduced end to end through the
real `POST /api/desktop/init` endpoint), four low, two trivial.

Two of the eight are self-inflicted by round 4 (F5-1, F5-5); one is a
round-4 fix that ships untested (F5-3); one is a round-4 fix whose docs half
landed on one surface out of three (F5-4). The rest are inherited or
cosmetic. **Round 4 did not repeat rounds 2 and 3's pattern of shipping a fix
worse than the bug** — every ordering/blocking fix it made is real and
mutation-proven. Its regression is narrower: `DesignateWorkspace`, written to
replace `InitWorkspace` on the connect path, dropped two rule guards that had
nothing to do with the scan it was created to avoid.

## Gate, run here

```
go build ./...   OK
go vet ./...     OK
go test ./... -count=1 -timeout 5400s   EXIT=0, 12/12 packages
    cmd/bdrive 135s · agenthooks 27s · autostart 8s · config 3s · daemon 39s
    journal 26s · remote 12s · secrets 5s · store 5s · syncer 70s
    templates 1s · webapp 1490s
gofmt -l <19 touched files>   clean
```

All 27 tests DESIGN.md §Status and the goal matrix name exist and are in that
green run. The two deleted names (`TestWorkspaceRefreshOnSyncStart`,
`TestInitRefusesAFolderContainingAnUnenrolledRoot`) appear nowhere in the
tree — F7 is properly closed.

Method note: `git stash` was not used. Comparison and mutation work ran
against a `tar`-piped copy of the worktree at `/tmp/wsclone`; the worktree
itself was never modified.

---

## Findings

### F5-1 — medium — `DesignateWorkspace` enforces neither of DESIGN.md §Rules' two root constraints

`internal/config/workspace.go:222-230` (`DesignateWorkspace`), reached from
`cmd/bdrive/desktop_onboard.go:489`.

DESIGN.md §Rules, lines 135-139:

> - A workspace root is never itself a mount.
> - Roots do not nest, and a root is never inside a project.

`InitWorkspace` (workspace.go:257-283) enforces both — `IsMount(folder)`
refusal plus an ancestor walk. `DesignateWorkspace` checks only
`IsWorkspaceRoot` and `checkRootAllowed`. `InitWorkspace` has **zero
production callers**; `DesignateWorkspace` is the only thing that ever writes
a manifest in shipped code. So both rules are enforced only in code nothing
runs.

Neither guard needs a scan: `IsMount` is one `os.Stat`, and the nesting
walk-up is the same `IsWorkspaceRoot` read the function already performs at
`root`. Dropping them was not required by round 4's F3 rewrite.

**Failure scenario A — a project folder becomes a permanent dead end.**
`mountConflict` (desktop_onboard.go:133-152) walks the *registry* and skips
rows where `!config.IsMount(mi.Path)`; a folder carrying
`.bdrive/config.json` that this device never enrolled is not in the registry
at all, so it is never considered. The repo documents that folder as a real
case (`helpers.go:syncBlocked`: "`.bdrive/config.json` travels with the
folder — e.g. arrives in a git clone"). Pick such a clone as the connect
root:

```
--- FAIL: TestProbeCollidedRoot (2.14s)
    registry before connect: map[]
    POST /init -> 200 {"mount":".../003/team","ok":true}
    phase=done
    root IsMount=true IsWorkspaceRoot=true
    COLLIDED: .../003 is now both a mount and a workspace root
    bdrive init at the collided folder -> err=.../003 is a BearDrive
      workspace root, not a project
    DEAD END: `bdrive init` now refuses the user's own project folder
```

`init.go:199`'s `IsWorkspaceRoot` refusal sits **before** the
"already initialized → resume" branch (init.go:239), so the clone can never
be enrolled again from the CLI, and DESIGN.md §Not done confirms "no command
… un-designates one". This is the exact shape of V3-1 — a project stranded
with no CLI route back — arriving through a different door.

**Failure scenario B — nested roots.** Connect a second folder using a
subfolder of an existing root as the root:

```
--- FAIL: TestProbeNestedRootViaConnect (1.33s)
    phase=done
    outer root=true inner root=true
    NESTED: roots do not nest, but .../003/sub is now a root inside .../003
```

`InitWorkspace` refuses the same layout with "roots do not nest".

**How verified.** Two probes in `/tmp/wsclone/cmd/bdrive/zz_probe_test.go`
driving the real `desktopHandler()` over `httptest`, plus a package-level
probe (`/tmp/wsclone/probe/main.go`) showing `DesignateWorkspace` returns
`created=true, err=nil` where `InitWorkspace` returns
`"is a project folder: a workspace root is never itself a mount"` and
`"is already inside the workspace root at …: roots do not nest"`.

**Not a sync leak.** I checked the obvious follow-on: a manifest inside a
mount does not sync. `internal/syncer` reaches `config.ReservedDir` per
directory and `TestWorkspaceRootNeverScanned` already asserts
`ReservedPath(".bdrive/workspace.json")`. DESIGN.md line 140-141's *reason*
("It is not in `ReservedDirs` because it never lives inside a mount") is now
false, but the outcome it claims still holds.

---

### F5-2 — medium — `checkRootAllowed` is case-sensitive, so a case-differing spelling of `$HOME` writes the manifest inside the beardrive home

`internal/config/workspace.go:190-207` (`checkRootAllowed`, `underPath`).

Round 4's F5 replaced equality with containment "on unresolved paths". Both
`underPath` comparisons are `filepath.Rel` on cleaned strings — exact,
case-sensitive — and `samePath`'s symlink resolution only helps when the path
already exists on disk. APFS is case-insensitive by default. The repo knows
this hazard: `config.ReservedDir` folds case with `EqualFold` and carries a
comment naming APFS and NTFS.

**Failure scenario.** Spell the connect root as `/HOME` where the real home is
`/home` (same directory on APFS). The correctly-spelled attempt is refused —
so the guard is the only thing standing there — and the case variant walks
straight past it:

```
--- FAIL: TestProbeHomeCaseBypass (2.87s)
    correct spelling -> 200 … phase=done ; manifest in the home: false
    case variant   -> 200 … phase=done
    BYPASS: a manifest is now inside the beardrive home
      .../home/.bdrive/workspace.json
    beardrive home now holds: device.json
    beardrive home now holds: mounts.json
    beardrive home now holds: settings.json
    beardrive home now holds: volumes
    beardrive home now holds: workspace.json
    IsWorkspaceRoot($HOME) = true
```

That is exactly the V3-7 outcome the guard exists to prevent: an index file in
the directory holding the device token, plus `$HOME` now reading as a
workspace root to `bdrive init` and to every `notAProject` message. A
follow-on: any daemon for a mount directly under `$HOME` will now
`ScanWorkspace($HOME)` on every start — listing the user's whole home and
reading one config per child, in a goroutine that a single wedged child
blocks forever.

**Second instance, same root cause — alias spelling with a not-yet-existing
`.bdrive`.** `samePath` falls back to `false` when `EvalSymlinks` fails, and
`EvalSymlinks` fails on the manifest directory that does not exist yet, which
is the normal case for a fresh root:

```
[deep] BDRIVE_HOME = <base>/state/.bdrive, root spelled through <base-alias>/state
  created=true err=<nil> ; manifest landed in the home: true
```

When `<root>/.bdrive` *does* already exist the same spelling is correctly
refused (probes `[link]`, `[link2]`), which pins the cause precisely.

**Honest grading.** This is **inherited, not introduced by round 4**:
`store.UnderRoot` — the older guard behind `bdrive init`'s home refusal and
`validateShared` — has the same case hole (`/tmp/wsclone/probe4`:
`UnderRoot(/HOME, /home/.bdrive) = false`). Round 4's F5 fix is real for the
cases it covers (removing `checkRootAllowed` fails
`TestWorkspaceRootRefusalCoversTheWholeHome`); it is simply not complete, and
its test exercises only exact-string containment — no case variant, no
symlink spelling, no not-yet-existing `.bdrive`.

**How verified.** `/tmp/wsclone/cmd/bdrive/zz_probe2_test.go` (real HTTP
connect endpoint, production-shaped `BDRIVE_HOME=$HOME/.bdrive`),
`/tmp/wsclone/probe3` (isolated per-case temp roots so an earlier write
cannot short-circuit `DesignateWorkspace`'s `IsWorkspaceRoot` early return),
`/tmp/wsclone/probe4` (`store.UnderRoot` comparison).

---

### F5-3 — low — round 4's F6 fix ships with no test; deleting it leaves the suite green

`internal/config/workspace.go:169-175` — the second `IsWorkspaceRoot(root)`
check after the scan in `RefreshWorkspace`.

Mutation: delete the post-scan re-check, keeping the leading guard.

```
./internal/config   ok   1.665s
./internal/daemon   ok   1.844s   (-run TestWorkspace)
./cmd/bdrive        ok   33.247s  (-run 'Workspace|Root')
```

Nothing fails. The goal doc's own standard is "A row is open until a test
**fails on the current tree and then passes**", and round 3's stated lesson is
"a fix to a blocking/ordering problem needs its own failing test before it is
believed". This is a fix to an ordering problem, believed on a reading.

The fix itself is correct as far as it goes (it narrows the window; it cannot
close it). What is missing is the pin.

---

### F5-4 — low — F6's documentation half landed on one surface out of the three it claims

The round-4 table says the fix was "re-checked after the scan …, and
README/docs/DESIGN say to stop syncing first". Grepping all three:

- `desktop/DESIGN.md:209` — has it: "do it with syncing stopped, since a
  refresh already in flight can rewrite it once".
- `README.md:311-313` — "Deleting the file (or corrupting it) is how you
  un-root the folder; **nothing recreates it**". No caveat.
- `web/docs/src/content/docs/reference/project-files.md` — "Deleting the file
  is different: it is how you **un-root** the folder. **Nothing recreates
  it**". No caveat; no occurrence of "stop syncing" / "syncing stopped" /
  "in flight" anywhere in the file.

The two surfaces a user actually reads still make the claim round 1's F2 and
round 4's F6 both found to be false. CLAUDE.md §"Docs to keep in sync" names
both files.

---

### F5-5 — low — `DesignateWorkspace` is not "one stat"; it does an unbounded `os.ReadFile` on the connect's critical path, and its scan-free test never covers that path

`internal/config/workspace.go:223` → `IsWorkspaceRoot` → `os.ReadFile`
(workspace.go:75). Claimed at `desktop/DESIGN.md:170-175` and
`desktop_onboard.go:471-477`: "one stat, one atomic write, over a directory
the flow has already proven reachable … so it needs no probe and cannot wedge
the connect."

```
[1] FIFO planted at <folder>/.bdrive/workspace.json
  config.IsWorkspaceRoot(folder)                 *** HUNG ***
  config.LoadWorkspace(folder)                   *** HUNG ***
  config.DesignateWorkspace(folder)              *** HUNG ***
  config.RefreshWorkspace(folder)                *** HUNG ***
```

`TestDesignateWorkspaceIsScanFree` plants its FIFO at
`<root>/wedged/.bdrive/config.json` — a *child*, which `DesignateWorkspace`
never reads. It does not plant one at `<root>/.bdrive/workspace.json`, the one
path the function does read. The test is genuinely load-bearing for what it
claims (giving `DesignateWorkspace` a scan makes it fail at 10s — verified),
but it does not test the hazard that is actually present.

This makes round 4's F2 disposition ("moot — designation no longer probes
anything") only half true. The unbounded read F2 was about is still on the
connect's critical path; round 4 removed the (admittedly broken) bound rather
than the read.

**Practical reachability is low**, and I say so plainly: an ordinary
unreadable or absent file returns immediately, and a wedged *directory* is
caught earlier by `validateShared`'s `probe(root, os.Stat)`. So this is a
false claim plus a coverage gap, not a live hang I can produce without
planting a FIFO. It is reported because the claim is load-bearing for the
"exactly one place" rule.

---

### F5-6 — trivial — `UndesignateWorkspace` removes a `.bdrive` this run did not create

`internal/config/workspace.go:235-242`. The comment reads "Only if now empty,
which is the case when this run created it." The premise is false — "now
empty" is also true of an empty `.bdrive` the user already had, and
`os.Remove` succeeds on a symlink regardless of its target:

```
[5] Undesignate:
  a .bdrive holding notes.txt kept it: true       <- documented case, correct
  a pre-existing EMPTY .bdrive survived: false    <- removed
…
  Undesignate on a root whose .bdrive is a symlink: the symlink still there: false
```

Impact is an empty directory or a dangling symlink; no data. The comment
should not claim a guarantee it does not have.

---

### F5-7 — trivial — `TestDesktopInitFailureKeepsAPreExistingRoot` asserts a property no code produces

`cmd/bdrive/desktop_onboard_test.go:459-505`. Its doc comment and the round-2
disposition both say the pre-existing manifest is kept "minus the dead entry"
/ "It only drops the entry for the project that no longer exists". In fact
`DesignateWorkspace` returns `(false, nil)` for a folder that is already a
root and **never adds the new entry at all**, so the manifest is untouched and
the assertion `len(w.Projects) != 1 || w.Projects[0].Path != "notes"` passes
vacuously — nothing ever put `"team"` there to drop. `undo`'s comment at
desktop_onboard.go:447-450 ("stale by one entry until the next daemon start")
describes the same non-event.

The test still has value as a "the manifest survived" guard. Only the stated
reason is wrong.

---

### F5-8 — low — the "exactly one place" rule is true of shipped code but is guarded by nothing; the second door is exported and is what every test uses

DESIGN.md:161-163 and workspace.go:154-159. Exhaustive grep of every caller
(below) confirms the claim **for production code**: `ScanWorkspace` has
exactly two callers, `RefreshWorkspace` and `InitWorkspace`;
`RefreshWorkspace`'s only production caller is
`daemon.go:411`'s goroutine via `RefreshWorkspaceOf`; and `InitWorkspace` has
**zero production callers** — it appears only in tests
(`cmd/bdrive/workspace_test.go` ×6, `internal/config/workspace_test.go` ×8,
`internal/daemon/workspace_test.go` ×2, `internal/syncer/workspace_test.go`,
`cmd/bdrive/desktop_onboard_test.go`).

But `InitWorkspace` is exported, is listed in DESIGN.md line 77-78 as part of
the implemented API, calls `ScanWorkspace` unbounded, additionally does an
unbounded `IsWorkspaceRoot`/`IsMount` read **per ancestor** up to `/`, and
carries none of `RefreshWorkspace`'s "ONLY CALL THIS WHERE BLOCKING IS
HARMLESS" warning. It is also the function every test reaches for, so the
next person adding a caller will copy the hazardous one. The rule is enforced
by nobody having opened the second door.

---

## Round-4 findings F1-F7, confirmed or refuted

| # | Round-4 claim | Verdict |
|---|---|---|
| **F1** | V3-6's child scan removed; guard registry-only; gap pinned by `TestInitDoesNotSeeAnUnenrolledRoot`; `TestInitNeverScansForRoots` fails if a scan returns | **CONFIRMED.** Re-adding a `os.ReadDir`-based child scan to `workspaceRootUnder` fails **both** tests — `TestInitNeverScansForRoots` after a 15s hang on the FIFO, `TestInitDoesNotSeeAnUnenrolledRoot` immediately ("the guard grew a scan"). The renamed/inverted test is an accurate pin of a deliberate limitation, not a rewrite to match broken behaviour: it names the gap, names the hang that closing it caused, and fires the moment the gap is closed the wrong way. Its one weakness is that it never runs `init`, despite the name. I separately checked the gap is exactly as documented and no wider: `bdrive stop` leaves the registry row in place (`syncBlocked` checks `store.Paused` separately), so paused projects still make their root visible; `resolvePath` makes both symlink spellings compare equal; the enrolled-root refusal fires from a parent and from several levels up (`TestInitRefusesAFolderContainingARoot`, and the registry match is a prefix test, not a parent test). Removing the `resolvePath(m) == self` skip fails `TestInitInAProjectUnderARootStillWorks` — `bdrive init` in a project under a root still works and is still guarded. |
| **F2** | "moot — designation no longer probes anything" | **HALF REFUTED → F5-5.** The probe is gone, but so is any bound: `DesignateWorkspace` still performs the unbounded `os.ReadFile` F2 was about, and it demonstrably hangs on a FIFO at `<root>/.bdrive/workspace.json`. |
| **F3** | `DesignateWorkspace` scan-free, synchronous, so `createdRoot` ⟺ "a manifest exists that this run wrote" | **CONFIRMED on the two things it set out to fix; REFUTED on a third.** Scan-free: giving it a scan fails `TestDesignateWorkspaceIsScanFree` at 10s. `createdRoot` accuracy: it is assigned from the synchronous return value, after `IsWorkspaceRoot`, with no goroutine to lose a race with — correct. But the rewrite silently dropped `InitWorkspace`'s mount and nesting guards → **F5-1**, and "one stat" is not what the code does → **F5-5**. |
| **F4** | child scan missed symlinked roots — moot with the scan gone | **CONFIRMED moot.** No child scan exists anywhere; `ScanWorkspace`'s own symlink stat-through (F1 of round 1) is intact and still asserted. |
| **F5** | home refusal is containment on unresolved paths, applied by both entry points | **CONFIRMED for the cases it covers, REFUTED as complete → F5-2.** Removing `checkRootAllowed` from `DesignateWorkspace` fails `TestWorkspaceRootRefusalCoversTheWholeHome`, so the fix is real and pinned. It is case-sensitive, and `samePath` fails open when `<root>/.bdrive` does not exist yet. |
| **F6** | post-scan re-check narrows the deleted-manifest race; README/docs/DESIGN say to stop syncing first | **CODE FIX PRESENT BUT UNTESTED → F5-3; DOCS HALF INCOMPLETE → F5-4.** Only DESIGN.md carries the caveat. |
| **F7** | DESIGN §Status check list rewritten off the deleted test name | **CONFIRMED.** All 27 named tests exist and pass in the green gate; `TestWorkspaceRefreshOnSyncStart` appears nowhere in the tree. |
| **bonus** | `created`/`rootCreated` shadowing renamed; joiner path protected | **CONFIRMED, and the suite really does catch it.** Reverting `rootCreated` to `created` (Go assigns, not shadows, since `werr` is the only new name) fails `TestDesktopInitJoiner`: "joining must not seed the template over a teammate's content". I looked for other collisions in the round's diff: `createdFolder`/`created`/`createdRoot` are distinct; `werr` is reused once inside the `if created` template block but in a separate `if`-scope with the outer value already dead. No other shadowing found. |

## Other things I attacked and found clean

- **Every manifest read is index-only.** Exhaustive grep of `.Projects` and
  `WorkspaceProject{` outside tests: the only production consumers are
  `ScanWorkspace` (appends what it just scanned) and `DesignateWorkspace`
  (writes the caller's own literal). `webapp`'s `s.Projects` is the unrelated
  hub registry. `LoadWorkspace` still has zero non-test callers. Nothing
  resolves a volume, journal, mount id or permission from the file. The
  traversal-shaped entry `{"path":"../../etc","id":"../../x"}` is written
  verbatim and read by nobody — consistent with DESIGN §Not done, which puts
  the obligation on the first caller.
- **The agent-hook guard, by hand, with a real `/bin/sh`.** I ran the verbatim
  `mountGuard` + sync-hook body in a real root layout (root with manifest,
  real enrolled project, non-BearDrive sibling, deep folder inside it, plain
  sibling), with a fake `bdrive` and a counting `grep` on `PATH`:

  | cwd | bdrive spawns | guard greps |
  |---|---|---|
  | `<root>` | 1 (correct — registry finds a mount below) | 1 |
  | `<root>/not-beardrive` | **0** | 1 |
  | `<root>/not-beardrive/src/deep` | **0** | 1 |
  | `<root>/plain` | **0** | 1 |
  | `<root>/team` | 1 | 1 |
  | `<root>/team/docs` | 1 | 1 |

  Exactly one `grep`, of `mounts.json` — the CLAUDE.md budget. No spawn in a
  non-BearDrive folder under a root at any depth. Mutation: teaching the
  walk-up to stop at `workspace.json` fails `TestHookGuardSkipsWorkspaceRoot`
  on all three hooks in both sibling folders.
- **Every toucher of `.bdrive/config.json` at a root / project / collided
  root.** `IsMount` is a stat again (V3-4's revert holds — no `ReadFile` in
  the syncer's per-directory walk). `LoadProject`'s `kind` guard is
  load-bearing: removing it fails `TestLoadProjectRefusesWorkspace` and
  `TestIsMountFalseAtWorkspaceRoot`. `notAProject` and `findProject`'s
  workspace branches are both load-bearing (removing each fails
  `TestCommandsAtARootDoNotAdviseInit` / `TestShareFamilyAtARootDoesNotAdviseInit`).
  `daemon.go:433`'s per-tick `LoadProject` treats the new error like a
  vanished config and exits cleanly — no delete propagation, consistent with
  CLAUDE.md.
- **Repo invariants.** Daemon liveness is still the `daemon.lock` flock;
  nothing in this change set reads a pidfile. The refresh is in a goroutine
  after `announce` — inlining it fails `TestWorkspaceRefreshNeverStallsTheDaemon`
  at 15s, which is exactly the "flock says running, sync never begins" outage
  V3-2 named. Refresh errors are logged, never fatal, so "never break sync,
  retry next cycle" holds. `startSync` has no refresh
  (`TestSyncStartNeverScansTheWorkspaceRoot`). Concurrent daemons writing one
  manifest are safe — `config.writeJSON` uses `os.CreateTemp` with a random
  suffix, so N `bdrive resume` daemons cannot collide on the temp name.
- **`UndesignateWorkspace` cannot write or delete outside the intended root.**
  `workspaceConfigPath` is a plain `Join`; `root` is `filepath.Dir(target)`
  where `validateShared` has already asserted `filepath.Dir(target) == root`
  on cleaned absolute paths. A `<root>/.bdrive` symlinked into the beardrive
  home is refused by `checkRootAllowed` (the link exists, so `samePath`
  resolves) — nothing lands in the home by that route.
- **Design conformance of the new DESIGN.md text**, other than F5-1/F5-5/F5-8:
  the `$HOME` refusal, the "index not truth" framing, the file-name deviation,
  the manifest-is-rebuilt-from-a-scan rule, the migration-is-none claim
  (`TestLegacyProjectUnchanged`) and the stated known gap all match the code.

## Suspicions (unproven)

1. **The gap in F5-1 scenario A may be wider than the clone case.** Any route
   that leaves a `.bdrive/config.json` on disk without a registry row —
   a registry file that failed to load (`mountConflict` returns `""` on
   `LoadMounts` error, i.e. **fails open**), a `$BDRIVE_HOME` swap, a restored
   backup — reaches the same collided state. I proved only the clone.
2. **`workspaceRootUnder` inherits `mountsUnder`'s blocking `resolvePath`.**
   `filepath.EvalSymlinks` lstats every component of every registered mount
   path; one mount on a dead network volume would hang `bdrive init`. This is
   pre-existing (`syncTargets` has used `mountsUnder` all along), not
   round-4's doing, and I did not build a wedged network mount to prove it.
3. **The daemon's refresh goroutine can outlive the daemon** and write a
   manifest after `Run` returned (e.g. after the folder moved). Harmless for
   an index by construction, but it is a write nobody is waiting for and
   nothing cancels.
4. **`DesignateWorkspace` checks `IsWorkspaceRoot` before `checkRootAllowed`**,
   so once a manifest exists — however it got there, including via F5-2 — the
   home guard never runs again for that folder.

## What I could not check

- Anything Windows or Linux: the `syscall.Mkfifo`/flock work is macOS-only
  here, and `GOOS=windows go build ./...` still does not pass at HEAD for the
  pre-existing reasons CLAUDE.md documents.
- Real TCC-gated folders and real wedged network mounts. FIFOs stand in for
  them; the failure mode is the same open(2) that never returns, but I cannot
  prove the macOS privacy prompt behaves identically inside a detached daemon.
- The Tauri shell itself — I drove the sidecar's HTTP API directly, so I
  cannot say what the UI renders when `DesignateWorkspace` logs its
  non-fatal error to stderr.
- Whether F5-2 reproduces on a case-**sensitive** APFS volume. By definition
  it should not; I have no such volume here.
- Multi-daemon manifest refresh under real load (I reasoned about the atomic
  write rather than racing N real daemons).
- `internal/webapp`'s 1490s of tests: I ran them and they pass, but I did not
  audit that package — nothing in this change set touches it.
