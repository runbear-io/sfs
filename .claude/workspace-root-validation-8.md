# Workspace root — validation round 8

**VERDICT: 4 findings** (1 medium, 3 low). No new blocking/unbounded read on a
critical path — the signature defect did not recur for the second round running.
Two findings are the *same class round 7 named for itself*: a fix that no test
pins (F7-5), and an assertion that passes for an unrelated reason (F7-4, only
half repaired). Two are documentation contradicted by the code round 7 changed.

Everything below was produced by running something, not by reading. **22
mutations applied, 19 caught, 3 survived** — two are the findings above, the
third (`RefreshWorkspace`'s first `IsWorkspaceRoot` guard alone) is redundant
with its own post-scan re-check, i.e. deliberate defence in depth, not a gap.
Gate run by me end to end: green, 12/12, exit 0.

---

## Findings

### F8-1 — `workspaceRootUnder`'s round-7 fix (F7-5) is pinned by no test

- **Severity:** medium (a real, reachable mis-refusal; the fix is correct but
  one careless revert restores the bug silently)
- **File:** `cmd/bdrive/helpers.go:133`

```go
for cur := filepath.Dir(resolvePath(m)); resolvePath(cur) != self; cur = filepath.Dir(cur) {
```

- **Mutation:** revert `filepath.Dir(resolvePath(m))` → `filepath.Dir(m)` —
  exactly round 7's pre-fix code.
- **Result:** the **entire `cmd/bdrive` package stays green.**
  `go test -C /tmp/r8repo ./cmd/bdrive/ -count=1 -timeout 1800s` → `ok … 171.198s`,
  exit 0. `workspaceRootUnder` is package-private to `main`, so no test outside
  `cmd/bdrive` can reach it either. Round 7 wrote of F7-1 "No test anywhere
  caught it"; its own F7-5 fix shipped in that state.
- **Failure scenario (I built it and it reproduces):** a mount registered
  through a symlinked spelling whose chain is *shorter* than the real one. The
  unresolved climb never passes through the folder being examined, so
  `resolvePath(cur) != self` never fires and the walk continues *above* it.

  ```
  <base>/.bdrive/workspace.json       <- a root ABOVE the folder
  <base>/inner                        <- the folder `bdrive init` names
  <base>/inner/proj                   <- the real project
  <base>/alias -> <base>/inner/proj   <- how it is registered
  ```

  `mountsUnder` returns the registry's raw spelling (`helpers.go:62`), so
  `<base>/alias` is what gets walked: `Dir("<base>/alias")` = `<base>`, which
  resolves to itself, never equals `<base>/inner`, and `IsWorkspaceRoot` says
  yes. `bdrive init <base>/inner` is then refused with
  *"contains the BearDrive workspace root at `<base>`"* — a root it does not
  contain and is in fact *inside*. The user has no CLI route past it.
- **How verified:** probe test at `/tmp/r8repo/cmd/bdrive/zz_probe_test.go`.
  Against the shipped code: `workspaceRootUnder(<base>/inner) = "", false` →
  PASS. Against the pre-fix line: `= "<base>", true` → FAIL. So the fix is
  load-bearing *and* untested.
- **Fix shape:** land that probe (or equivalent) as a real test. Note the
  existing `TestInitFindsARootAboveADeepProject` cannot catch it — it creates
  no symlink, and the loop *condition* already resolves `cur`, which is why the
  unresolved *start* looks harmless in every non-symlinked layout.

### F8-2 — the assertion round 7 repaired for F7-4 is still vacuous

- **Severity:** low (test quality; the rule itself is pinned elsewhere, by
  `TestWorkspaceRefusesNesting` and `TestDesignateWorkspaceObeysTheRootRules`)
- **File:** `internal/config/workspace_test.go:751-759`
  (`TestDesignateWorkspaceNeverReadsAnAncestor`, final stanza)
- **Mutation:** delete the `IsWorkspaceRoot(cur)` "roots do not nest" branch
  from `CheckRootPlacement` (`internal/config/workspace.go:216-218`).
- **Result:** `TestDesignateWorkspaceNeverReadsAnAncestor` still **PASSES**.
- **Why:** the assertion targets `filepath.Join(root, "inside")`, a directory
  that **still does not exist** (round 7's F7-4 fix created the directories in
  the *other* test, not this one), and it asserts only `err == nil` — no
  wording match, unlike the F7-4 sibling. The refusal it actually receives is
  the *other* ancestor rule: the test plants a FIFO at
  `<base>/.bdrive/config.json`, `IsMount` is a bare `os.Stat` (which succeeds on
  a FIFO — it never opens it), so `<base>` reads as a mount and the walk returns
  *"is inside the project at `<base>`"*. Printed directly:

  ```
  IsMount(base) = true   <- the FIFO config.json stats fine
  InitWorkspace(root/inside) err = … is inside the project at <base>: a workspace
      root indexes folders that project already syncs
  ```

  So the line labelled "InitWorkspace must still refuse a root inside a root"
  is asserting the *root-inside-a-project* rule, and would keep passing if the
  nesting rule were deleted outright.
- **Also:** `plain := filepath.Join(base, "deep", "Other")` on line 752-755 is
  created and then never used — dead setup, and the tell that this stanza was
  edited rather than re-derived.
- **Sharpest form of it:** `internal/config/workspace_test.go:651-653` — in the
  *sibling* test round 7 did repair — carries the note explaining exactly this
  defect: *"The directory must EXIST for this to test the guard: InitWorkspace
  scans before it saves, so a missing folder fails with ENOENT and the
  assertion passes even with the nesting rule deleted."* The fix and its
  rationale landed there; the same defect 100 lines later did not get it.
- **How verified:** mutation above + `/tmp/r8repo/internal/config/zz_probe2_test.go`
  printing which refusal fires.

### F8-3 — DESIGN.md and the public reference state rules the code does not implement

- **Severity:** low (documentation only; no data at risk)
- **Files:** `desktop/DESIGN.md` §"Workspace root and projects" §Rules and its
  closing paragraph; `web/docs/src/content/docs/reference/project-files.md`
- Three distinct claims, each contradicted by shipped code. Round 7 fixed the
  Go doc comments (F7-2, F7-3) and left every prose surface behind — and
  `CheckRootPlacement`'s comment now *cites* DESIGN.md as agreeing with it.

  1. **DESIGN.md §Rules bullet 2** — "Roots do not nest, and a root is never
     inside a project. … **Enforced by `bdrive init`, not by the desktop
     connect**". `bdrive init` does not enforce either rule. Verified by grep:
     `CheckRootPlacement` has exactly one caller, `InitWorkspace`, which appears
     only in `*_test.go`; `cmd/bdrive/init.go` calls only `config.IsWorkspaceRoot`
     and `workspaceRootUnder`, both of which are about *mounting*, not about
     *creating a root*. `internal/config/workspace.go:206-209` says so outright:
     "SO NOTHING IN THE SHIPPED PRODUCT CALLS THIS. … Both ancestor rules are
     therefore dormant today". The two documents contradict each other.
  2. **DESIGN.md §Rules bullet 3** — "It is not in `ReservedDirs` because it
     never lives inside a mount." Two problems. A root *can* live inside a
     mount — the connect applies only `checkRootHere`, and DESIGN's own bullet 2
     admits two connects can produce a root inside a root. And the reasoning is
     the one round 7 corrected (F7-3): `internal/config/workspace.go:220-222`
     now says the opposite — "`.bdrive` is a `ReservedDir` at any depth, so it
     never does [sync]". I confirmed the code's version:
     `internal/syncer/walk.go:54` applies `ignoredDir` (→ `config.ReservedDir`)
     per directory at every level, and `TestWorkspaceRootNeverScanned` asserts
     `ReservedPath(".bdrive/workspace.json")` is true. The *real* reason
     `workspace.json` is not a reserved *name* is the one that test states: a
     user's own file of that name must keep syncing.
  3. **DESIGN.md closing paragraph** — "Containment rather than equality, **on
     unresolved paths**, so a home that does not exist yet still counts."
     Round 7 (F7-1) changed exactly this: `checkRootAllowed` now compares
     `resolveExisting(dir)` against `resolveExisting(home)`. Comparing
     *unresolved* paths is the bug F7-1 diagnosed, and the design still
     describes it as the design.
  4. **`project-files.md`** repeats claim 1 to end users as flat fact —
     "Roots do not nest, and a root is never inside a project." — with no
     "should" and no caveat, on a reference page whose neighbouring sentences
     ("`bdrive init` refuses to mount one") *are* enforced. A shipped test
     asserts the opposite: `internal/config/workspace_test.go:648-650` requires
     `DesignateWorkspace` inside an existing root to **succeed**
     (`created == true`), because "the connect flow does not walk ancestors" —
     and the connect flow is the only thing that makes roots.
- **How verified:** `grep -rn 'CheckRootPlacement|InitWorkspace' --include='*.go'`
  (all non-test hits are the definitions themselves); reading
  `cmd/bdrive/init.go:196-224`; `internal/syncer/walk.go:54`; and running the
  suite, which passes with both ancestor rules unreachable.

### F8-4 — `checkRootAllowed`'s own comments describe the pre-round-7 mechanism

- **Severity:** low (documentation only, but it sits two lines above the code
  it contradicts)
- **File:** `internal/config/workspace.go:242-243` and `254-258`
- The doc says: *"A home that does not exist yet still counts — **comparing
  unresolved paths is why**, since EvalSymlinks fails on a missing directory
  and an equality check would then pass."* The body then calls
  `resolveExisting` on **both** sides. The reason the missing-home case works
  is now `resolveExisting`'s rejoin, not "comparing unresolved paths".
- The body comment still describes round 6's approach: *"resolve the FOLDER —
  which does exist — and rebuild the path under it."* `resolveExisting` does
  not resolve the folder; it resolves the deepest prefix that resolves (which
  may be an ancestor when the folder itself is missing) and rejoins the tail.
- **How verified:** read the two comments against the three lines under them;
  behaviour confirmed by the `resolveExisting` shape table below.

---

## Round-7 findings — confirmed or refuted

| # | Verdict | Evidence |
|---|---------|----------|
| **F7-1** (both-side canonicalisation) | **Fix confirmed, correct in both directions, no over-correction** | Mutations M1 (resolve neither side), M2 (folder side only — the exact F7-1 bug), M3 (home side only), M4 (`resolveExisting` → identity), M5 (tail rejoined in reverse order) are **all caught**, by `TestWorkspaceRootRefusalSeesThroughSymlinks` / `…CoversTheWholeHome` / `…IsNeverTheBdriveHome`. Separately I ran the four macOS shapes the brief names against `checkRootHere`: symlinked `$HOME` with the project inside it, `/tmp` vs `/private/tmp`, a project under a symlinked parent, and an external volume under `/Volumes` aliasing the boot volume — **all four accepted**; the two aliased-home spellings (`/Volumes/Macintosh HD/Users/me` as root, and a home spelled through a symlinked `$HOME`) **both refused**. Probe: `/tmp/r8repo/internal/config/zz_probe3_test.go`. |
| **F7-2** (`CheckRootPlacement` doc claimed `bdrive init` calls it) | **Fixed in the Go doc; NOT fixed in DESIGN.md** — see F8-3.1 | grep: no non-test caller. |
| **F7-3** (nesting refusal asserted a harm `ReservedDirs` already prevents) | **Fixed in the code message and comment; DESIGN.md still carries the old reasoning** — see F8-3.2 | `internal/config/workspace.go:220-227` vs DESIGN §Rules bullet 3. |
| **F7-4** (two vacuous assertions) | **Half fixed.** The pair in `TestDesignateWorkspaceObeysTheRootRules` (lines 651-669) is genuinely repaired — directories created, both match on wording (`"nest"`, `"inside the project"`), and mutation M8 (drop the `IsMount` rule from `checkRootHere`) is caught there. The assertion in `TestDesignateWorkspaceNeverReadsAnAncestor` is **still vacuous** — see F8-2. | mutation M13. |
| **F7-5** (`workspaceRootUnder` walked the registry's spelling) | **Fix confirmed correct and load-bearing, but pinned by NO test** — see F8-1. Termination for a mount not under the folder confirmed (`filepath.Dir(cur) == cur` break; `TestInitDoesNotSeeAnUnenrolledRoot` and the intermediate-folder assertions in `TestInitFindsARootAboveADeepProject` exercise the non-match paths). Returning the **resolved** spelling breaks no caller: the only consumer is `init.go:214-219`, which interpolates it into a message; the folder named there is a real directory, so the resolved spelling is a valid, existing path a user can act on. | mutation M6 + probe. |

---

## `resolveExisting` — attacked directly

`internal/config/workspace.go:270`. Every case below was run with a 3-second
watchdog on a goroutine; **none blocked, all terminated**
(`/tmp/r8repo/internal/config/zz_probe_test.go`).

| shape | result |
|---|---|
| missing component in the **middle** (`<t>/a/missing/b`, `a` exists) | rejoined correctly, 1 ms |
| **self symlink loop** (`loop -> loop`), and mutual (`l1->l2->l1`) | terminates; `EvalSymlinks` ELOOPs at each level, the walk goes up, 8 ms / 304 ms |
| dead symlink, and a path *through* a dead symlink | terminates, 0 s |
| **FIFO** as the final component, as an interior component, and as `.bdrive` | terminates, 0 s — `EvalSymlinks` **lstats**, it never `open`s, so a FIFO cannot block it |
| 200-deep path, none of it existing | 20 ms |
| `/`, `.`, `""`, `..`, `../..`, `rel/path`, `a/../b` | all terminate; `Dir` reaches a fixed point (`/`→`/`, `.`→`.`) |
| `..` in the rejoined tail | **impossible**: `filepath.Clean(p)` runs *before* the loop, so interior `..` is gone and only a leading relative `..` can survive — which `Dir` consumes on the way up, never entering `tail` in an escaping position |
| escape attempt `<t>/a/../../../../../../../../etc` | `→ /private/etc` — i.e. exactly what the kernel resolves; `Clean` normalised it before any syscall |
| `<t>/toroot/nope/../../etc` with `toroot -> /` | `→ <t>/etc`; `Clean` again, no escape |

**Termination proof:** each iteration sets `cur = filepath.Dir(cur)` and
returns when `filepath.Dir(cur) == cur`, which `Dir` reaches for absolute
(`/`) and relative (`.`) paths alike; `tail` grows by one per iteration, so the
loop is bounded by path depth. **Non-blocking:** the only syscall is
`filepath.EvalSymlinks`, which is `Lstat`+`Readlink` per component. The
worst-case cost is `depth × 255` lstats (a symlink loop at every level) —
bounded, and measured at 304 ms for a shallow path.

`checkRootHere` and `DesignateWorkspace` were also run against F6-1's hazard
shapes: a FIFO **as** `<folder>/.bdrive`, and FIFOs at an ancestor's
`config.json` **and** `workspace.json`. All returned in under 10 ms.

---

## Everything else I attacked, and found nothing

**Mutation coverage of the rest of the change set** (17 caught / 19 applied):

| mutation | caught by |
|---|---|
| M7 `underPath` loses the case-fold retry | `TestWorkspaceRootRefusalCoversTheWholeHome` |
| M8 `checkRootHere` loses the `IsMount` rule | `TestWorkspaceRefusesNesting`, `TestDesignateWorkspaceObeysTheRootRules` |
| M9 `LoadProject` stops refusing `kind: workspace` | `TestLoadProjectRefusesWorkspace`, `TestIsMountFalseAtWorkspaceRoot` |
| M14 `IsWorkspaceRoot` ignores the kind | `TestWorkspaceManifestRoundTrip` |
| M15 `RefreshWorkspace` drops the post-scan re-check | `TestRefreshDoesNotResurrectADeletedManifest` |
| M16 `ScanWorkspace` stops stat-ing through symlinked children | `TestWorkspaceRescanCorrectsStaleEntry` |
| M17 `RefreshWorkspace` may create a root (**both** guards removed) | `TestWorkspaceRescanCorrectsStaleEntry`, `TestRefreshDoesNotResurrectADeletedManifest` — removing only the *first* guard survives, but the post-scan re-check catches it, so the two are deliberate defence in depth, not a gap |
| M18 daemon refresh inlined (no goroutine) | `TestWorkspaceRefreshNeverStallsTheDaemon` (hung 15 s, then failed) |
| M19 daemon refresh deleted | `TestWorkspaceRefreshOnDaemonStart` |
| M11 `workspaceRootUnder` drops the self-skip (V3-1's bug) | `TestInitInAProjectUnderARootStillWorks`, `TestInitFindsARootAboveADeepProject` |
| M12 `workspaceRootUnder` checks the parent only (F6-2's bug) | `TestInitFindsARootAboveADeepProject` |
| M20 `bdrive init` drops the at-a-root refusal | `TestInitRefusesWorkspaceRoot` |
| M21 `bdrive init` drops the containing-a-root refusal | `TestInitRefusesAFolderContainingARoot` |
| M22 `notAProject` drops the workspace-root branch | `TestCommandsAtARootDoNotAdviseInit`, `TestShareFamilyAtARootDoesNotAdviseInit` |

**Manifest reads are index-only.** Every read in the tree:
`IsWorkspaceRoot` (returns a bool), `LoadWorkspace` (**zero non-test callers**,
re-confirmed by grep), `ScanWorkspace` (produces, never consumes),
`RefreshWorkspace`/`RefreshWorkspaceOf` (rewrite). No volume path, journal,
mount id, remote URL or permission is derived from an entry anywhere.
`w.Projects` is only ever *written* in production code.

**Every toucher of `.bdrive/config.json`, handed a root.** `projectConfigPath`
(config-internal only; `agenthooks` has an unrelated same-named helper for
agent config), `IsMount` (bare `os.Stat` → false at a root, no config.json
there), `LoadProject` (ENOENT → `(false, nil)`; and refuses `kind: workspace`
if the manifest is hand-planted in `config.json`), `SaveProject` (unreachable
at a root — `bdrive init` refuses first, and the connect writes at
`<root>/<name>`), `findProject`/`notAProject`/`mustProject` (report the root
instead of advising `bdrive init`). `SaveWorkspace`/`UndesignateWorkspace`
touch only `workspace.json`; `undo`'s `os.RemoveAll` targets `<target>/.bdrive`,
never the root's.

**Agent-hook guard, reproduced by hand with the real `/bin/sh`.** I extracted
the exact `mountGuard()` string from the Go source (352 bytes) and ran it with
`/bin/sh -c "$GUARD echo REACHED"` against a real layout, with a fake `bdrive`
on `PATH` that logs every invocation:

```
/tmp/r8ws/root                    falls-through=REACHED   (registry half: a mount below)
/tmp/r8ws/root/not-beardrive      falls-through=NO
/tmp/r8ws/root/not-beardrive/deep falls-through=NO
/tmp/r8ws/root/team               falls-through=REACHED
/tmp/r8ws/root/team/docs          falls-through=REACHED
/tmp/r8ws/plain                   falls-through=NO
spawn.log: (empty)
```

No `bdrive` spawned in the non-BearDrive siblings, and the guard itself never
spawns it anywhere (`command -v` only). Moving the manifest into `config.json`
flips *every* row to REACHED, which is the property
`TestHookGuardSkipsWorkspaceRoot` claims and I confirmed by hand. Budget intact:
one `grep`, no `find`/`awk`/`jq`, no process on the common path.

**CLAUDE.md invariants.** Nothing in this change set touches journals, blob
ordering, scan-before-pull, `journal.Less`/`Replay`, materialize's dirty check,
or the volume flock. `SaveWorkspace` goes through `config.writeJSON`, which is
temp-file (`.bdrive-tmp-*`) plus rename. The daemon change is a goroutine placed
after `announce`, touching neither `daemon.lock` nor the sync loop; its error is
logged, never returned — the "never break sync, retry next cycle" posture. The
hook guard invariant is verified above.

**`bdrive init` refuses before any network call or write.** Both refusals sit at
`init.go:196` and `:214`, ahead of the `--project/--name` check, ahead of
template resolution, and ahead of `ensureLogin`. `TestInitRefusesWorkspaceRoot`
and `TestInitRefusesAFolderContainingARoot` both bound the call and assert
nothing was written.

**gofmt.** All 19 touched `.go` files are clean (`gofmt -l` returns nothing for
them). The pre-existing offenders named in the brief are untouched by this
change set.

---

## Gate — run by me, not pasted

```
go build ./... && go vet ./...            -> BUILD_VET_OK (clean, no output)
go test ./... -count=1 -timeout 5400s     -> EXIT=0, 12/12 packages ok
```

```
ok  cmd/bdrive             196.608s     ok  internal/remote      17.404s
ok  internal/agenthooks     39.449s     ok  internal/secrets      4.506s
ok  internal/autostart      10.026s     ok  internal/store        6.190s
ok  internal/config          4.092s     ok  internal/syncer     111.321s
ok  internal/daemon         41.210s     ok  internal/templates    0.669s
ok  internal/journal        37.858s     ok  internal/webapp    2711.884s
```

`internal/webapp` took **2711 s (45 min)** on this run — the slowest of the
eight rounds (previous high 1953 s). Anyone re-running this needs the
`-timeout 5400s`; the default 600 s and even 1800 s die on the alarm, which the
goal file already records as having been misread once as a real failure.

Additionally, `-race` on the new tests (not part of the standard gate):
`internal/config` (all `Workspace|Root|Refresh|Designate|Undesignate|LoadProject|IsMount|Rescan|Nesting`)
and `internal/daemon` (`Workspace`) both **ok**, no data races — including
`TestRefreshDoesNotResurrectADeletedManifest`, the FIFO handshake round 6
(F6-5) had to rewrite.

**Confirmed I changed nothing but this report:** `git diff HEAD --stat` is
byte-identical before and after my pass (16 files, 609 insertions, 11
deletions); the only new entry in `git status --short` is
`.claude/workspace-root-validation-8.md`. All mutation work ran on a `tar`
copy at `/tmp/r8repo`. No `git stash` was used.

---

## Suspicions (unproven)

1. **`checkRootHere`'s syscall enumeration is now wrong, though I could not
   turn it into a hang.** Its doc says it makes "one stat of its own
   config.json, and path arithmetic", and that "every call it makes is on a
   path the caller named and has already touched" — the property F6-1's split
   exists to guarantee. Round 7's own `resolveExisting` broke the enumeration:
   `checkRootAllowed` now runs `EvalSymlinks` over **two** paths, which lstats
   every component of the folder *and* every component of `$BDRIVE_HOME` — a
   tree the caller never named. I could not make this block, and I believe it
   cannot in practice: the folder's ancestors must already be traversable for
   the `IsMount` stat to work, and the connect flow reads `settings.json` out
   of `$BDRIVE_HOME` (to get the token) long before it reaches
   `DesignateWorkspace`. So the *property* holds while the *stated reason* no
   longer does. Worth re-wording rather than re-engineering.
2. **`RefreshWorkspace` is not guarded by `checkRootAllowed`.** Creation is
   refused inside the beardrive home (`checkRootHere`), but refresh obeys any
   manifest already on disk. `RefreshWorkspaceOf` on a mount directly in
   `$HOME` calls `RefreshWorkspace($HOME)`, which reads
   `$HOME/.bdrive/workspace.json` — i.e. inside the default `$BDRIVE_HOME`. It
   is a no-op today because nothing can put a manifest there (all three writers
   refuse), so the only routes are a hand-planted file or a `BDRIVE_HOME` moved
   *after* a root was designated. I did not find a route the product itself
   takes, and the consequence (an index rewritten in the credential dir, plus a
   scan of the home's children in the harmless goroutine) is not a leak. Noted
   as an asymmetry, not claimed as a bug.
3. **`resolveExisting`/`underPath` fail open on a relative path.**
   `filepath.Rel(abs, rel)` errors, `underPath` returns false, and the refusal
   is skipped — the exact fail-open `config.Home()`'s comment was written to
   prevent. Unreachable today: `validateShared` requires `filepath.IsAbs(root)`
   before the connect can reach `DesignateWorkspace`, `Home()` runs
   `filepath.Abs`, and `InitWorkspace`/`CheckRootPlacement` have no production
   callers. It belongs in the same note as the documented `LoadWorkspace`
   validation obligation: the first caller that can pass a relative path owns it.
4. **`findProject` gained a read per ancestor.** `share`/`restore`/`forget`/
   `url` now call `config.IsWorkspaceRoot(cur)` at each level of the walk-up,
   which is an unbounded `os.ReadFile` of `<ancestor>/.bdrive/workspace.json`.
   It widens an existing hazard rather than creating one — the same loop already
   calls `LoadProject(cur)`, an unbounded read of the sibling file in the same
   directory — and it short-circuits once a root is found. These are foreground
   CLI commands a user can interrupt, which is the same standard round 7 applied
   to `CheckRootPlacement` in `bdrive init`. Not on any UI or daemon path
   (`grep` of `findProject` callers: `forget.go`, `share.go` ×3, `url.go`,
   `restore.go` — all cobra `RunE`s). Flagged only so it is a known widening.

---

## What I could not check

- **Windows and Linux.** Everything here ran on macOS/APFS. The case-folding
  branch in `underPath` is justified by NTFS too, and `resolveExisting`'s
  `Dir` fixed point differs on Windows volume roots (`C:\`) — untested, and
  `GOOS=windows go build ./...` does not pass at HEAD for unrelated reasons
  (CLAUDE.md).
- **A real wedged network mount or a TCC-gated folder.** All blocking tests use
  FIFOs, which model a blocking `open` but not a blocking `lstat`/`stat`. The
  one hazard class I could not exercise is the one suspicion 1 turns on: a
  `$BDRIVE_HOME` on a dead NFS/SMB mount, where `EvalSymlinks` would block
  uninterruptibly in the kernel. That needs the `sandbox/` container or a real
  dead mount.
- **The desktop shell.** I exercised `POST /api/desktop/init` through the Go
  tests only; no Tauri build, no real connect from the app UI.
- **`internal/webapp` in depth.** It has no workspace-root code (grep: zero
  hits for any `Workspace*` symbol), so I read the gate result rather than
  auditing it.
- **Whether the two documentation findings matter to anyone.** F8-3 and F8-4
  are contradictions I can demonstrate; whether the design doc or the code
  comment is the one that should change is a call for the author.
