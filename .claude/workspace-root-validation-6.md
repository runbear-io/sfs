# Workspace root — validation round 6

**Verdict: 5 findings.** One high (self-inflicted by round 5, reproduced end to
end through the real `POST /api/desktop/init`), one medium (inherited, proved
as a real content leak through the syncer), three low.

Round 5's headline fix — `CheckRootPlacement`, the extraction created to stop
`DesignateWorkspace` losing rules — **lost no rule** (F6-A below proves all four
guards fire identically from both entry points, by mutation). But the way it
recovered them put an **unbounded ancestor walk back on the desktop connect's
critical path**, which is the exact class rounds 2, 3 and 4 each spent a round
removing. That is F6-1, and it makes this the fourth round in five where a
fix introduced the round's worst bug.

## Gate, run here

```
go build ./...                          OK
go vet ./...                            OK
go test ./... -count=1 -timeout 5400s   EXIT=0, 12/12 packages
    cmd/bdrive 132s · agenthooks 21s · autostart 8s · config 5s · daemon 42s
    journal 17s · remote 7s · secrets 3s · store 6s · syncer 51s
    templates 1s · webapp 1781s
gofmt -l <19 touched .go files>         clean
```

All 30 test names DESIGN.md §Status and the goal matrix cite exist and pass
(run by name, output in "What I attacked"). The two deleted names
(`TestWorkspaceRefreshOnSyncStart`,
`TestInitRefusesAFolderContainingAnUnenrolledRoot`) appear nowhere in the tree.

Method: `git stash` was not used. All mutation and probe work ran against a
`tar`-piped copy at `/tmp/ws6`; the worktree was never modified (`git status
--short` identical before and after).

---

## Findings

### F6-1 — high — `CheckRootPlacement`'s ancestor walk puts unbounded reads back on the desktop connect; the connect wedges forever with no error and 409 on every retry

`internal/config/workspace.go:196-204` (the walk), reached synchronously from
`DesignateWorkspace` (workspace.go:278) ← `cmd/bdrive/desktop_onboard.go:490`.

The walk does `IsWorkspaceRoot(cur)` — an `os.ReadFile` of
`<ancestor>/.bdrive/workspace.json` — plus `IsMount(cur)` (`os.Stat`), for every
ancestor up to `/`. Neither is bounded. Round 5 introduced this walk; before
it, `DesignateWorkspace` read only the root's own manifest path (F5-5,
accepted) and did pure path arithmetic.

Three places now state the opposite:

- `workspace.go:186-188`: "one stat plus one small read per ancestor. No
  directory listing and no read of anything below folder, **so it is safe on
  the paths a UI blocks on** — unlike ScanWorkspace."
- `desktop_onboard.go:475-479`: "Scan-free … one stat and one atomic write,
  over a directory this flow has already statted and written inside, so it
  needs no probe and **cannot wedge the connect**."
- `DESIGN.md:171-172`: "one stat, one atomic write, over a directory the flow
  has already **proven reachable**".

The last is the load-bearing error: F5-5's justification for leaving one
unbounded read in place was that the connect had proven *the root* reachable.
The flow has proven nothing about the root's ancestors — `validateShared` stats
`root` only (`desktop_onboard.go:100-126`), and `mountConflict` only stats
registered mount paths.

**Failure scenario.** A FIFO at any ancestor's `.bdrive/workspace.json`.
Driving the real endpoint (`/tmp/ws6/cmd/bdrive/zz_probe6_test.go`, real
`desktopHandler()` over `httptest`, root = `<base>/ancestor/MyProjects`, FIFO at
`<base>/ancestor/.bdrive/workspace.json`):

```
POST /init -> 200 {"mount":".../ancestor/MyProjects/team","ok":true}
after 20s:  phase=connecting  detail=connecting to http://127.0.0.1:52929  err=
retry POST /init -> 409 a folder is already being connected
target IsMount=true
registry=map[]
WEDGED: connect never finished
```

So: the hub project was created, `<root>/team/.bdrive/config.json` was written,
`EnrollMount`/`startSync`/the daemon never ran, `undo` never ran, the UI sits at
"connecting" with no error, and `onboarding.running` stays true so **every
retry gets 409 for the life of the sidecar**. Restarting the app clears
`running` but re-enters the same hang. This is verbatim round-4 F2's outcome
("wedging the connect at 'connecting' forever"), which that round closed.

**Pinpointed to the walk, not to F5-5's known read** — with the FIFO two levels
above the candidate root (`/tmp/ws6/probe6e`):

```
IsWorkspaceRoot(root)  [root's own file]    false      <- returns
IsMount(root)                               false      <- returns
CheckRootPlacement(root)                    *** HUNG (3s) ***
DesignateWorkspace(root)                    *** HUNG (3s) ***
InitWorkspace(root)                         *** HUNG (3s) ***
```

**Scope.** Desktop connect only. `bdrive init` does not call
`CheckRootPlacement`; its two guards are one read of the folder the user named
(`init.go:199`) and the registry-only `workspaceRootUnder`, still pinned by
`TestInitNeverScansForRoots`.

**Honest on reachability.** A FIFO is the reproducible trigger; I could not
build a real TCC-gated or wedged-network ancestor that is unreachable while the
root itself is readable, so I am not claiming a probability. The reason it is
still graded high is the repo's own standard: rounds 3 and 4 treated "an
unbounded filesystem read on a UI-blocking path" as the bug regardless of
trigger (V3-2, V3-3, round-4 F1/F3), and the three comments above assert the
property that is now false. `probe` is explicitly ruled out as the remedy by
round 4's F3, so no cheap wrapper exists — which is precisely why the claim
mattered.

**No test covers it.** `TestDesignateWorkspaceIsScanFree` plants its FIFO at a
*child* (`<root>/wedged/.bdrive/config.json`); nothing plants one in an
ancestor.

---

### F6-2 — medium — the containing-root guard only looks at a mount's immediate parent, so a root whose enrolled project sits deeper is invisible and its private folders sync to the whole team

`cmd/bdrive/helpers.go:118` — `if parent := filepath.Dir(m); config.IsWorkspaceRoot(parent) …`.

DESIGN.md:219-220 claims: "it finds an enrolled project under a root **at any
depth**, over paths this device already syncs." The code checks exactly one
level: `filepath.Dir(mount)`. A mount at `<root>/a/b/deep` — reachable with
nothing but plain CLI use, `bdrive init <root>/a/b/deep`, which is allowed —
leaves `<root>` invisible to the guard.

```
only a DEEP enrolled mount under the root: workspaceRootUnder(outer) = "" found=false
with an IMMEDIATE enrolled child too:      workspaceRootUnder(outer) = ".../Projects" found=true
```
(`/tmp/ws6/cmd/bdrive/zz_probe6c_test.go`)

`bdrive init <outer>` is therefore not refused, and the harm `init.go:214-218`
describes in its own error text follows. Confirmed end to end through the real
syncer, two devices over a shared `file://` remote
(`/tmp/ws6/internal/syncer/zz_probe6_test.go`):

```
peer received: [Projects/private/secret.txt readme.md]
LEAK: Projects/private/secret.txt reached the peer
```

The root's manifest itself does not leak (`.bdrive` is reserved), and the
nested mount is excluded — it is exactly the folders the root exists to hold
apart that go.

This is **inherited** (round 4's registry-only rewrite), not round 5's doing.
It is reported because it is a second, undocumented gap: DESIGN.md §Status
names only the "never enrolled" gap and asserts the any-depth property this
does not have, and `TestInitDoesNotSeeAnUnenrolledRoot` pins the documented gap
while this one is unpinned in either direction. Round 4's justification for
the gap it *did* document ("closing it means reading folders the user did not
name") does not cover this one: every path involved is already in the registry.
I am not prescribing the walk-every-ancestor version, since that is exactly the
shape F6-1 is about.

---

### F6-3 — low — F5-2 is recorded as fixed but only its case-folding half shipped; the symlink-alias instance round 5 documented still writes the manifest into the beardrive home

`internal/config/workspace.go:226` (`checkRootAllowed`) with
`internal/config/project.go:296` (`samePath`).

Round 5's report body documented two instances under F5-2. The goal doc's
disposition table carried only the first (case), and `underPath`'s fold fixes
only that. The second — alias spelling with a not-yet-existing `<root>/.bdrive`
— reproduces unchanged, because `samePath` returns false when `EvalSymlinks`
fails on a path that does not exist yet, and `underPath` is lexical so it
cannot see through a symlink:

```
BDRIVE_HOME  = <base>/real/state/.bdrive
root spelled = <base>/alias/state          (alias -> real)

CheckRootPlacement(real)  = ... cannot be a workspace root: its .bdrive and the
                            beardrive home ... contain one another
CheckRootPlacement(alias) = <nil>
DesignateWorkspace        = created=true err=<nil>
beardrive home now holds: workspace.json
IsWorkspaceRoot(real state) = true
```
(`/tmp/ws6/probe6c`)

That is the V3-7 outcome the guard exists to prevent, by a third spelling.
Reachability is genuinely low — it needs a `BDRIVE_HOME` nested below
`<root>/.bdrive` *and* a symlinked spelling of the root, since when
`<root>/.bdrive` already exists `samePath` resolves and refuses correctly. The
finding is that a hole which was found, written down and then silently dropped
from the ledger now reads as closed.

`TestWorkspaceRootRefusalCoversTheWholeHome:492-497` asserts the case property
on the unexported `underPath` rather than through a refusal, so nothing in the
suite would notice either spelling regressing at the guard level. (I separately
confirmed the case fix does work end to end through `CheckRootPlacement` —
`/tmp/ws6/probe6b`.)

---

### F6-4 — low — "roots do not nest" is enforced only upward; two ordinary desktop connects produce nested roots

`internal/config/workspace.go:196-204`. `CheckRootPlacement`'s own doc calls
itself "the single copy of DESIGN.md §Rules" and lists "roots do not nest, and
a root is never inside a project" (workspace.go:182-183). It walks ancestors
only, so a candidate root that **contains** a root is allowed. DESIGN.md:137
and `web/docs/.../project-files.md:109-110` both state the rule without
qualification; DESIGN.md documents a descendant gap only for `bdrive init`,
never for designation.

Two real connects, child first then parent
(`/tmp/ws6/cmd/bdrive/zz_probe6b_test.go`, real endpoint; the fake hub has to
be seeded with a second project name, because `onboardHub` mints one fixed id
for every newly-created name and `checkNotAlreadyMounted` then refuses — a
harness artifact, not product behaviour):

```
connect #1 (root=<base>/proj)  -> phase=done ; IsWorkspaceRoot(inner)=true
connect #2 (root=<base>)       -> phase=done
IsWorkspaceRoot(outer)=true IsWorkspaceRoot(inner)=true
NESTED ROOTS: <base>/proj is a root inside the root <base>

bdrive init <base> -> "<base> contains the BearDrive workspace root at <base>/proj …"
```

Note the asymmetry in the last line: the CLI refuses the shape the UI just
built. **I could not demonstrate harm** — the outer manifest simply omits the
inner root (it is not a mount, so `ScanWorkspace` skips it), `RefreshWorkspaceOf`
targets the correct level for each project, and `findProject`/`notAProject`
report the nearest root correctly. Graded low as a design-conformance gap, not
a bug with a victim.

---

### F6-5 — low, test-only — `TestRefreshDoesNotResurrectADeletedManifest` is timing-dependent, and losing its race turns it into a package-wide hang rather than a failure

`internal/config/workspace_test.go:510-556`, specifically the `time.Sleep(200 *
time.Millisecond)` at :532 followed by `os.OpenFile(fifo, os.O_WRONLY, 0)` at
:536.

The test is genuinely load-bearing for what it claims — deleting the post-scan
re-check fails it in 0.28s:

```
workspace_test.go:553: a refresh in flight rewrote a manifest the user had deleted
--- FAIL: TestRefreshDoesNotResurrectADeletedManifest (0.28s)
```

But the ordering is a sleep, not a synchronisation. If the refresh goroutine has
not reached the FIFO within 200ms, `RefreshWorkspace`'s *leading*
`IsWorkspaceRoot` sees the already-deleted manifest, returns immediately, and
**nobody ever opens the FIFO for reading** — so the test's own `O_WRONLY` open
blocks forever. Setting the sleep to 0 reproduces it exactly:

```
FAIL github.com/runbear-io/beardrive/internal/config 61.327s
goroutine 22 [syscall]: ... os.OpenFile(... O_WRONLY ...)
    config.TestRefreshDoesNotResurrectADeletedManifest(...) workspace_test.go:536
```

Under the gate's `-timeout 5400s` that is a 90-minute hang ending in a
package-wide panic that takes every other `internal/config` test with it, not a
clean failure. Related and smaller: any `t.Fatal` after the goroutine starts
(e.g. :534) leaves `RefreshWorkspace` blocked on the FIFO for the life of the
test binary. 200ms is generous for the work involved, so this is a latent flake
rather than an observed one — but the failure mode is disproportionate to the
race it is guarding.

---

## Round-5 findings F5-1 … F5-8, confirmed or refuted

| # | Round-5 claim | Verdict |
|---|---|---|
| **F5-1** | `DesignateWorkspace` lost `InitWorkspace`'s placement rules; one shared `CheckRootPlacement` restores them, scan-free, safe where `ScanWorkspace` is not | **CONFIRMED on the rules, REFUTED on "safe" → F6-1.** No rule was lost in the extraction: mutating each of the four guards out individually fails a test on **both** entry points (see F6-A). A 13-shape differential of `InitWorkspace` vs `DesignateWorkspace` agrees on every placement. But "scan-free … safe on the paths a UI blocks on" is false — the ancestor walk hangs the real connect. |
| **F5-2** | `underPath` folds case so the home refusal fails closed | **HALF CONFIRMED → F6-3.** Removing the fold fails `TestWorkspaceRootRefusalCoversTheWholeHome`, and the fold does refuse the case variant end to end through `CheckRootPlacement`. The symlink-alias instance round 5 documented under the same finding was never fixed and still writes into the beardrive home. Folding is monotone (it is tried only after the exact check said "not under"), so it can only ever **over**-refuse — I could not construct a false negative. It over-refuses `/data/BDRIVE` vs `/data/bdrive` on a case-sensitive volume, and on Unicode (`/x/K` Kelvin-sign vs `/x/k`); both are the fail-closed direction the comment names. `samePath` is still consulted; the three checks are OR'd, so they cannot "disagree" into a permission — only into a refusal. |
| **F5-3** | round-4 F6's post-scan re-check now has a deterministic FIFO test | **CONFIRMED as load-bearing, QUALIFIED on "deterministic" → F6-5.** Deleting the re-check fails it. The delete does land mid-scan (`ScanWorkspace` reads `slow` before `team`, `.bdrive` is skipped, so the FIFO is hit first — that part is deterministic); the *start* of the scan is not synchronised, and losing it hangs the package. |
| **F5-4** | README and `project-files.md` now say to stop syncing before deleting the manifest | **CONFIRMED.** README:313-314 ("Stop syncing first (`bdrive stop`): a re-index already in flight can write the file back once") and project-files.md:119-121 both carry it, alongside DESIGN.md:208. The advice is sound: `bdrive stop` stops the daemon *and* sets `store.SetPaused`, so a turn hook cannot restart the one thing that rewrites the file. |
| **F5-5** | `DesignateWorkspace`'s own unbounded manifest read is accepted and stated | **CONFIRMED as stated, but its justification no longer covers the code.** The read is still there and still hangs on a FIFO. The reason given — "it cannot decide without reading the file it is about to write, and the connect flow has already proven the root reachable" — does not extend to the ancestors round 5 added; that is F6-1. |
| **F5-6** | `UndesignateWorkspace` removes the manifest only; an empty `.bdrive` left behind is inert | **CONFIRMED, and the assertion is correct in both directions.** Mutation: re-adding `os.Remove(<root>/.bdrive)` fails `TestUndesignateKeepsADirectoryItDidNotCreate`; mutating `undo` to un-designate unconditionally fails `TestDesktopInitFailureKeepsAPreExistingRoot`. Inertness verified everywhere it was asked about: `IsMount`=false, `IsWorkspaceRoot`=false, `LoadProject`/`LoadWorkspace` clean `(zero,false,nil)`, a parent root's `ScanWorkspace` does not index it, `SaveProject` afterwards succeeds and the folder becomes a normal mount, `mountConflict` skips it (`!config.IsMount` → stale row), and the syncer never sees it (`.bdrive` is `ReservedDir`). **The agent-hook walk-up does not stop there** — verified by hand with a real `/bin/sh` running the verbatim guard (table below): an empty `.bdrive` has no `config.json`, so `[ ! -f … ]` keeps climbing, 0 spawns, 1 grep. |
| **F5-7** | `TestDesktopInitFailureKeepsAPreExistingRoot`'s assertion is now about the manifest surviving untouched, not about an edit | **CONFIRMED, and it is not vacuous.** Mutating `DesignateWorkspace` to append the entry at an existing root fails it with `manifest = [{notes …} {team …}], want the pre-existing entry, untouched`. Its sibling assertion (`undo` keeps `<root>/team`) is also load-bearing: making `undo` remove the target unconditionally fails it. Both corrected assertions describe what the code does **and** what it should do — `undo`'s own contract is "a folder the user already had keeps its contents". |
| **F5-8** | `InitWorkspace` now carries the blocking warning and points at `DesignateWorkspace` | **CONFIRMED in the code, still stale in DESIGN.md.** workspace.go:306-316 has the warning. DESIGN.md:77-78's implemented-API list still reads `Workspace, LoadWorkspace, SaveWorkspace, ScanWorkspace, RefreshWorkspace(Of), InitWorkspace` — naming the function with zero production callers and omitting `DesignateWorkspace`/`UndesignateWorkspace`/`CheckRootPlacement`/`IsWorkspaceRoot`, i.e. every function the product actually runs. Trivial; folded here rather than raised as its own finding. |

### F6-A — the extraction lost nothing (the thing F5-1 existed to prove)

Each rule mutated out of `CheckRootPlacement` one at a time, run against both
entry points:

| mutation | fails (InitWorkspace path) | fails (DesignateWorkspace path) |
|---|---|---|
| drop `IsMount(folder)` | `TestWorkspaceRefusesNesting:308` | `TestDesignateWorkspaceObeysTheRootRules:570` |
| drop `checkRootAllowed` | `TestWorkspaceRootIsNeverTheBdriveHome:362`, `…CoversTheWholeHome:470` | `…CoversTheWholeHome:470` |
| drop ancestor `IsWorkspaceRoot` | `TestWorkspaceRefusesNesting:303` | `…ObeysTheRootRules:586` |
| drop ancestor `IsMount` | `TestWorkspaceRefusesNesting:315` | `…ObeysTheRootRules:595` |

Differential over 13 placement shapes (plain / already-a-mount / inside a mount
/ deep inside a mount / inside a root / deep inside a root / contains a mount /
contains a root / `$HOME` / holds the bdrive home deeper / nonexistent / a file
/ collided root): **one** disagreement, `nonexistent folder` —
`InitWorkspace` refuses (its `ReadDir` fails) while `DesignateWorkspace` allows
and `MkdirAll`s the tree. Unreachable from the connect (`validateShared` has
already required root to be an existing directory) and benign; recorded, not
raised.

**Termination** (item c) is clean on every shape I could construct — relative
`b`, `.`, `..`, `""`, `/`, a path containing `..`, raw `a/../a/b/../b`,
trailing slash, doubled slash, a symlinked chain, and a symlink cycle
(`cyc1 -> cyc2 -> cyc1`): all returned. `filepath.Dir` cleans and reaches a
fixed point at `/` or `.`, and the walk is lexical so a link cycle cannot trap
it. One consequence worth writing down: a **relative** path terminates at `.`,
so the ancestors above the CWD are never checked. No production caller passes a
relative path (`validateShared` requires `filepath.IsAbs`).

---

## Other things I attacked and found clean

- **The agent-hook guard, by hand, with a real `/bin/sh`.** The verbatim
  `mountGuard` + `bdrive sync . --hook` body, in a real root layout, with a
  fake `bdrive` and a counting `grep` on `PATH`. Includes the new case F5-6
  creates — a folder whose `.bdrive` is empty because un-designation left it:

  | cwd | bdrive spawns | greps |
  |---|---|---|
  | `<root>` | 1 (correct — registry finds a mount below) | 1 |
  | `<root>/not-beardrive` | **0** | 1 |
  | `<root>/not-beardrive/src/deep` | **0** | 1 |
  | `<root>/plain` | **0** | 1 |
  | `<root>/undesignated` (empty `.bdrive`) | **0** | 1 |
  | `<root>/undesignated/inner` | **0** | 1 |
  | `<base>/lone` (empty `.bdrive`, no root above) | **0** | 1 |
  | `<root>/team` | 1 | **0** |
  | `<root>/team/docs/deep` | 1 | **0** |

  Exactly one `grep`, of `mounts.json`, and none at all inside a real mount —
  the CLAUDE.md budget. No spawn outside a BearDrive project at any depth.

- **Every manifest read is index-only.** Exhaustive grep of `LoadWorkspace` /
  `ScanWorkspace` / `IsWorkspaceRoot` / `.Projects` / `WorkspaceProject{`
  outside tests: `LoadWorkspace` still has **zero non-test callers**;
  `ScanWorkspace` only appends what it just read from disk;
  `DesignateWorkspace` writes the caller's own literal. The three production
  `IsWorkspaceRoot` call sites (`share.go:250`, `helpers.go:118,129`,
  `init.go:199`) consume a boolean. No volume, journal, mount id or permission
  is resolved from the file anywhere.

- **Every toucher of `.bdrive/config.json`, handed a root.** At a real root all
  16 non-test `config.IsMount` / `config.LoadProject` call sites get
  `false` / `(zero,false,nil)` and route to `notAProject`. At a *collided* root
  (manifest hand-planted in `config.json`) `IsMount` is true — deliberately,
  so Go and shell agree — and `LoadProject` errors; every caller handles it
  safely, including `daemon.go:433`, which treats it as a vanished config and
  **exits cleanly, propagating nothing** (CLAUDE.md's daemon rule).
  `syncer/walk.go:56` and `ignore.go:87` treat it as a nested mount, the safe
  direction.

- **Repo invariants (CLAUDE.md).** Nothing in the change set touches journal
  ownership, blob-before-journal ordering, scan-before-pull, `journal.Less` /
  `Replay`, or dirty-file materialization. `SaveWorkspace` → `writeJSON` is
  temp-file + rename with the `.bdrive-tmp-` prefix. Daemon liveness is still
  the `daemon.lock` flock; nothing new reads a pidfile. The refresh is a
  goroutine after `announce` and its error is logged, never fatal. The hook
  guard stays pure shell (verified above and by
  `TestHookGuardStaysPureShell`).

- **Docs.** All 30 test names in DESIGN.md §Status exist and pass (run by
  name). The new README §`.bdrive/workspace.json` and
  `project-files.md` §"a workspace root" both carry the stop-syncing-first
  caveat (F5-4) and both are accurate on: the CLI never creating one, the
  manifest listing paused/unenrolled folders, "a corrupted manifest has the
  same effect" (`configKind` returns `""` for an unparseable body, so
  `IsWorkspaceRoot` is false), and `bdrive init` mounting the folder normally
  after the manifest is deleted (verified against `workspaceRootUnder`'s
  self-skip). Their one inaccuracy is the unqualified "roots do not nest" —
  F6-4.

- **Tests edited to match behaviour rather than to assert intent.** I looked
  specifically, given round 5 found one. I found none this round. Every
  changed assertion in `desktop_onboard_test.go` (`TestDesktopInitFounder`'s
  root-is-not-a-mount rewrite, `…FailureUnroots`' empty-`.bdrive` allowance,
  `…KeepsAPreExistingRoot`' two corrected assertions) states a property the
  product should have, and each fails under a mutation that removes that
  property (M5–M9 above).

## Suspicions (unproven)

1. **F6-1 may have a non-adversarial trigger I could not build.** iCloud Drive
   (`~/Library/Mobile Documents/…`, which `protectedRoots()` already names) and
   automount paths are the candidates: an `open(2)` on a nonexistent file under
   a stalled provider is the same syscall the FIFO wedges. I could not produce
   one where the root is readable and the ancestor is not.
2. **`workspaceRootUnder` inherits `mountsUnder`'s blocking `resolvePath`**
   (`EvalSymlinks` lstats every component of every registered mount path), so
   one mount on a dead network volume would hang `bdrive init`. Carried over
   from round 5's suspicion 2; still not built.
3. **The refresh goroutine can outlive `daemon.Run`** and write after it
   returned. Harmless for an index by construction; nothing cancels it.
4. **`DesignateWorkspace` checks `IsWorkspaceRoot` before `CheckRootPlacement`**,
   so once a manifest exists — however it got there, including via F6-3 — none
   of the placement rules ever run again for that folder. Carried over from
   round 5's suspicion 4.
5. **`LoadWorkspace` errors on a corrupt manifest while `IsWorkspaceRoot`
   returns false for the same bytes.** No production caller sees the
   divergence today (zero `LoadWorkspace` callers), and DESIGN §Not done
   already puts validation on the first caller.

## What I could not check

- Anything not macOS: `syscall.Mkfifo`, flock and `Setsid` work here only, and
  `GOOS=windows go build ./...` still does not pass at HEAD for the reasons
  CLAUDE.md documents.
- Real TCC-gated folders, real wedged network mounts, and a case-**sensitive**
  APFS volume. FIFOs stand in for the first two; I have no volume for the third.
- The Tauri shell. I drove the sidecar's HTTP API directly, so I cannot say
  whether the UI has its own timeout that would soften F6-1, or what it renders
  when `DesignateWorkspace` logs its non-fatal error to stderr.
- `internal/webapp`'s 1781s: run and green, but not audited — nothing in this
  change set touches it.
- Multi-daemon manifest refresh under real load (reasoned about the atomic
  write; did not race N real daemons).
