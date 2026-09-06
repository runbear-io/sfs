# Workspace root — validation round 7

**Verdict: 4 findings.** One medium, three low. **No new blocking read reached a
critical path this round** — the pattern that produced the last five rounds'
worst bug is, for the first time, not the story. F6-1's split holds under
attack: I drove the real `POST /api/desktop/init` against seven hostile
ancestor shapes (FIFOs at two levels, dead symlinks, symlink loops, a FIFO
*as* `.bdrive`) and every one settled in under 3 s.

The medium is a **refutation of round 6's F6-3**: the `EvalSymlinks` addition
canonicalises the *folder* but not the *home*, so the beardrive-home refusal
still fails open under an aliased spelling — and the addition has **no test
anywhere** (deleting it leaves all 12 packages green). That is the third round
in a row in which this one guard was "closed" from one side only.

The other three are documentation/test-honesty defects in text round 6 wrote or
edited.

## Gate, run here

```
go build ./...                            OK
go vet ./...                              OK
go test ./... -count=1 -timeout 5400s     EXIT=0, 12/12 packages
    cmd/bdrive 201.5s · agenthooks 29.4s · autostart 9.1s · config 4.9s
    daemon 40.3s · journal 42.3s · remote 13.0s · secrets 3.0s · store 3.0s
    syncer 96.0s · templates 0.7s · webapp 2669.8s
gofmt -l <19 touched .go files>           clean (no output)
```

All 32 test names cited by DESIGN.md §Status and the goal matrix exist (one
definition each). The two deleted names (`TestWorkspaceRefreshOnSyncStart`,
`TestInitRefusesAFolderContainingAnUnenrolledRoot`) appear nowhere in the tree.

Method: `git stash` was **not** used. Every mutation and probe ran against a
`tar`-piped copy at `/tmp/ws7`; `git status --short` in the worktree is
byte-identical before and after (only this report was added).

---

## Findings

### F7-1 — medium — the beardrive-home refusal still fails open under an aliased spelling of the HOME; round 6's F6-3 fix is one-sided and untested

`internal/config/workspace.go:236-256` (`checkRootAllowed`), reached from both
`InitWorkspace` and `DesignateWorkspace` via `checkRootHere`.

F6-3 added:

```go
if real, rerr := filepath.EvalSymlinks(folder); rerr == nil {
    if realDir := filepath.Join(real, ProjectDir); realDir != dir {
        if err := homeConflict(realDir, home); err != nil { return err }
    }
}
```

It canonicalises `folder`. It never canonicalises `home`. `Home()`
(`config.go:19`) returns `$BDRIVE_HOME` through `filepath.Abs` (which does not
resolve symlinks) or `os.UserHomeDir()` (which returns `$HOME` verbatim), so
`home` is whatever spelling the environment carries. `homeConflict` then
compares a canonicalised path against an uncanonicalised one; `samePath`
degrades to string equality whenever `<folder>/.bdrive` does not exist yet
(the first-connect case, which is the only case that matters), and `underPath`
is unresolved by design.

**Failure matrix**, driven directly against `checkRootHere` on the unmutated
tree (`/tmp/ws7/internal/config/zz_r7_f63b_test.go`, `real` and `alias` naming
one directory):

| `$BDRIVE_HOME` spelling | folder spelling | refused? |
|---|---|---|
| canonical | canonical | yes |
| canonical | non-canonical (`/var/…`) | yes ← this is what F6-3 fixed |
| canonical | symlink | yes ← this is what F6-3 fixed |
| **non-canonical** | **canonical** | **NO — manifest written into the beardrive home** |
| non-canonical | non-canonical | yes (plain string equality) |
| **non-canonical** | **symlink** | **NO — manifest written into the beardrive home** |

Two reproductions on the real functions (`zz_r7_f63c_test.go`):

```
TestR7NestedAliasedHome            # the exact shape TestWorkspaceRootRefusal-
  home  = /var/…/state/.bdrive/store          # CoversTheWholeHome pins,
  folder= /private/var/…/state                # spelled through /var
  InitWorkspace = <nil>
  MANIFEST WRITTEN AT /private/var/…/state, whose .bdrive contains the home

TestR7AliasedHomeThatDoesNotExistYet
  home=/var/…/h/.bdrive (absent)  folder=/private/var/…/h  InitWorkspace=<nil>
  MANIFEST WRITTEN INTO THE BEARDRIVE HOME /var/…/h/.bdrive
```

The second directly contradicts `checkRootAllowed`'s own doc — "A home that
does not exist yet still counts" — for the aliased spelling.

**No test covers the addition at all.** Deleting the whole `EvalSymlinks` block
leaves `internal/config` green (mutation M4) *and* `cmd/bdrive` green (M4b).
`TestWorkspaceRootRefusalCoversTheWholeHome` exercises containment and, via two
direct `underPath` unit assertions, case-folding — never an alias. This is
round-5 F5-3 repeating: a fix shipped with no regression guard.

**Reachability, honestly.** Default macOS (`$HOME=/Users/x`, `$BDRIVE_HOME`
unset) is canonical and safe; I confirmed the end-to-end connect at a
canonical-home/`$HOME`-as-root is correctly refused
(`zz_r7_e2e_test.go`, real `POST /api/desktop/init`). The trigger is a
non-canonical `home`: a relocated/symlinked `$HOME` (a documented macOS shape),
a home on another volume reached through a link, `/home/x` → `/mnt/home/x` on
Linux, or a `$BDRIVE_HOME` set by a convenient alias — CLAUDE.md's own manual
testing instruction is `BDRIVE_HOME=/some/tmp/dir`, and `/tmp` is a symlink on
macOS. `validateShared`'s `store.UnderRoot` guard is lexical too, so it does
not catch what this one misses (round 5 F5-2 already noted that hole).

**Consequence** when it fires: `.bdrive/workspace.json` lands inside
`$BDRIVE_HOME`, beside the device token — the V3-7 outcome the matrix records
as closed. `IsWorkspaceRoot($HOME)` then becomes true, so `bdrive init` there
reports "workspace root" rather than the home refusal, and a daemon for any
project directly under the home calls `RefreshWorkspaceOf` →
`ScanWorkspace($HOME)`, listing the user's whole home and one config per child.
No credential is *read* or transmitted; this is containment, not disclosure.

---

### F7-2 — low — `CheckRootPlacement`'s safety argument names a caller that does not exist; both ancestor rules are enforced by nothing in shipped code

`internal/config/workspace.go:195-207`:

> "…That is why `DesignateWorkspace` does NOT call this… **`bdrive init`, which
> calls it**, is a foreground command a user can see hanging and interrupt."

`bdrive init` does not call it. `CheckRootPlacement` has exactly one caller,
`InitWorkspace` (workspace.go:360), and `InitWorkspace` has **zero** production
callers — its own doc says so ("It has NO production callers"), and round 6's
own report says so ("`bdrive init` does not call `CheckRootPlacement`"). Every
`InitWorkspace`/`CheckRootPlacement` call site in the tree is in a `_test.go`
file (verified by grep over all `*.go`, and by mutation P1/P2: panicking inside
`CheckRootPlacement` fires only from test fixtures that call
`config.InitWorkspace` to build a root, never from `initCmd()`).

So the two rules that walk ancestors — **"roots do not nest"** and **"a root is
never inside a project"** — are enforced by no shipped code path. The only
production designation is the connect's `DesignateWorkspace`, which applies
`checkRootHere` only.

DESIGN.md carries the same error in its Rules section (the bullet is
specifically about creating a root):

> "Roots do not nest, and a root is never inside a project. … **Enforced by
> `bdrive init`, not by the desktop connect**"

This is low because the trade-off itself is deliberate and F6-4 is right that a
nested root is harmless (see below). It is a finding because it is the
load-bearing sentence of round 6's headline fix, and it tells the next reader a
rule is enforced somewhere it is not.

---

### F7-3 — low — DESIGN.md's new nesting text describes one of the two rules the split gives up, and overstates the harm of the other

`desktop/DESIGN.md` §Rules, second bullet. The split abandons **both** ancestor
rules, but the text names only one consequence ("two connects can produce a
root inside a root") and leaves "a root inside a project" reading as still
enforced. `DesignateWorkspace` refuses only when the root folder *itself* is a
mount, so `DesignateWorkspace(<mount>/sub, …)` succeeds — reachable through the
real connect whenever the enclosing project folder is present but not enrolled
on this device (`mountConflict` consults the registry only).

The same bullet asserts the harm: "a workspace root inside a project **would
sync its manifest to the whole team**". That is false. The manifest lives at
`<sub>/.bdrive/workspace.json`, and `.bdrive` is in `ReservedDirs`, matched at
**any** depth by `ReservedPath` (`zz_r7_reserved_test.go`):

```
ReservedPath("sub/.bdrive/workspace.json")      = true
ReservedPath("a/b/c/.bdrive/workspace.json")    = true
ReservedPath("workspace.json")                  = false   # row 10 still holds
ReservedPath("notes/workspace.json")            = false
```

Nothing syncs. The real cost of a root inside a project is the same cosmetic
one the nesting case has: `bdrive init` there refuses, and `notAProject` /
`findProject` report "workspace root". Worth saying accurately, because a
future reader may re-add an expensive guard to prevent a leak that cannot
happen.

---

### F7-4 — low — two assertions written this round are vacuous: they pass with the guard they name removed

`internal/config/workspace_test.go:606` and `:695`, both worded
"InitWorkspace must still refuse a root inside a root":

```go
if err := InitWorkspace(filepath.Join(outer, "sub2")); err == nil { … }   // 606
if err := InitWorkspace(filepath.Join(root, "inside")); err == nil { … }  // 695
```

`sub2` and `inside` are never created. `InitWorkspace` on a missing directory
fails in `ScanWorkspace` (`os.ReadDir` → ENOENT) whether or not the nesting
guard exists. Mutation: deleting the nesting check from `CheckRootPlacement`
and running **only** `TestDesignateWorkspaceObeysTheRootRules` (M20) or **only**
`TestDesignateWorkspaceNeverReadsAnAncestor` (M20b) → both **PASS**.

These are the compensating assertions the round-6 rewrite added to justify
relaxing the nesting expectation, so their being decorative matters. The rule
itself is properly covered by `TestWorkspaceRefusesNesting`, which uses
directories that exist and matches on the word "nest" (M7 → it fails). So this
is a coverage-honesty defect, not a coverage gap.

---

### F7-5 — low — `workspaceRootUnder` can report a root ABOVE the folder as one beneath it (inherited; round 6 neither introduced nor fixed it)

`cmd/bdrive/helpers.go:108-138`. `mountsUnder` selects mounts by comparing
**resolved** paths but returns the **unresolved** registry path, and the walk
then starts at `filepath.Dir(m)` — the *lexical* parent. A mount registered
through a symlink can therefore have lexical ancestors outside the folder being
examined.

Reproduced (`zz_r7_fp_test.go`): workspace root `R`, project `R/team/wiki`,
symlink `R/shortcut → R/team/wiki` registered as the mount path.

```
workspaceRootUnder(R/team) = "R", true
FALSE POSITIVE: reported the root ABOVE R/team as one beneath it
```

`bdrive init R/team` — a legitimate place for a project under a root — is then
refused with "contains the BearDrive workspace root at R". Same class as V3-1
(a wrongly-refused init on a normal path), far narrower reach.

Explicitly **not** a round-6 regression: the pre-F6-2 code checked
`IsWorkspaceRoot(filepath.Dir(m))`, which is `R` in this shape too, so it
false-positived identically. Reported because it is a demonstrable wrong
refusal in the function under priority review, and no test covers the
symlinked-registry-path direction.

---

## Round-6 findings: confirmed or refuted

**F6-1 — `CheckRootPlacement` on the connect's critical path — CONFIRMED FIXED.**
Enumerated every syscall `checkRootHere` can make, directly and through
`checkRootAllowed` / `homeConflict` / `samePath` / `underPath` / `IsMount` /
`Home`:

| call | syscalls | path |
|---|---|---|
| `IsMount(folder)` | one `stat` | `<folder>/.bdrive/config.json` |
| `Home()` | `getenv`; `getwd` (via `Abs`) or `getenv` (`UserHomeDir`) | none of the caller's |
| `filepath.Join` | none | pure |
| `EvalSymlinks(folder)` | `lstat` per component | prefixes of `folder` |
| `samePath(dir, home)` | `lstat` chains on `<folder>/.bdrive` and on the home | both already named |
| `underPath`, `homeConflict` | none | pure `filepath.Rel` + `strings` |

**No `open`/`read` of file contents anywhere.** That is the property that makes
the claim true: a FIFO blocks on `open`, never on `stat`/`lstat`.
`EvalSymlinks` does walk component by component, but every component it lstats
is a prefix of the path the caller named, and step 1's `os.Stat` already forces
the kernel through that same chain — so it adds syscalls, not blocking surface.
A symlink loop returns `ELOOP` and the branch is skipped.

Attacked the real `POST /api/desktop/init` (`zz_r7_probe_test.go`, real
`desktopHandler()` over `httptest`, root `<base>/anc/Projects`) — all seven
settled, none wedged:

```
fifo-parent-workspace-json         PASS 2.33s   phase=done  isRoot=true
fifo-parent-config-json            PASS 1.34s   phase=done  isRoot=true
fifo-grandparent-both              PASS 1.11s   phase=done  isRoot=true
dead-symlink-root-dot-bdrive       PASS 2.83s   phase=done  isRoot=false  (designation reported to stderr, connect finished)
symlink-loop-at-parent-dot-bdrive  PASS 1.29s   phase=done  isRoot=true
symlink-loop-in-root-path          PASS 2.49s   phase=done  isRoot=true
root-dot-bdrive-is-fifo-itself     PASS 2.26s   phase=done  isRoot=false
```

Round 6's exact repro (FIFO at the parent's `.bdrive/workspace.json`) is the
first row. Mutation M6 (putting `CheckRootPlacement` back into
`DesignateWorkspace`) makes `TestDesignateWorkspaceNeverReadsAnAncestor` hang
its 10 s bound and fail — the guard is load-bearing.
F5-5's one remaining unbounded read (the root's *own* manifest) is unchanged
and still the accepted gap; the caveat on its stated justification is F7-1's
territory, not a new blocking path.

**F6-2 — `workspaceRootUnder`'s per-level walk — CONFIRMED FIXED, terminates,
no worse hang.**
Mutation M1 (revert to parent-only) → `TestInitFindsARootAboveADeepProject`
FAILS. M1b (drop the self-skip, i.e. re-introduce V3-1) →
`TestInitInAProjectUnderARootStillWorks` FAILS. M12 (remove the guard from
`init.go`) → `TestInitRefusesAFolderContainingARoot` FAILS (20 s: reaches the
login flow).
Termination, driven directly (`zz_r7_probe_test.go`): a mount two levels under
its root, `folder` above the root → finds it; `folder` == the root → correctly
returns false (a root is not strictly beneath itself; `init` at a root is
refused by the separate `IsWorkspaceRoot` check); `folder == "/"` → returns
immediately (`mountsUnder("/")` builds the prefix `"//"` and matches nothing —
pre-existing, see Suspicions). The `filepath.Dir(cur) == cur` break makes `/`
terminal in every other shape.
Symlinked spellings: `resolvePath` on both sides makes the ordinary
`/tmp` vs `/private/tmp` and symlinked-`folder` cases exit at the right level.
The one shape it gets wrong is F7-5, which predates this fix.
Hang: the walk does `IsWorkspaceRoot(cur)` — an unbounded `ReadFile` — per
level, where before it did one at the mount's parent. Same hazard class, more
instances, on `bdrive init` only (a foreground, interruptible command; the
same acceptance `CheckRootPlacement` already has). The levels walked lie
between a path this device syncs and a folder the user just named, and
`.bdrive` never syncs, so a teammate cannot plant the FIFO. Not a regression:
the pre-F6-2 read at the mount's parent hangs on the same wedged NAS.
`TestInitNeverScansForRoots` still passes but is weaker than it reads — with no
registered mount under the folder, the loop never executes at all.

**F6-3 — the symlink-alias hole — REFUTED. See F7-1.** The fix closes the
folder-side alias (matrix rows 2 and 3 above) and introduces **no** false
refusal for an ordinary folder reached through a symlinked parent — I checked
that explicitly (`TestR7SymlinkedParentNoFalseRefusal`: `link → realparent`,
`InitWorkspace(link/Projects)` succeeds and writes the manifest in both
spellings). But the home-side alias is untouched, so the hole moved rather than
closed, and nothing tests either half.

**F6-4 — the nesting trade-off — CONFIRMED HARMLESS.** Built the exact shape two
connects produce (`DesignateWorkspace` at both levels,
`zz_r7_nest_test.go`) and checked every decision the goal doc names:

- `ResolveMount` returns the same `ID`/`Volume`/`Remote`; `VolumeDir` is keyed
  by mount id, so no volume or journal path differs.
- Neither root is a mount → nothing syncs at either, no permission changes.
- `RefreshWorkspace(outer)` indexes nothing (the inner root is not a project)
  and leaves the inner manifest byte-identical; `RefreshWorkspace(inner)` still
  indexes its project. Two indexes, no shared state.
- `bdrive init` inside the project under the nested root still works
  (`workspaceRootUnder` finds nothing); `bdrive init` at either root is refused
  with "is a BearDrive workspace root, not a project".
- Concurrent refreshes from two daemons write the same file through
  `writeJSON` → `os.CreateTemp(".bdrive-tmp-*")` + rename: unique temp names,
  atomic, last-writer-wins on an index nothing resolves from.

The *test* change is an **honest pin**, not a bent test: the rewritten
assertion (designation inside a root succeeds) fails under M6, so it pins the
real behaviour, and the compensating "InitWorkspace still refuses" lines were
added rather than the rules being dropped. Two of those lines are decorative —
F7-4.

**F6-5 — the non-blocking handshake — CONFIRMED FIXED.**
- Cannot hang: the retry loop opens `O_WRONLY|O_NONBLOCK`, which returns ENXIO
  until a reader exists and never blocks; a 10 s deadline ends it with
  `t.Fatalf`. The blocking `open` round 6 was bitten by is gone.
- Cannot leak: `done` is buffered (cap 1), so the `RefreshWorkspace` goroutine
  always completes its send whether or not the select is still waiting. (If the
  handshake itself times out the scan goroutine may park — but the test has
  already failed at that point, and the package still exits.)
- Still load-bearing: mutation M5 (delete the post-scan `IsWorkspaceRoot`
  re-check) → `TestRefreshDoesNotResurrectADeletedManifest` FAILS in 0.06 s.
- Not flaky: 20 runs under `-race` in 3.7 s total (≈0.18 s each — the ENXIO
  loop never spins near its bound).
- Bonus: that same re-check is what stops `RefreshWorkspace` creating a root.
  Removing only the *entry* guard leaves `TestWorkspaceRefreshNeverCreatesARoot`
  green (M14b); removing both makes it fail (M14c).

---

## Mutation results (every test added or changed this round, plus the round's guards)

`FAIL` = the mutation was caught, which is what we want.

| # | mutation | result |
|---|---|---|
| M1 | `workspaceRootUnder` → parent-only (undo F6-2) | FAIL `TestInitFindsARootAboveADeepProject` |
| M1b | drop the self-skip (undo V3-1) | FAIL `TestInitInAProjectUnderARootStillWorks`, `…FindsARootAboveADeepProject` |
| M2 | drop "a root is never a mount" from `checkRootHere` | FAIL `TestWorkspaceRefusesNesting`, `TestDesignateWorkspaceObeysTheRootRules` |
| M3 | drop `checkRootAllowed` from `checkRootHere` | FAIL `TestWorkspaceRootIsNeverTheBdriveHome`, `…RefusalCoversTheWholeHome` |
| **M4** | **drop the `EvalSymlinks` alias check (F6-3)** | **PASS — survived `internal/config`** |
| **M4b** | **same, against `cmd/bdrive`** | **PASS — survived; no package catches it** |
| M5 | drop `RefreshWorkspace`'s post-scan re-check | FAIL `TestRefreshDoesNotResurrectADeletedManifest` |
| M6 | `DesignateWorkspace` uses `CheckRootPlacement` (undo F6-1) | FAIL `…NeverReadsAnAncestor` (10 s hang), `…ObeysTheRootRules` |
| M7 | drop "roots do not nest" | FAIL `TestWorkspaceRefusesNesting` |
| M8 | drop "a root is never inside a project" | FAIL `TestWorkspaceRefusesNesting`, `…ObeysTheRootRules` |
| M9 | `notAProject` loses the root branch | FAIL `TestCommandsAtARootDoNotAdviseInit`, `TestShareFamilyAtARootDoesNotAdviseInit` |
| M10 | `findProject` loses the `wsRoot` report | FAIL `TestShareFamilyAtARootDoesNotAdviseInit` |
| M11 | `init` loses the at-a-root refusal | FAIL `TestInitRefusesWorkspaceRoot` (20 s: reached the login flow) |
| M12 | `init` loses the containing-root refusal | FAIL `TestInitRefusesAFolderContainingARoot` |
| M13 | daemon stops refreshing | FAIL `TestWorkspaceRefreshOnDaemonStart` |
| M14 | daemon refreshes inline (undo V3-2) | FAIL `TestWorkspaceRefreshNeverStallsTheDaemon` |
| M14b | `RefreshWorkspace` loses only the entry guard | PASS — the post-scan re-check covers it |
| M14c | `RefreshWorkspace` loses both guards | FAIL `TestWorkspaceRefreshNeverCreatesARoot` |
| M15 | `LoadProject` loses the `kind` guard | FAIL `TestLoadProjectRefusesWorkspace`, `TestIsMountFalseAtWorkspaceRoot` |
| M16 | `UndesignateWorkspace` removes `.bdrive` too | FAIL `TestUndesignateKeepsADirectoryItDidNotCreate` |
| M17 | the connect's undo stops un-rooting | FAIL `TestDesktopInitFailureUnroots` |
| M17b | the connect stops designating | FAIL `TestDesktopInitFounder` |
| M18 | hook guard also matches `workspace.json` | FAIL `TestHookGuardSkipsWorkspaceRoot` |
| M19 | `ScanWorkspace` stops stat-ing through symlinks | FAIL `TestWorkspaceRescanCorrectsStaleEntry` |
| M20 | nesting guard removed, `…ObeysTheRootRules` only | **PASS — vacuous assertion (F7-4)** |
| M20b | nesting guard removed, `…NeverReadsAnAncestor` only | **PASS — vacuous assertion (F7-4)** |
| M21 | `startSync` refreshes again (undo V3-3) | FAIL `TestSyncStartNeverScansTheWorkspaceRoot` (30 s hang) |
| M22 | `workspace.json` becomes a `ReservedName` | FAIL `TestWorkspaceRootNeverScanned` |
| P1/P2 | `panic()` in `CheckRootPlacement` | reached only from test fixtures calling `config.InitWorkspace` — never from `initCmd()` (F7-2) |

---

## What else I attacked (and found clean)

**Manifest reads are index-only.** Three readers exist: `IsWorkspaceRoot`
(existence + `kind`), `LoadWorkspace`, `RefreshWorkspace` (reads only to answer
"is this already a root"). `grep -rn "\.Projects"` over every non-test `.go`
file finds exactly one hit in this feature — `ScanWorkspace` *appending*
(`workspace.go:144`). **No production code reads a manifest entry at all**, so
no volume, journal, mount id or permission can be resolved from one.
`LoadWorkspace` still has zero non-test callers (round 1's Suspicion 7 remains
correctly parked in DESIGN.md "Not done").

**Every toucher of `.bdrive/config.json`, handed a root.** `projectConfigPath`
feeds `IsMount`, `LoadProject`, `SaveProject`, `mountLivesAt`; above them
`findMountRoot`, `syncTargets`, `mountsUnder`, `ResolveMount`, `EnrollMount`,
the syncer's per-directory `IsMount`, and the shell walk-up. At a root there is
no `config.json`, so `IsMount` → false and `LoadProject` → `ok=false`; both
correct, and both cost one `stat`/one absent-file `open`. For the hand-planted
collided layout, `LoadProject` refuses with "workspace root, not a project"
(M15) while `IsMount` says true — the documented, deliberate asymmetry, and Go
and shell agree on it (verified below). `desktop_onboard.go:435`'s
`os.RemoveAll(<target>/.bdrive)` is on the project folder, never the root.

**The agent-hook guard, by hand, with a real `/bin/sh`** (`/tmp/guard.sh` is
`mountGuard()` verbatim with `bdrive` replaced by a tracer; `/tmp/guard_drive.sh`
builds the tree):

```
at the workspace root                          -> WOULD SPAWN (registry: mounts below)
non-BearDrive sibling of the project           -> no spawn
deeper inside that sibling                     -> no spawn
another non-BearDrive sibling                  -> no spawn
inside the real mount                          -> WOULD SPAWN (d=<root>/team)
inside a subfolder of the mount                -> WOULD SPAWN (d=<root>/team)
above the root                                 -> WOULD SPAWN (registry: mounts below)
non-BearDrive folder under a COLLIDED root     -> WOULD SPAWN   <- the stated consequence
above a registered mount, no root involved     -> WOULD SPAWN
```

`sh -x` from the non-BearDrive sibling shows the whole budget: five `[ -f ]`
builtins (no process), one `case`, **one** `grep` of `mounts.json`, `exit 0`.
No `bdrive`, matching CLAUDE.md's invariant and `sec_hooks`/`sec_audit4`. The
collided-layout row is exactly what `agenthooks.go:98-108` and DESIGN.md say
happens, and M18 proves the current file name is what produces the other rows.

**CLAUDE.md §Invariants.** Journals, blob-before-journal ordering, scan-before-
pull, `Less`/`Replay`, dirty-file materialize and the volume flock are untouched
by this diff (no file under `internal/syncer`, `internal/journal` or
`internal/store` is modified; the only syncer addition is a test). Atomic
writes: `SaveWorkspace` → `writeJSON` → `os.CreateTemp(dir, ".bdrive-tmp-*")` +
`os.Rename`, and `.bdrive-tmp-` is already ignored by the scanner. Daemon
liveness is still the flock — the new refresh runs after `announce`, in a
goroutine, and touches neither the pidfile nor the lock. Errors degrade: the
daemon logs a failed refresh and keeps running; a failed designation is written
to the sidecar's stderr and the connect still reports `done` (observed in the
`dead-symlink` and `fifo`-as-`.bdrive` probe rows above).

**Row 10's direction still holds.** A user's own `workspace.json` inside a
project syncs (`ReservedPath("notes/workspace.json") = false`), and making it
reserved is caught (M22).

**DESIGN.md's two round-6 corrections.** The registry-walk claim ("for every
enrolled project below the named folder it walks up to that folder looking for
a root, so a project nested any distance under its root is found") is accurate
and proven by M1. The connect-does-not-enforce-nesting claim is accurate as far
as it goes; its enforcement attribution and its second half are F7-2 and F7-3.

---

## Suspicions (unproven)

1. **`workspaceRootUnder`'s per-level `IsWorkspaceRoot` is still an unbounded
   read**, now once per level instead of once per mount. I argue above that it
   is not a regression (the parent-only version reads on the same wedged NAS)
   and that `bdrive init` is the accepted place for it, but I did not attempt a
   real wedged network mount, only FIFOs — and a FIFO there has to be planted
   locally, so I cannot claim a probability. `TestInitNeverScansForRoots` does
   not cover this direction: with no registered mount under the folder, its
   loop never runs. A FIFO test with a real registered mount two levels down
   would settle it.
2. **`mountsUnder("/")` matches nothing**, because it builds the prefix `"//"`.
   Harmless here (`workspaceRootUnder("/")` just returns false) but it also
   means `syncTargets("/")` is empty. Pre-existing, unrelated to this feature,
   not investigated further.
3. **`underPath`'s case folding could over-refuse on a genuinely
   case-sensitive volume** where two differently-cased paths are distinct
   directories. The code names the trade-off; I did not build a case-sensitive
   volume to measure it.
4. `TestDesignateWorkspaceNeverReadsAnAncestor` creates `plain :=
   filepath.Join(base, "deep", "Other")` and never uses it. Cosmetic; possibly
   the residue of an assertion that was meant to check the ancestor rules
   against a folder *outside* the new root (which would have exercised the
   FIFO ancestor and hung). Not investigated.

---

## What I could not check

- **Windows and Linux.** Everything ran on macOS (darwin 25.2.0). The
  case-folding and `/private/var` aliasing that F7-1 turns on are macOS-shaped;
  the same asymmetry exists on any platform whose `$HOME` contains a symlink,
  but I did not run it there. `GOOS=windows go build ./...` still does not pass
  at HEAD for reasons CLAUDE.md documents (unrelated to this feature).
- **A real TCC-gated or wedged-network path.** Every blocking probe used FIFOs.
  I could not construct an ancestor that is unreachable while the root itself is
  readable, so the "realistic case" the comments name is still untested by
  anyone.
- **The desktop UI.** I drove `POST /api/desktop/init` and
  `/api/desktop/init/status` directly; nothing in `desktop/` (Rust/Tauri) was
  built or exercised, so how a failed designation surfaces to a user is
  unverified beyond "written to the sidecar's stderr".
- **`internal/webapp` in detail.** It is in the green gate (2669 s) but is not
  touched by this diff and I did not read it.
- **Concurrency at scale.** I reasoned about two daemons refreshing one root
  and checked the write is atomic with unique temp names, but did not run a
  stress test with many projects under one root.
