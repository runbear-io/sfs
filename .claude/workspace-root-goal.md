# Workspace root manifest — goal

Read this + `desktop/DESIGN.md` (§Workspace root and projects) first: that is
the decision, this is "done".

## Mission

A machine's BearDrive state hangs off one **workspace root** connected once.
The root carries `.bdrive/workspace.json` (`"kind": "workspace"`) — an index
of which immediate children are projects — beside folders BearDrive never
touches (DESIGN.md specified `config.json`; see the deviation below). The root
is never a mount, never syncs, nothing is keyed off the manifest. Ship that
without changing what a project folder means today.

## Done

1. Every matrix row closed by its named test.
2. `go build ./... && go vet ./... && go test ./...` green.
3. The validation subagent returns **zero** findings.
4. On a tree with no root, every existing test passes untouched — migration
   is none.

## The one rule

A row is open until a test **fails on the current tree and then passes**.
Never by reading code, a summary, or "it should work".

## Traps

- `LoadProject` accepts an empty `ID` (legacy pre-id config), so a manifest
  parses today as a valid identity-less project. It must refuse
  `kind: workspace` and say so.
- `IsMount` only `os.Stat`s — never parses. A root reads as a mount to every
  caller. DESIGN.md names only `LoadProject`.
- `agenthooks.mountGuard` walks up to the first `.bdrive/config.json`, so a
  root carrying one makes hooks fire inside non-BearDrive siblings. It runs on
  every tool call on the machine; spawning `bdrive` there fails the round
  (`agenthooks/sec_audit4_test.go`). Resolved by the manifest's own file name,
  so the guard needs no knowledge of workspaces at all.
- The manifest is rebuilt from a scan; a stale entry is corrected, never
  obeyed. Any volume/journal/mount-id/permission resolved *from* it is a
  broken design, not a bad entry.

## Matrix (add rows, never drop)

All closed 2026-09-01. Each test was verified to fail on the tree before its
fix, EXCEPT row 10, whose test is a regression guard: the syncer only ever
walks the mount, so a workspace root was already outside its reach and the
assertions hold with the feature absent. Its value is the direction nobody
would think to check — a user's own file named `workspace.json` inside a
project must keep syncing, i.e. no name-based rule was smuggled in.

1. DONE manifest type + read/write, relative paths — `TestWorkspaceManifestRoundTrip` + `TestWorkspaceManifestShape` (config)
2. DONE `LoadProject` refuses a workspace config — `TestLoadProjectRefusesWorkspace`
3. DONE `IsMount` false at a root, callers still right — `TestIsMountFalseAtWorkspaceRoot`
4. DONE rebuilt from scan, stale entry corrected — `TestWorkspaceRescanCorrectsStaleEntry`
5. DONE hook walk-up skips a root, keeps climbing, no spawn — `TestHookGuardSkipsWorkspaceRoot` + `TestHookGuardStaysPureShell`, `sec_hooks`/`sec_audit4` green
6. DONE `bdrive init` at a root refuses, before any network call — `TestInitRefusesWorkspaceRoot` (cmd)
7. DONE roots don't nest, a project holds no project — `TestWorkspaceRefusesNesting`
8. DONE desktop connect designates the root and un-designates it on failure; every daemon start refreshes it, off the startup path — `TestWorkspaceRefreshOnDaemonStart`/`TestWorkspaceRefreshNeverStallsTheDaemon` (daemon), `TestSyncStartNeverScansTheWorkspaceRoot`/`TestDesktopInitFounder`/`TestDesktopInitFailure*` (cmd)
9. DONE no-root layout unchanged — full suite + `TestLegacyProjectUnchanged`
10. DONE nothing at the root syncs, not in `ReservedDirs` — `TestWorkspaceRootNeverScanned` (syncer)

**Deviation from DESIGN.md, row 5.** The design put the manifest in the root's
`.bdrive/config.json` and told it from a project by a `kind` field. The
agent-hook walk-up would then have to inspect each ancestor's config to know
whether to stop there. Affordable (`read` is a shell builtin — validation F5
corrected an earlier claim that it cost a process per level), but it makes the
machine's hottest guard depend on knowing what a workspace is. The manifest is
`.bdrive/workspace.json` instead; the `kind` field and both Go-side guards
(rows 2, 3) stay for a hand-edited or older root. DESIGN.md updated.

## Validation — independent subagent

All rows closed and suite green → launch one general-purpose subagent given
**only** this file, `desktop/DESIGN.md`, and `git diff <merge-base>..HEAD` —
never a summary, since the summary is what is being checked. Brief:

1. Diff vs DESIGN.md §Workspace root: report anything the code does that the
   design doesn't say, or the design says that the code doesn't do.
2. Grep every manifest read; prove each is index-only.
3. Grep every toucher of `.bdrive/config.json` (`projectConfigPath`,
   `ProjectDir`, `IsMount`, `LoadProject`); check each when handed a root.
4. Reproduce the shell walk-up by hand from a non-BearDrive sibling under a
   root — no `bdrive` spawned — then confirm it still fires inside a real
   mount under that root.
5. Run build/vet/test itself; don't trust a pasted result.

It reports only — fixes nothing, grades nothing. Each finding becomes a row
or closes the same round. **A round ends on a zero-finding pass.**

## Round protocol

Lowest-numbered open row first (order ≈ dependency). Failing test first, then
the smallest change at the layer that owns it: identity in `internal/config`,
the guard in `internal/agenthooks`, the flow in `cmd/bdrive` — touching two
layers means one is a pass-through. Close the row with its check, update this
matrix and DESIGN.md §Status.

## Validation round 1 — 9 findings, all dispositioned

Report: `.claude/workspace-root-validation.md` (independent subagent, given
only this file, DESIGN.md and the raw diff). It confirmed the two structural
claims by attack: `LoadWorkspace` has zero non-test callers, and the hook
walk-up is correct and within budget in the shipped layout.

| # | Finding | Disposition |
|---|---|---|
| F1 | manifest never refreshed on resume/daemon start; 4 places claimed it was | fixed — moved into `daemon.Run`, which `bdrive resume` does pass through. `TestWorkspaceRefreshOnDaemonStart` + `TestWorkspaceRefreshNeverCreatesARoot` (`internal/daemon`) |
| F2 | docs promised a deleted/corrupt manifest self-heals; it does not | fixed as docs — a wrong *entry* self-heals, deleting the file un-roots the folder. README, `project-files.md`, DESIGN Rules all say so now |
| F3 | `kind` guards are Go-only; the shell walk-up can't see them | fixed as docs — DESIGN.md states the collided layout is unguarded in the hook path, and names the line that would have to learn about it |
| F4 | a failed desktop connect left the folder converted into a root, permanently | fixed — `undo` un-designates a root THIS run created, and leaves a pre-existing one untouched (round 5 F5-7: there is no dead entry to drop, since designation is a no-op at an existing root). `TestDesktopInitFailureUnroots` + `TestDesktopInitFailureKeepsAPreExistingRoot` |
| F5 | the deviation's stated reason ("a grep per level") is false — `read` is a builtin | fixed in the four named places; two more were missed and are V5 below |
| F6 | DESIGN claimed "a project folder does not contain another project" — the syncer handles nested mounts | fixed as docs, in DESIGN Rules and `ScanWorkspace` |
| F7 | row 10's test asserts little the feature owns | acknowledged above; the valuable half kept |
| F8 | `ScanWorkspace` silently skipped symlinked children | fixed — stat through the link; asserted in `TestWorkspaceRescanCorrectsStaleEntry` |
| F9 | every other command still advised `bdrive init` at a root, which refuses | fixed for six commands via a shared `notAProject`; `findProject`'s separate message was missed and is V2 below. `TestCommandsAtARootDoNotAdviseInit` |

Suspicion 7 (`LoadWorkspace` validates nothing it returns) is recorded in
DESIGN.md's "Not done" instead of fixed: with no caller, a `ValidMountID` +
traversal check would be a guard on a path nobody walks. The obligation is on
the first caller and it is written down where that caller will look.

**On the gate.** `go test ./... -count=1 -timeout 5400s` is GREEN, all 12
packages. `internal/webapp` alone dominates and its runtime swings with machine
load — 1953s under validation round 2, 1307s on an idle machine — so it needs
a `-timeout` well above Go's 600s default. An earlier claim here that it
"cannot pass under any timeout" was wrong: `-timeout 1800s` happened to be
short of that day's runtime, so those runs died on the alarm, not on anything
real. Done #2 holds as written, given the timeout.

## Validation round 2 — 6 findings, all fixed

Report: `.claude/workspace-root-validation-2.md`. It re-derived every round-1
disposition from the code and the real binary: seven confirmed, F5 partially
refuted, F9 refuted as stated.

| # | Finding | Fix |
|---|---|---|
| V1 | `ScanWorkspace` is unbounded, and this round put it on two blocking paths — a FIFO or wedged child hung the desktop connect forever with no undo, and `daemon.Run`'s refresh sat inside `daemon.Start`'s 10s window (20k siblings = 4s, before any blocking) | the daemon's refresh moved AFTER `announce` (the daemon is live first, worst case a stale index); the connect's designation and its undo wrapped in the `probe` bound this file already uses for every user-path syscall |
| V2 | F9 was half-fixed: `findProject` — the OTHER not-a-project message — still sent `share`/`restore`/`forget`/`url` to `bdrive init` at a root | `findProject` now reports the nearest workspace root it walked past. `TestShareFamilyAtARootDoesNotAdviseInit` |
| V3 | `bdrive init` ABOVE a root was never refused, so the folders a root exists to hold apart synced to the whole team | refused, via `workspaceRootUnder` (registry, not a walk down the disk). `TestInitRefusesAFolderContainingARoot` |
| V4 | README/docs said the manifest lists children that *sync*; it lists project folders, paused and unenrolled ones included | both surfaces corrected |
| V5 | the F5 correction missed two places, one of them the test DESIGN.md cites as its proof — DESIGN.md contradicted itself | both corrected |
| V6 | `sync_run.go`'s comment justified the duplicate refresh with a case that cannot happen (`--foreground` goes into `daemon.Run`) | comment corrected to the one real case |

Round 2 also confirmed, by mutation rather than by reading, that this round's
tests are load-bearing; that `LoadWorkspace` still has zero non-test callers;
and that the hook guard spawns nothing in a non-BearDrive folder under a root
while still firing inside a real project there.

Round 2's caveat on the F4 tests (they asserted only `phase == "error"`, so an
earlier failure would make them vacuous) is closed: both now pin the template
seed failure by name.

## Validation round 3 — 8 findings, all fixed

Report: `.claude/workspace-root-validation-3.md`. It ran the gate itself
(green, 12/12, `webapp` 1434s) and **refuted round 2's V1 on all three
sub-claims** — the two "fixes" that round made were worse than the bug.

| # | Finding | Fix |
|---|---|---|
| V3-1 | **critical, self-inflicted.** `mountsUnder(folder)` includes `folder`, so V3's guard read a project's OWN root as a root beneath it: `bdrive init` in any project under a root — the shipping desktop layout, and the documented way to resume after `bdrive stop` — was refused, stranding the project with no CLI route back | skip the folder itself. `TestInitInAProjectUnderARootStillWorks` (fails without the skip) |
| V3-2 | **critical, self-inflicted.** V1's "after announce" move turned a visible start failure into a silent outage: with a wedged sibling the flock says running, `status` says running, and the sync loop never begins | the refresh runs in a goroutine; sync never waits on the index. `TestWorkspaceRefreshNeverStallsTheDaemon` — a real FIFO, and it hangs when the call is inlined again |
| V3-3 | `startSync`'s copy of the refresh was still unbounded: `bdrive init` hung, and the desktop connect hung at "syncing" on every connect after the first | deleted. `daemon.Run` covers every gesture that reaches a daemon; a daemon that never spawns leaves the index stale, which is what an index may do. `TestSyncStartNeverScansTheWorkspaceRoot` replaces the test that asserted the deleted behaviour |
| V3-4 | the `IsMount` stat→ReadFile put a blocking read in the syncer's per-directory walk — `bdrive sync` hung where a HEAD binary finished in <1s — and bought nothing, since the manifest has its own file name | reverted to the stat. Go and shell now give the same answer for a collided root |
| V3-5 | `probe` cannot cancel its goroutine, so a timed-out designation still landed the manifest afterwards while `createdRoot` stayed false — F4 reintroduced through V1's own bound | `createdRoot` decided before the call; removing a manifest that was never written is a no-op |
| V3-6 | V3's refusal was registry-only, and "that case has nothing to leak" was false | immediate children checked too. Deeper unenrolled roots remain a stated gap. `TestInitRefusesAFolderContainingAnUnenrolledRoot` |
| V3-7 | `$HOME` as a connect root put the manifest inside `$BDRIVE_HOME`, beside the device token | `InitWorkspace` refuses. `TestWorkspaceRootIsNeverTheBdriveHome` |
| V3-8 | `sync_run.go`'s comment still scoped a call that ran unconditionally | moot — the call is gone |

The lesson worth keeping: three of these eight were introduced by the previous
round's fixes, and two of those were worse than what they fixed. **A fix to a
blocking/ordering problem needs its own failing test before it is believed** —
V3-1 shipped because no test covered `bdrive init` in a project under a root,
the most-travelled path in the product.

## Validation round 4 — 7 findings, all fixed

Report: `.claude/workspace-root-validation-4.md`. Gate verified independently
(green 12/12). **Three of the seven were introduced by round 3's fixes, two of
them worse than what they closed — the third round running in which the fixes
were the bug.**

| # | Finding | Fix |
|---|---|---|
| F1 | **self-inflicted.** V3-6's child scan hung `bdrive init` forever on one FIFO child — the same hazard V3-3 had just deleted from `startSync`, moved one function over onto the same command | scan removed; the guard is registry-only again and the unenrolled-root gap is stated in DESIGN.md and pinned by `TestInitDoesNotSeeAnUnenrolledRoot`. `TestInitNeverScansForRoots` fails if a scan comes back |
| F2 | **self-inflicted.** V3-5 hoisted `IsWorkspaceRoot` out of `probe`, wedging the connect at "connecting" forever | moot — designation no longer probes anything, see F3 |
| F3 | **V3-5 refuted.** `probe` cannot cancel, so `undo` removed a manifest that the abandoned goroutine then wrote at t≈4s | `DesignateWorkspace` is scan-free (one stat, one write of the project just created), so it needs no probe and completes synchronously. `TestDesignateWorkspaceIsScanFree` |
| F4 | the child scan missed symlinked roots | moot with the scan gone |
| F5 | the `$HOME` refusal was equality-only and failed open for a home that does not exist | containment on unresolved paths, applied by both entry points. `TestWorkspaceRootRefusalCoversTheWholeHome` |
| F6 | "nothing recreates the manifest" was false: a refresh in flight rewrites a deleted one | re-checked after the scan (narrowing, not closing), and README/docs/DESIGN say to stop syncing first |
| F7 | DESIGN.md §Status cited the deleted `TestWorkspaceRefreshOnSyncStart` | check list rewritten |

**The rule this round finally wrote down** (`workspace.go`, DESIGN.md):
`ScanWorkspace` may only be called where blocking forever is harmless — today
exactly one place, the daemon's goroutine. Three separate attempts to put it
somewhere else each hung a user-facing path, and `probe` does not make it safe
because it abandons rather than cancels.

One more self-inflicted bug, caught by the existing suite rather than by a
validator: `created, werr := config.DesignateWorkspace(...)` **shadowed** the
`created` that says whether the HUB created the project, which is what decides
whether the template is seeded — so a joiner would have seeded over a
teammate's content. Go accepts the redeclaration silently because `werr` is
new. Renamed `rootCreated`, and the reason is a comment now.

## Validation round 5 — 8 findings, all fixed

Report: `.claude/workspace-root-validation-5.md`. Gate verified independently
(green 12/12). **Round 4 broke the pattern**: every ordering/blocking fix it
made is real and mutation-proven, and the shadowing fix is load-bearing. The
findings here are narrower and of a different kind.

| # | Finding | Fix |
|---|---|---|
| F5-1 | **self-inflicted, medium.** `DesignateWorkspace` was written as `InitWorkspace` minus the scan and lost the placement RULES with it — a clone carrying `.bdrive/config.json`, picked as the connect root, became a mount AND a root, after which `bdrive init` there refuses forever. Nesting unenforced too | one `CheckRootPlacement` both entry points call. Scan-free (one stat, then a stat + small read per ancestor), so it is safe where `ScanWorkspace` is not. `TestDesignateWorkspaceObeysTheRootRules` |
| F5-2 | **inherited, medium.** `checkRootAllowed` was case-sensitive, so `$HOME` spelled `/HOME` on APFS wrote the manifest into the beardrive home — the V3-7 outcome by another spelling. (`store.UnderRoot` has the same hole; not fixed here) | `underPath` folds case on the containment retry: a refusal must fail closed. Asserted in `TestWorkspaceRootRefusalCoversTheWholeHome` |
| F5-3 | round-4 F6's code fix shipped with **no test** — deleting the re-check left every package green | `TestRefreshDoesNotResurrectADeletedManifest` makes the race deterministic with a FIFO: the scan blocks, the manifest is deleted mid-flight, the write must not resurrect it |
| F5-4 | F6's docs half landed only in DESIGN.md | README and `project-files.md` now say to stop syncing before deleting the manifest |
| F5-5 | `DesignateWorkspace` still reads its own manifest path unbounded (a FIFO there hangs it), and the "scan-free" test only plants FIFOs in children | accepted and stated: it cannot decide without reading the file it is about to write, and the connect flow has already proven the root reachable |
| F5-6 | `UndesignateWorkspace` removed a `.bdrive` the run did not create ("only if now empty" is also true of the user's own empty one) and unlinked a symlink whatever it pointed at | removes the manifest only. `TestUndesignateKeepsADirectoryItDidNotCreate` |
| F5-7 | `TestDesktopInitFailureKeepsAPreExistingRoot` asserted "keeps the manifest minus the dead entry" — nothing produces that; designation is a no-op at an existing root, so the entry was never added | comment and the round-2 disposition corrected; the assertion is now about the manifest surviving untouched |
| F5-8 | the "exactly one place" rule is true of shipped code (`InitWorkspace` has zero production callers) but nothing guards it, and it is exported and documented as API | `InitWorkspace` now carries `ScanWorkspace`'s warning and points callers at `DesignateWorkspace` |

Two of my own new assertions were wrong and the suite caught them: `.bdrive`
is now deliberately left behind by un-designation (F5-6), and `undo` keeps a
target folder the user already had — I had asserted both the other way round.

## Validation round 6 — 5 findings, all fixed

Report: `.claude/workspace-root-validation-6.md`. Gate verified independently
(green 12/12). It confirmed `CheckRootPlacement` lost **no** rule in the round-5
extraction (each guard mutated out individually fails a test on both entry
points) — but found that the way it recovered them put an unbounded read back
on the connect's critical path.

| # | Finding | Fix |
|---|---|---|
| F6-1 | **self-inflicted, high.** The ancestor walk reads a file per level of directories nobody named; a FIFO at an ancestor's manifest wedges `POST /api/desktop/init` forever at `connecting`, 409 on every retry, hub project created, no undo. **Fifth time an unbounded read reached a critical path in this feature** | split: `checkRootHere` (one stat + path math) is what the connect applies; `CheckRootPlacement` adds the ancestor rules and is used only by `bdrive init`, a foreground command a user can interrupt. `TestDesignateWorkspaceNeverReadsAnAncestor` |
| F6-2 | **inherited, medium.** `workspaceRootUnder` checked only each mount's immediate parent, so a root whose project sits deeper (`<root>/team/wiki`) was invisible and `bdrive init` above it synced the root's private folders to the team — proven end to end through the real syncer | walk every level from the mount up to the folder. `TestInitFindsARootAboveADeepProject` (fails with the parent-only check) |
| F6-3 | F5-2 shipped only its case half; the symlink-alias spelling still wrote the manifest into the beardrive home | resolve the FOLDER (which exists) and re-check the rebuilt `.bdrive` path — `EvalSymlinks` cannot resolve a `.bdrive` that is not there yet |
| F6-4 | "roots do not nest" was enforced upward only, so two ordinary connects build a shape `bdrive init` then refuses | now a deliberate, stated trade-off of F6-1's split rather than an accident |
| F6-5 | **my test was the hazard**: `TestRefreshDoesNotResurrectADeletedManifest`'s 200 ms sleep was a guess, and losing the race made its own blocking `open` hang the package for 90 minutes instead of failing | wait for the reader with a non-blocking `O_WRONLY` retry loop — ENXIO until the scan is actually blocked |

**The pattern, five rounds running:** every regression in this feature has been
an unbounded filesystem read placed on a path where blocking matters. The rule
is now in three places (`ScanWorkspace`'s doc, `CheckRootPlacement`'s doc,
DESIGN.md) and pinned by four FIFO tests — `TestInitNeverScansForRoots`,
`TestSyncStartNeverScansTheWorkspaceRoot`, `TestWorkspaceRefreshNeverStalls
TheDaemon`, `TestDesignateWorkspaceNeverReadsAnAncestor`. Anything new that
reads the filesystem in this feature needs one of those tests pointed at it
before it is believed.

## Validation round 7 — 5 findings, all fixed

Report: `.claude/workspace-root-validation-7.md`. Gate verified independently
(green 12/12), 28 mutations run, 25 caught.

**The blocking-read pattern did not recur** — the first round in six with no
new unbounded read on a critical path. F6-1's split held under direct attack:
seven hostile ancestor shapes (FIFOs at parent and grandparent, dead symlinks,
symlink loops, a FIFO *as* `.bdrive`) all settled in under 3s against the real
`POST /api/desktop/init`, and every syscall `checkRootHere` makes was
enumerated as a stat or path arithmetic.

| # | Finding | Fix |
|---|---|---|
| F7-1 | **self-inflicted, medium.** F6-3 canonicalised the FOLDER but not the HOME — `Home()` returns `$BDRIVE_HOME` verbatim, and `EvalSymlinks` fails on either side's last component when it does not exist yet, which is the only case that matters. An aliased home spelling still wrote the manifest INTO the beardrive home. **No test anywhere caught it**: deleting the whole block left 12/12 green | `resolveExisting` resolves as much of each path as exists and rejoins the rest, applied to both sides. `TestWorkspaceRootRefusalSeesThroughSymlinks` covers both directions and the common macOS shape (an ordinary folder under a symlinked parent, which must NOT be refused) |
| F7-2 | `CheckRootPlacement`'s comment claimed `bdrive init` calls it. Nothing does — `bdrive init` never designates a root, it only refuses to mount one | the doc now says so outright: both ancestor rules are dormant, enforced by tests, waiting for a gesture that can afford to block |
| F7-3 | the nesting refusal asserted a harm (`would sync its manifest to the whole team`) that `ReservedDirs` already prevents at any depth | message and reasoning corrected to the real one: two answers to "what is BearDrive here" that disagree |
| F7-4 | **two assertions I added last round were vacuous** — "InitWorkspace must still refuse…" pointed at directories that do not exist, so `ScanWorkspace`'s ENOENT passed them with the guard deleted | the directories are created, and both now match on the refusal's wording |
| F7-5 | **inherited.** `workspaceRootUnder` walked the registry's spelling of a mount, so a symlinked registration walked a different chain than the named folder, never hit the stop test, and could report a root ABOVE the folder as one beneath it | walk the resolved mount path. (The reported root is now the resolved spelling — the test compares directories, not strings) |

F6-2, F6-4 and F6-5 all confirmed fixed, including nested roots being provably
harmless (identity, volume dir, both manifests and `bdrive init` all checked)
and the FIFO handshake being hang-free and load-bearing under `-race`.

## Validation round 8 — 4 findings, all fixed

Report: `.claude/workspace-root-validation-8.md`. Gate verified independently
(green 12/12), 22 mutations run, 19 caught. **The code held**: `resolveExisting`
survived 20 hostile shapes (mid-path gaps, self and mutual symlink loops, dead
links, a FIFO as a component and as `.bdrive`, 200-deep, `/`, `.`, `""`, `..`,
relative, `..`-escape attempts) — all terminate, none block, no escape. F7-1
confirmed closed in both directions with no over-correction: symlinked `$HOME`,
`/tmp` vs `/private/tmp`, a project under a symlinked parent and a `/Volumes`
alias are all still accepted.

| # | Finding | Fix |
|---|---|---|
| F8-1 | **medium.** F7-5's fix was correct but pinned by NO test — reverting it left all of `cmd/bdrive` green. A mount enrolled under a symlink with a SHORTER chain makes the walk climb the wrong tree, never reach the stop test, and report a root ABOVE the folder as one beneath it: `bdrive init` then refuses a legitimate folder, naming a root it is actually inside | `TestWorkspaceRootUnderWalksTheResolvedMountPath`, built from the validator's exploit; it fails on the pre-fix line |
| F8-2 | **low.** `TestDesignateWorkspaceNeverReadsAnAncestor`'s nesting assertion passed for the wrong reason — its target did not exist, it matched no wording, and the refusal it actually got was the "inside a project" rule firing off the test's own FIFO. The sibling test 100 lines above carries a comment about exactly this trap | directory created, wording asserted. Deleting the nesting guard now fails three tests instead of one |
| F8-3 | **low, docs.** DESIGN.md still said the ancestor rules were "enforced by `bdrive init`" (nothing calls `CheckRootPlacement`), still carried the `ReservedDirs` reasoning round 7 corrected, and still described the home comparison as "on unresolved paths" — the bug F7-1 diagnosed | all three corrected; the dormancy is now stated outright |
| F8-4 | **low, docs.** `project-files.md` told users "roots do not nest" as fact while a shipped test requires nested roots to be creatable | reworded to what is true and why it is harmless |

Also corrected: `checkRootHere`'s doc claimed "one stat and path arithmetic"
while `resolveExisting` lstats every component of the folder and of
`$BDRIVE_HOME`. Never blocking, but the wording was wrong — it now says what
it costs.

## Validation round 9 — 3 findings, all fixed

Report: `.claude/workspace-root-validation-9.md`. (A first attempt at this
round stalled in Job 2; it had already caught DESIGN §Rules bullet 3 still
claiming the manifest "is not in `ReservedDirs`" when `.bdrive` demonstrably
is — fixed before the retry.)

**All three findings are the class rounds 1–6 found five times — an unbounded
blocking read where blocking is not affordable — in the one place eight rounds
never looked: the file `IsWorkspaceRoot` itself opens.** Every prior round
attacked scans of *children* and walks over *ancestors*.

| # | Finding | Fix |
|---|---|---|
| F9-1 | **medium.** `IsWorkspaceRoot` is `os.ReadFile`, not `os.Stat`, and `DesignateWorkspace` calls it first. A FIFO at `<root>/.bdrive/workspace.json` wedges the connect forever — `handleDesktopInit` runs a bare goroutine with no watchdog, so `onboarding.running` never clears and every retry 409s for the sidecar's life. Three shipped texts asserted this could not happen | `HasManifest` (a stat) answers "already spoken for?" wherever blocking matters — the connect, the ancestor walk, and all four CLI callers. `IsWorkspaceRoot` keeps its content check for the daemon goroutine and `LoadWorkspace`, and now says outright that it can block. `TestDesignateWorkspaceNeverReadsTheManifestPath` |
| F9-2 | **low.** `workspaceRootUnder`'s doc claimed "one stat per registered mount"; it was one unbounded read per intermediate directory, so a FIFO between a mount and the folder hung `bdrive init` | same `HasManifest` switch; the doc now matches |
| F9-3 | **low.** `SaveWorkspace`'s `MkdirAll` rebuilt the whole chain, so a refresh racing an `rm -rf` of the root recreated the directory (9/300 runs) | the root must already exist; only `.bdrive` is created. `TestSaveWorkspaceDoesNotResurrectADeletedRoot` |

Manifest reads are also bounded now (1 MiB): a 100 MB file at that path cost
5.4s per call, and every command that asks "is this a root?" paid it.

**Round-8 fixes: all four confirmed**, F8-1 and F8-2 by mutation, F8-3 by
building a workspace root inside a real mount and running a two-device sync.

**Held under attack:** concurrency (`-race` clean with 6 refreshers, 4 readers
and a designator, and with two real daemons under one root); 15 hostile
manifests (traversal, absolute paths, NUL, 200k-deep, 100k entries) all inert;
the hook guard hand-run against real `/bin/sh` across 45 cases including 14
hostile root names and `$CLAUDE_PROJECT_DIR` at a root; and the whole CLI
driven through the real binary over a root layout, plus root rename, project
rename and move-out.

## Validation round 10 — ZERO FINDINGS

Report: `.claude/workspace-root-validation-10.md`. Gate run by the validator:
build OK, vet OK, `go test ./... -count=1 -timeout 5400s` → 12/12, exit 0;
`gofmt -l` clean over the 19 touched files; `-race` clean over the workspace
tests. Round 9's F9-1, F9-2 and F9-3 all confirmed fixed, each new test
mutation-tested and load-bearing.

**The signature defect is structurally absent, not merely unobserved.** The
inventory rests on one fact: `IsWorkspaceRoot` — the only function that OPENS
a manifest — is reachable from exactly one production caller
(`RefreshWorkspace`), whose only production caller is the daemon's own
goroutine, where blocking is affordable by construction. Every other call site
(`bdrive init`, `notAProject`, `findProject`, `workspaceRootUnder`,
`DesignateWorkspace`, `CheckRootPlacement`) goes through `HasManifest`, a stat.

Attacked and held: FIFOs, symlink loops, 100-link chains, dangling links and
`/dev/zero` at every path the connect can reach (slowest return 7 ms); the
1 MiB cap against a 3000-project root (933 KB — unreachable by real data);
`SaveWorkspace`'s race at 300 trials (0 silent resurrections, was 9/300); the
hook guard by hand across 40 cases and 17 hostile root names (`workspace.json`
opened 0 times, `bdrive` never spawned).

### Accepted notes, not fixed after the passing round

- `desktop_onboard.go` says `createdRoot` is true iff a manifest exists; on the
  write-failure branch that is false. Demonstrated, and harmless — the only
  consumer is an undo that then no-ops.
- `findProject` does a per-ancestor unbounded `LoadProject` read. **Pre-existing
  and byte-identical at HEAD**, on a CLI budget, but it is F9-1's shape one file
  over. The next person to touch that walk should switch it to a stat.

## GOAL REACHED

All ten rows closed by named tests; the gate green; ten independent validation
rounds run, 55 findings raised and fixed, ending on a zero-finding pass. Future
work continues against this matrix — add rows, never delete them.
