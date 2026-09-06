# Workspace root — validation round 4

**Verdict: 7 findings.** Two HIGH (both self-inflicted by round 3, both hang a
user-facing flow forever with no message), one MEDIUM (round 3's V3-5 fix does
not fix V3-5 — the bug reproduces unchanged), four LOW.

The pattern holds for a third round running: **three of the seven findings were
introduced by round 3's own fixes**, and two of those (F1, F2) are worse than
what they closed — F2 replaces a wrong-but-finite outcome with an infinite
hang. F3 is a fix that changed nothing.

Gate run here (not trusted from the report):

```
go build ./...                       OK
go vet ./...                         OK
go test ./... -count=1 -timeout 5400s   12/12 ok   (webapp 1774.583s)
gofmt -l <19 touched files>          clean
```

`webapp` took 1774s on this machine vs the 1211s reported — machine-load
variance, as the goal file already notes. Nothing failed.

---

## Findings

### F1 — HIGH — `bdrive init` hangs forever, silently, on a folder with one unreadable child

*File:* `cmd/bdrive/helpers.go:107-116` (`workspaceRootUnder`), reached
unconditionally from `cmd/bdrive/init.go:214`.

Round 3's V3-6 fix added an `os.ReadDir(folder)` plus a `config.IsWorkspaceRoot(child)`
for **every immediate child directory**. `IsWorkspaceRoot` is an `os.ReadFile`
of `<child>/.bdrive/workspace.json`. It is unbounded, it is not wrapped in
`probe`, and it runs on **every** `bdrive init` — including the resume path,
which is the documented way back after `bdrive stop`.

This is the identical hazard round 3 raised as V3-3 and closed by deleting the
same kind of scan from `startSync`; the scan simply moved one function over,
onto the same command. `desktop/DESIGN.md` now asserts the opposite in writing:

> For the same reason `startSync` does not refresh at all: an unbounded scan
> there hung `bdrive init` and the desktop connect's sync step.

`bdrive init` still has one.

**Failure scenario.** A user runs `bdrive init ~/work`. `~/work` contains a
child sitting on a stale SMB/NFS mount (or, per the repo's own model in
`fsProbeTimeout`, a TCC-gated path). The `open` blocks in the kernel. `bdrive
init` prints **nothing at all** and never returns — the guard runs before
`ensureLogin`, before the resume check, before any output.

**How verified.** Real binary (`go build -o /tmp/bdrive-v4 ./cmd/bdrive`),
isolated `$BDRIVE_HOME`/`$HOME`, empty mount registry. A new folder with two
children, one of them holding a FIFO at `.bdrive/workspace.json` — the same
FIFO proxy `internal/daemon/workspace_test.go` and
`cmd/bdrive/workspace_test.go` use for this exact class:

```
===== bdrive init on a folder with ONE wedged child =====
>>> RESULT: HUNG (killed after 30s)          # zero bytes of output

===== control: same folder with the FIFO removed =====
no interactive terminal detected — using the device-code sign-in flow
to finish signing in, open this link in any browser: ...
```

The registry was empty, so `mountsUnder` returned nothing: the only new code
that touched a child was the `ReadDir` loop.

**Cost, separately measured** (warm local SSD, `workspaceRootUnder` called
directly): 100 children 3.1 ms, 1 000 children 11.7 ms, 20 000 children
827 ms. Throughput is survivable; the unbounded wait is not. Round 2's V1
called 4 s of sibling scanning a problem inside the daemon's 10 s window —
this sits on a command with no window at all.

The rest of the V3-1 matrix is **clean** — verified with the real binary
against a hand-built root + registry:

| scenario | result |
|---|---|
| `init` in a project under a root (resume) | resumes, daemon starts ✅ |
| `init` at a project NOT under a root | resumes ✅ |
| `init` at a subfolder of a project | refused, for the pre-existing nested-project reason ✅ |
| `init` at the root | refused, names the root ✅ |
| `init` above an **enrolled** root | refused, names the root ✅ |
| `init` above an **unenrolled** root | refused, names the root ✅ |
| registry `/private/tmp/...`, user types `/tmp/...` | resumes ✅ |
| registry `/tmp/...`, user types `/private/tmp/...` | resumes ✅ |

No false positive found other than the hang.

---

### F2 — HIGH — the desktop connect hangs at "connecting" forever: round 3 hoisted one syscall out of the bound

*File:* `cmd/bdrive/desktop_onboard.go:488`.

```go
root := filepath.Dir(target)
alreadyRoot := config.IsWorkspaceRoot(root)   // <-- NOT inside probe
createdRoot = !alreadyRoot
_, _ = probe(root, func() (struct{}, error) { ... })
```

V3-5's fix moved the `IsWorkspaceRoot` decision *out of* the probed closure so
`createdRoot` could be decided up front. `IsWorkspaceRoot` is an `os.ReadFile`.
It is now the one filesystem call on a user-named folder in this file that is
**not** bounded — directly beneath a comment (lines 481-487) that says:

> Bounded like every other syscall on a user-chosen path here: the scan reads
> the root's directory and one config per child, and a child on a gated or
> wedged path blocks rather than failing — a connect stuck forever at
> "connecting" is the one outcome this flow must never have.

That is precisely the outcome. `fsProbeTimeout`'s own doc comment records the
2026-08-21 bug this rule exists for.

**Failure scenario.** The root's `.bdrive/workspace.json` is unreadable in the
blocking sense (wedged network path, TCC gate that resolves on the manifest but
not the target). The connect screen sits at "connecting" with no error, no
timeout and no undo, for the life of the sidecar.

**How verified.** Go test in a /tmp copy of the tree, unmodified branch code,
using the shipped `onboardHub`/`onboardEnv`/`postInit` harness. FIFO at
`<root>/.bdrive/workspace.json`, plain readable `<root>/team` as the target so
the opening `probe(target, os.Stat)` passes:

```
--- FAIL: TestV4DesignationHasOneUnboundedSyscall (25.15s)
    the connect never left phase connecting in 25s
```

25 s is ~16× the 1.5 s `fsProbeTimeout`.

---

### F3 — MEDIUM — V3-5 is **not fixed**: a failed connect still converts the user's folder into a workspace root

*File:* `cmd/bdrive/desktop_onboard.go:443-444` (undo) and `495-501`
(designation).

Round 3 moved `createdRoot` before the probed call and justified it:

> Removing a manifest that was never written is a no-op, so deciding up front
> is safe in the direction that matters.

The no-op **is** the problem. `probe` abandons but cannot cancel. Sequence:

1. `alreadyRoot=false`, `createdRoot=true`.
2. `probe(InitWorkspace)` times out at 1.5 s; the goroutine is still scanning.
3. The connect then fails → `undo` → `os.Remove(<root>/.bdrive/workspace.json)`
   → **ENOENT, no-op** (nothing written yet) → `os.Remove(<root>/.bdrive)`.
4. The abandoned goroutine finishes and calls `SaveWorkspace`, which
   `MkdirAll`s `.bdrive` again and writes the manifest.

Net result is byte-for-byte V3-5's, and F4's before it: the connect screen says
the connect failed, and the folder is permanently a workspace root. Nothing
un-designates one — there is no CLI for it, and `bdrive init` refuses a root.

**How verified.** Go test in the /tmp copy against unmodified branch code —
FIFO decoy under the root, a writer opening it at t=4 s (the user clicking
Allow a moment after the deadline, the scenario `fsProbeTimeout` exists for),
and the same template-seed failure injection the two shipped failure tests use:

```
    immediately after undo: IsWorkspaceRoot=false
--- FAIL: TestV4ProbeTimeoutStillLeavesALateRoot (4.06s)
    a FAILED connect converted <root> into a workspace root ... after undo ran
```

The two shipped tests (`TestDesktopInitFailureUnroots`,
`...KeepsAPreExistingRoot`) both cover only the **fast** path — confirmed
load-bearing by mutation, but neither exercises the timeout, which is the whole
of what V3-5 was about. The round protocol's own rule ("a fix to a
blocking/ordering problem needs its own failing test before it is believed")
was not applied to its own fix.

---

### F4 — LOW — the immediate-children check misses a **symlinked** child, so DESIGN.md's claim about it is not true

*File:* `cmd/bdrive/helpers.go:109` — `if !e.IsDir() || ...  { continue }`.

`os.ReadDir` reports a symlink's **own** type, so `e.IsDir()` is false for a
symlink to a directory. Round 1's F8 found and fixed exactly this in
`ScanWorkspace` (`internal/config/workspace.go:126-133` stats through the
link, with a comment explaining why). `workspaceRootUnder` was written without
it.

DESIGN.md states the coverage unconditionally:

> The second looks in two places: the mount registry ... and the folder's
> immediate children, which catches a root this device never enrolled (a tree
> copied from another machine). **Known gap:** an unenrolled root more than one
> level down is not seen.

A symlinked immediate child is one level down and is not seen either.

**Failure scenario.** `~/Projects` is a symlink to a root on an external
volume. `bdrive init ~` is not refused, and every folder that root exists to
hold apart syncs to the team.

**How verified.** Go test in the /tmp copy, empty registry, root reached only
by symlink: `IsWorkspaceRoot(link)` is true, `workspaceRootUnder(outer)`
returns `("", false)`.

---

### F5 — LOW — `InitWorkspace`'s `$HOME` refusal is equality-only and fails open when the home does not exist yet

*File:* `internal/config/workspace.go:187` —
`samePath(filepath.Join(folder, ProjectDir), home)`.

`samePath` compares raw strings, then `EvalSymlinks` on both — and
`EvalSymlinks` **fails on a path that does not exist**, so the guard falls open.
Two gaps, both verified by Go test in the /tmp copy:

| case | result |
|---|---|
| custom `$BDRIVE_HOME` elsewhere, `InitWorkspace($HOME)` | allowed — **correct** ✅ |
| symlinked `$HOME` spelling, home directory **exists** | refused — **correct** ✅ |
| symlinked `$HOME` spelling, home **not yet created** | **allowed; the manifest is written into `$BDRIVE_HOME`** |
| `InitWorkspace($BDRIVE_HOME)` and `InitWorkspace($BDRIVE_HOME/sub)` | **both allowed** (`<folder>/.bdrive != home`, so the equality never fires) |

Not reachable from today's only caller: `runDesktopInit` takes
`root = filepath.Dir(target)`, and `handleDesktopInit` already refuses a target
that contains or is under the home, which makes `root == home` impossible. But
DESIGN.md nominates `config.InitWorkspace` as "the seam" for any future
designation command and asserts the property flatly:

> A root is never `$HOME`, because `$HOME/.bdrive` IS the beardrive home —
> `InitWorkspace` refuses rather than writing an index beside the device token.

It refuses one spelling of one shape.

---

### F6 — LOW — "Deleting the manifest un-roots the folder. Nothing recreates it" is false under a slow scan

*File:* `internal/daemon/daemon.go:410-414` → `config.RefreshWorkspace`
(`workspace.go:157-165`).

`RefreshWorkspace` checks `IsWorkspaceRoot`, then scans, then writes. The scan
is unbounded, so with a wedged sibling the check-to-write window is arbitrarily
long, and `SaveWorkspace` `MkdirAll`s the directory back. Three documentation
surfaces state the opposite — `README.md`, `web/docs/.../project-files.md`, and
DESIGN.md §Rules — and it is the **only** documented way to un-root a folder.

**Failure scenario.** A daemon starts while a sibling is wedged. Minutes later
the user deletes `workspace.json` to un-root the folder. The wedge clears; the
manifest reappears; `bdrive init` there still refuses.

**How verified.** Go test in the /tmp copy, real `daemon.Run`, FIFO sibling,
manifest deleted 500 ms after start, FIFO writer released after:

```
--- FAIL: TestV4DeletedManifestComesBack (0.69s)
    the manifest the user deleted came back: <root>/.bdrive/workspace.json
```

---

### F7 — LOW — DESIGN.md §Status cites a test this round deleted

*File:* `desktop/DESIGN.md:181`.

The Status block still lists `TestWorkspaceRefreshOnSyncStart` as a check.
Round 3 deleted it (V3-3). It exists nowhere in the tree — the only other hits
are in round 2's report. Its replacement
(`TestSyncStartNeverScansTheWorkspaceRoot`) and the daemon's
`TestWorkspaceRefreshNeverStallsTheDaemon` are both missing from the list. The
goal file's matrix row 8 *was* updated; DESIGN.md was not.

Same class as round 2's V5, which was DESIGN.md citing a wrong test as its
proof. Third round in which the design doc's own citations are stale.

---

## Round-3 findings — confirmed or refuted

| # | Verdict |
|---|---|
| **V3-1** — `mountsUnder` includes the folder itself | **CONFIRMED FIXED.** Real binary, 8-scenario matrix: init works in a project under a root, at a project outside any root, and under both symlink spellings; refusals still fire at a root, above an enrolled root and above an unenrolled one. Mutation: deleting the `resolvePath(m) == self` skip fails `TestInitInAProjectUnderARootStillWorks`. **But the same fix's other half introduced F1 (hang) and F4 (symlink blind spot).** |
| **V3-2** — refresh in a goroutine | **CONFIRMED FIXED, and the fix holds under attack.** Mutation: inlining the call fails `TestWorkspaceRefreshNeverStallsTheDaemon` (15 s timeout). Attacked as briefed: the goroutine dies with the process, so nothing writes after exit; a manifest written while `.bdrive` is being removed correctly *drops* the entry (`ScanWorkspace` uses `LoadProject`, not the registry); two daemons under one root are benign — `writeJSON` puts its temp file in `<root>/.bdrive/`, which is never inside a mount (`InitWorkspace` refuses a root inside a project) and carries the ignored `.bdrive-tmp-` prefix anyway, and the rename is atomic; a real two-daemon run left one 0600 `workspace.json` and no strays. The flock invariant is intact — `announce` still precedes the goroutine. Only residue: F6. |
| **V3-3** — refresh deleted from `startSync` | **CONFIRMED FIXED, no orphaned path.** Mutation: restoring the call fails `TestSyncStartNeverScansTheWorkspaceRoot` (30 s hang). Real binary: `bdrive init` on the first project under an existing root indexes it within ~3 s, via the daemon it spawns; a project added while a daemon is already running appears at the next daemon start. Nothing is left wrong indefinitely as long as a daemon eventually starts, which is what DESIGN.md's Migration section claims. **The deleted `TestWorkspaceRefreshOnSyncStart` was asserting behaviour that has correctly gone away — no real assertion was lost — and its replacement is load-bearing.** |
| **V3-4** — `IsMount` reverted to a stat | **CONFIRMED FIXED, both directions measured.** Walked all 8 non-test `IsMount` call sites with a real root: correct at every one (a real root has no `config.json`, so it is structurally not a mount). Nothing round 1's F3/row 3 closed is reopened; only the hand-planted collided layout reads as a mount, which DESIGN.md now states. Speed: a real syncer cycle over a mount containing a wedged nested `.bdrive/config.json` finishes in **21 ms**; with `IsMount` reading the file it **hangs past 30 s**. `TestIsMountFalseAtWorkspaceRoot` is the regression guard — it fails under that mutation. |
| **V3-5** — probe timeout re-opens F4 | **REFUTED — the bug is unchanged.** See F3. The reorder fixed the bookkeeping and not the ordering; the manifest still lands after undo. The reorder additionally created F2. |
| **V3-6** — refusal was registry-only | **CONFIRMED FIXED for plain directories** (real binary refuses above an unenrolled root with an empty registry; mutation removing the child loop fails `TestInitRefusesAFolderContainingAnUnenrolledRoot` while the enrolled-path test still passes, so the two mechanisms are separately covered). **Incomplete for symlinked children (F4), and it is the source of F1.** |
| **V3-7** — `$HOME` as a connect root | **CONFIRMED FIXED for the shipping shape** (mutation removing the guard fails `TestWorkspaceRootIsNeverTheBdriveHome`; the symlinked spelling is refused when the home exists; a custom `$BDRIVE_HOME` correctly leaves `$HOME` designatable). **Narrower than documented — see F5.** |
| **V3-8** — stale comment in `sync_run.go` | **CONFIRMED FIXED.** The call is gone; the replacement comment (lines 33-39) describes the absence and the one real consequence accurately. |

## Test judgement (by mutation, not by reading)

Every test this round was mutation-tested. All are load-bearing:

| test | mutation | result |
|---|---|---|
| `TestSyncStartNeverScansTheWorkspaceRoot` | restore `RefreshWorkspaceOf` in `startSync` | FAIL (30 s) ✅ |
| `TestWorkspaceRefreshNeverStallsTheDaemon` | inline the daemon refresh | FAIL (15 s) ✅ |
| `TestInitInAProjectUnderARootStillWorks` | drop the `== self` skip | FAIL ✅ |
| `TestInitRefusesAFolderContainingAnUnenrolledRoot` | drop the child scan | FAIL ✅ |
| `TestInitRefusesAFolderContainingARoot` | drop the child scan | PASS (registry path — distinct mechanism, correctly) ✅ |
| `TestWorkspaceRootIsNeverTheBdriveHome` | drop the `$HOME` refusal | FAIL ✅ |
| `TestIsMountFalseAtWorkspaceRoot` | `IsMount` reads the file | FAIL ✅ |
| `TestDesktopInitFounder` | remove the designation | FAIL ✅ |
| `TestDesktopInitFailureUnroots` | `createdRoot = false` (round-2 shape) | FAIL ✅ |

**No test passes for a reason unrelated to its feature. No existing test was
weakened.** `TestDesktopInitFounder`'s changed assertion is a genuine
re-expression, not a dilution: `os.Stat(<root>/.bdrive)` must-not-exist became
*not a mount* + *no `config.json`* + *nothing in `.bdrive` but the manifest* +
*is a root* + *manifest holds exactly the one project* + *no `.bdriveignore` at
the root* — strictly more.

**One gap in coverage, not in the code:** nothing tests the probe-timeout path
of the designation, which is why F3 shipped as a fix that fixes nothing.

## Other checks (all clean)

- **Every manifest read is index-only.** `LoadWorkspace` still has **zero**
  non-test callers. The only non-test readers are `IsWorkspaceRoot` (reads
  `kind` and nothing else) and `ScanWorkspace`→`SaveWorkspace` (write from a
  scan). No volume path, journal, mount id or permission is resolved from the
  manifest anywhere.
- **Every toucher of `.bdrive/config.json`** — `projectConfigPath`,
  `ProjectDir`, `IsMount` (8 call sites), `LoadProject`, `SaveProject` — is
  correct at a real root, because a real root has no `config.json`.
  `LoadProject` refuses the collided layout by `kind`.
- **Agent-hook guard, reproduced by hand with a real `/bin/sh`** under a root,
  with a fake `PATH` logging every spawn:

  | folder | fires | greps | other spawns |
  |---|---|---|---|
  | `R/non-beardrive-1` | no | 1 | 0 |
  | `R/non-beardrive-2/src/deep` | no | 1 | 0 |
  | `R/team` (mount root) | yes | 0 | 0 |
  | `R/team/docs/deep` | yes | 0 | 0 |
  | `R` (the root itself) | yes | 1 | 0 |
  | unrelated folder | no | 1 | 0 |

  No `bdrive` spawn outside a project, ≤1 `grep`, pure shell. Invariant intact.
- **CLAUDE.md invariants.** Own-journal-only, blobs-before-journal,
  scan-before-pull, `Less`/`Replay`, dirty-file protection, `Cycle` under the
  volume flock: untouched by this change set. Atomic writes: `SaveWorkspace`
  goes through `writeJSON` (temp + rename, `.bdrive-tmp-` prefix). "A daemon's
  liveness is its flock": intact — `announce` precedes the refresh goroutine,
  which was V3-2's whole point and is confirmed by mutation. "Never break sync,
  retry next cycle": the daemon's refresh degrades to a log line and never
  fails a cycle — **but F1 and F2 hang the two commands that start syncing**,
  which is the same failure one step earlier.
- `gofmt -l` clean on all 19 touched files.

## Suspicions (unproven)

1. **`findProject` now does an `IsWorkspaceRoot` read per ancestor level** on
   the `share`/`restore`/`forget`/`url` path (`share.go:250`). Bounded by path
   depth so it is not the F1 class, but each read is still an unbounded `open`;
   a wedged ancestor would hang those four commands. Did not construct it.
2. **`InitWorkspace`'s nesting walk climbs to `/`**, doing two reads per level
   (`IsWorkspaceRoot` + `IsMount`) on the connect path, inside `probe` — so
   bounded, but a deep target burns the 1.5 s budget on ancestors before ever
   reaching the scan. Not measured.
3. **`undo`'s `os.Remove(<root>/.bdrive)`** will delete a `.bdrive` directory
   the *user* had at their root when `createdRoot` is true and the directory
   happens to be empty. Harmless today (nothing else puts an empty `.bdrive`
   at a non-mount) but it is unwinding something this run did not create.
4. **The error F1's guard produces has no remedy.** When a stale root is
   legitimately in the way, `bdrive init` says "sync the project folders under
   <root> instead" and there is no command to un-designate. DESIGN.md's "Not
   done" acknowledges the missing command; the message does not mention the
   manual `rm`.

## What I could not check

- **Windows.** `internal/autostart`'s Windows tests have still never executed,
  and `GOOS=windows go build ./...` does not pass at HEAD for reasons that
  predate this change set. All my filesystem reasoning is POSIX.
- **Real TCC gating.** Every blocking case was proved with a FIFO, the proxy
  this repo's own tests use. I did not reproduce an actual macOS privacy prompt
  or a wedged NFS/SMB mount; the claim that those block rather than fail comes
  from `fsProbeTimeout`'s doc comment and DESIGN.md, not from my own
  measurement.
- **The Mac app itself.** I exercised the sidecar's `/api/desktop/init` HTTP
  surface, never the Tauri shell, so what the user actually sees during F2's
  hang (spinner, timeout, retry affordance) is inferred from the `phase` field.
- **Concurrency at scale.** Two daemons under one root were run and left clean
  state, but I did not stress `SaveWorkspace` with many concurrent writers.
- **`internal/webapp`'s 1774 s** — I confirmed it passes but did not attribute
  the runtime or check for anything workspace-related inside it (the change set
  does not touch that package).

---

*Scratch artifacts for this round live in `/tmp` (`/tmp/wsval4` is an rsync
copy of the worktree used for mutations and probe tests; `/tmp/v4*.sh` are the
real-binary labs). Nothing in the worktree was modified except this file.*
