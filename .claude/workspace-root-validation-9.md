# Workspace-root validation — round 9

**VERDICT: 3 findings** (1 medium, 2 low).

All three are the *same defect class rounds 1–6 found five times* — an unbounded,
blocking filesystem read where blocking is not affordable — but in the one place
eight rounds never looked: **the file `IsWorkspaceRoot` itself opens.** Every
previous round attacked scans of *children* and walks over *ancestors*. Nobody
checked the manifest at the folder the caller named. `IsWorkspaceRoot` is
`os.ReadFile`, not `os.Stat`, and `DesignateWorkspace` — the desktop connect's
designation step, which three documents and one test name as unable to wedge —
calls it first thing.

Nothing else broke. Round 8's four fixes are all confirmed; two were
mutation-tested and both are load-bearing. Concurrency, the manifest as
untrusted input, the CLI's interaction with roots, and the agent-hook guard
under 14 hostile root names all held.

Worktree `/Users/snow/workspace/runbear/wt-desktop-app`, branch `desktop-app`,
HEAD `1672a13` + uncommitted work. Mutation sandbox `/tmp/r9repo` (tar copy — no
`git stash`). Nothing in the worktree was edited but this file.

**GATE, run by me end to end:** `go build ./...` OK, `go vet ./...` OK,
`go test ./... -count=1 -timeout 5400s` → **12/12 ok, EXIT=0** (webapp 2971 s,
cmd/bdrive 269 s, syncer 140 s). `gofmt -l` over the 19 touched files: **clean**.

---

## Findings

### F9-1 — `DesignateWorkspace` blocks forever on the desktop connect path; the code, DESIGN.md and a test all assert it cannot

- **Severity: medium.** A permanent UI wedge with no cancel, no undo and a 409
  on every retry — the exact failure mode the whole scan-free design exists to
  prevent. Needs an exotic file to trigger, which is true of all five prior
  instances of this class.
- **Files:** `internal/config/workspace.go:75-77` (`IsWorkspaceRoot`),
  reached from `internal/config/workspace.go:340` (`DesignateWorkspace`),
  called at `cmd/bdrive/desktop_onboard.go:490`.

```go
func IsWorkspaceRoot(folder string) bool {
	data, err := os.ReadFile(workspaceConfigPath(folder))   // <- opens and reads. Unbounded.
	return err == nil && configKind(data) == WorkspaceKind
}
```

```go
func DesignateWorkspace(root string, p WorkspaceProject) (bool, error) {
	if IsWorkspaceRoot(root) {          // <- the blocking read, before anything else
		return false, nil
	}
	if err := checkRootHere(root); err != nil { ... }
```

- **Failure scenario, reproduced:** `<root>/.bdrive/workspace.json` is a FIFO
  (equivalently: a device node, a symlink to one, or a regular file on a stalled
  network mount — the project's own stated hazard set, quoted in
  `ScanWorkspace`'s comment). `open()` blocks in the kernel. `os.Stat` would
  not; `os.ReadFile` does.

  In the connect flow this is terminal. `handleDesktopInit` sets
  `onboarding.running = true` and launches a **bare `go runDesktopInit(...)`**
  (`desktop_onboard.go:397`) — no watchdog, no context. `runDesktopInit` wedges
  inside `DesignateWorkspace`, so `onboarding.fail` never runs, `running` is
  never cleared, and every subsequent connect gets
  `409 "a folder is already being connected"` (`desktop_onboard.go:385`) for the
  life of the sidecar process. The UI sits at "connecting" forever.

  Nothing earlier in the flow touches `<root>/.bdrive/` at all: `probe` stats
  **`target`** (`<root>/<name>`), `MkdirAll` creates **`target`**, `SaveProject`
  writes **`target/.bdrive`**. So "already proven reachable" covers the root
  *directory* and says nothing about the file that is opened.

- **How verified:** `/tmp/r9repo/internal/config/zz_r9_fifo_test.go`, 4-second
  watchdog on a goroutine, against the shipped code:

  ```
  --- FAIL: TestR9_IsWorkspaceRootOnFifo (4.14s)
      BLOCKED: IsWorkspaceRoot(root with a FIFO manifest) never returns
  --- FAIL: TestR9_DesignateWorkspaceOnFifoManifest (4.06s)
      BLOCKED: DesignateWorkspace hangs on a FIFO manifest at the root it was handed
  ```

  And the same read is unbounded in size — a 100 MB manifest is read into memory
  in full, **5.4 s per call**, on every one of the amplification sites below:
  `TestR9_IsWorkspaceRootHugeManifest` → `IsWorkspaceRoot = true in 5.408567291s`.

- **Amplification sites** (every one is an `os.ReadFile` of a caller-named path):
  `DesignateWorkspace` (desktop connect), `cmd/bdrive/init.go:199`
  (`bdrive init` at the named folder), `helpers.go:149` (`notAProject`, on the
  error path of `sync`/`log`/`grep`/`stale`/`scope`/`forget`/`share`/`url`/`stop`),
  `helpers.go:134` (per intermediate directory — F9-2), `share.go:250` (per
  ancestor to `/`), `CheckRootPlacement` (per ancestor; dormant), and
  `RefreshWorkspace` ×2 (harmless — goroutine).

- **Three shipped claims this contradicts, verbatim:**
  1. `internal/config/workspace.go:337` — *"this adds **one stat** and one atomic
     write."* It adds an `os.ReadFile`, then a stat, then `resolveExisting`'s
     lstats, then `MkdirAll` + `CreateTemp` + write + `Rename`.
  2. `cmd/bdrive/desktop_onboard.go:475-478` — *"Scan-free
     (config.DesignateWorkspace): **one stat** and one atomic write … so it needs
     no probe and **cannot wedge the connect**."*
  3. `desktop/DESIGN.md` §Migration — *"`DesignateWorkspace` writes a manifest
     holding the one project it just created — **one stat, one atomic write**,
     over a directory the flow has already proven reachable."*

  `checkRootHere`'s own comment (*"It never OPENS a file"*) is **true of
  `checkRootHere`** — the false step is the `IsWorkspaceRoot` call that runs
  *before* it.

- **Why eight rounds missed it:** the two tests that guard this function both
  aim one level away. `TestDesignateWorkspaceIsScanFree` FIFOs a **child**
  (`<root>/wedged/.bdrive/config.json`). `TestDesignateWorkspaceNeverReadsAnAncestor`
  FIFOs an **ancestor** (`<base>/.bdrive/{workspace.json,config.json}`). Neither
  touches `<root>/.bdrive/workspace.json`. Confirmed by grepping every `Mkfifo`
  in `internal/config` and `cmd/bdrive` (5 sites, none at the designation root).
  `TestDesignateWorkspaceIsScanFree`'s comment even blesses the read — *"it must
  read nothing but the manifest it is about to write"* — which is precisely the
  read that blocks.

- **Fix shape (not applied):** `IsWorkspaceRoot` stats first and only reads a
  regular file, and/or reads a bounded prefix (`kind` is all it needs). That
  fixes every amplification site at once, which is where the root cause lives.

### F9-2 — `workspaceRootUnder` hangs `bdrive init`; its doc says "one stat per registered mount"

- **Severity: low.** Same class, on a CLI where Ctrl-C works, over directories
  inside the user's own tree.
- **File:** `cmd/bdrive/helpers.go:97` (doc) and `:134` (the call).

```go
// … so this is one stat per registered mount under folder — no walk down the user's disk.
…
		for cur := filepath.Dir(resolvePath(m)); resolvePath(cur) != self; cur = filepath.Dir(cur) {
			if config.IsWorkspaceRoot(cur) {   // <- os.ReadFile, once per INTERMEDIATE directory
```

- Two things the summary line gets wrong: it is a **read**, not a stat, and it is
  **one per intermediate directory**, not one per mount. (The loop comment
  eleven lines below correctly describes the per-level walk; the header does not.)
- **Failure scenario, reproduced:** layout `<folder>/a/b/team` with `team`
  enrolled, and a FIFO at `<folder>/a/b/.bdrive/workspace.json`.
  `bdrive init <folder>` never returns.
- **How verified:** `/tmp/r9repo/cmd/bdrive/zz_r9_wru_test.go` →
  `BLOCKED: workspaceRootUnder hangs on a FIFO in an intermediate directory — bdrive init never returns` (6 s watchdog).
- **Why the existing test does not catch it:** `TestInitNeverScansForRoots`
  (`cmd/bdrive/workspace_test.go:406`) plants its FIFOs in **children of the
  named folder** — the set a *scan* would reach. The intermediate directories
  between the folder and an enrolled mount are a different set, and the test's
  own stated purpose covers them: *"A guard that can hang the command it guards
  is worse than the gap it closes."*
- The common shipping layout is unaffected: for a mount at `<root>/team` and
  `init` at `<root>`, `filepath.Dir(resolvePath(m)) == self` on entry, so the
  loop body never executes and **zero** files are read. Only deeper nesting pays.

### F9-3 — a refresh racing a root *deletion* recreates the root directory, not just the manifest

- **Severity: low.** Benign in effect (a ghost folder holding only
  `.bdrive/workspace.json`), but it is a documented behavior stated one notch
  too small, and it re-roots a folder the user deleted.
- **File:** `internal/config/workspace.go:105` (`SaveWorkspace`'s
  `os.MkdirAll(filepath.Join(root, ProjectDir))`), reached from
  `RefreshWorkspace` after its post-scan re-check.
- **Failure scenario, reproduced:** the user deletes the whole root folder while
  a daemon start's refresh goroutine is in flight. The delete lands after the
  second `IsWorkspaceRoot` check, so `MkdirAll` **recreates `<root>` and
  `<root>/.bdrive`** and writes a manifest indexing projects that no longer
  exist. The folder is a workspace root again, so `bdrive init` there refuses
  with *"is a BearDrive workspace root, not a project"*.
- **How verified:** `/tmp/r9repo/internal/config/zz_r9_daemonfail_test.go`,
  300 trials of `RemoveAll(root)` racing `RefreshWorkspace(root)`:
  `root directory recreated after RemoveAll: 9/300 (of which 9 recreated ONLY .bdrive, losing the projects)`.
- **What the docs say:** `web/docs/.../project-files.md` — *"a re-index that is
  already running can **write the file back** one last time"* — and
  `RefreshWorkspace`'s comment — *"the user can delete the manifest to un-root
  the folder … a delete landing between this check and the write still loses."*
  Both describe deleting **the file**. Deleting **the folder** and getting a
  skeleton root back is not covered.
- The narrower, documented case behaves exactly as documented: deleting only the
  manifest while a refresh runs resurrects it **22/200** times
  (`TestR9_UndesignateRacesRefresh`), and `bdrive stop` first is the stated cure.

---

## Round-8 findings — confirmed or refuted

| # | Verdict | Evidence |
|---|---------|----------|
| **F8-1** — `workspaceRootUnder`'s F7-5 fix pinned by no test | **FIXED, and the new test is load-bearing** | `TestWorkspaceRootUnderWalksTheResolvedMountPath` (`cmd/bdrive/workspace_test.go:489`). Mutation **M-A**: revert `filepath.Dir(resolvePath(m))` → `filepath.Dir(m)` (round 7's exact pre-fix line) ⇒ **FAIL**, with round 8's exact symptom: `workspaceRootUnder(<base>/a/b) = <base>` — a root ABOVE the folder reported as beneath it. Also re-verified end-to-end through the real binary (case E of `/tmp/r9cli3.sh`): `bdrive init` at the inner folder is **not** refused with a root that is above it. |
| **F8-2** — the F7-4 assertion was still vacuous | **FIXED, and it now discriminates the right rule** | Mutation **M-B**: delete the `IsWorkspaceRoot(cur)` "roots do not nest" branch from `CheckRootPlacement` ⇒ `TestDesignateWorkspaceNeverReadsAnAncestor` **FAILS**, printing the wrong-rule refusal round 8 diagnosed (`… is inside the project at <base>`) — so the `strings.Contains(err, "nest")` assertion is what catches it. `inside` is now created (`:757-760`). The dead `plain :=` setup is gone from this stanza (the one remaining `plain :=` at `:279` is a different test and is used). |
| **F8-3** — DESIGN.md / project-files.md stated rules the code does not implement | **FIXED — all four claims, each re-derived from code rather than trusted** | (1) §Rules bullet 2 now reads *"Enforced by no shipped code today"*: grep confirms `CheckRootPlacement`'s only caller is `InitWorkspace`, whose only callers are `*_test.go` (plus one comment reference at `helpers.go:128`). (2) §Rules bullet 3 — **the bullet the stalled attempt caught still false** — now reads *"it lives in `.bdrive`, which IS in `ReservedDirs`, at any depth"*: **true**, `ReservedDirs = {".git", ProjectDir}` with `ProjectDir = ".bdrive"` (`project.go:21,34`), applied per directory at every level by `syncer/walk.go:54`. **Proved by construction**, not by reading: I built a workspace root two levels inside a real mount and ran a two-device sync — the peer received `top.md` and `work/Projects/readme.md` and **not** `work/Projects/.bdrive/` (`/tmp/r9repo/internal/syncer/zz_r9_rootinmount_test.go`, PASS). The bullet also now admits nothing shipped prevents that layout, matching the code. (3) The closing paragraph now says *"Both sides are canonicalised as far as they exist (`resolveExisting`)"* — matches `checkRootAllowed`. (4) `project-files.md` now says roots *"are not meant to nest either, but connecting two folders where one is inside the other will produce that — harmlessly"*, matching `workspace_test.go:641-649`, which **requires** that nesting to succeed. |
| **F8-4** — `checkRootAllowed`'s comments described the pre-round-7 mechanism | **Fixed where it was wrong; one imprecise clause remains (not a finding)** | The doc comment no longer attributes the missing-home case to "comparing unresolved paths" — it now names `resolveExisting` and why `EvalSymlinks` alone fails. The body comment still says *"resolve the FOLDER — which does exist — and rebuild the path under it"*, which is accurate for the dominant case (`.bdrive` missing, folder present) and imprecise when the folder is missing too. I tested that case rather than arguing it: `checkRootAllowed` with the folder **and** several ancestors missing, through a symlinked ancestor, still refuses `$HOME` correctly (directly, via the symlink, and upper-cased) and still allows unrelated paths (`/tmp/r9repo/internal/config/zz_r9_missingfolder_test.go`). Two of my six expectations were wrong, not the code: a folder *deep inside* `$HOME`, and a folder that *contains* `$BDRIVE_HOME`'s parent, are both correctly allowed — the rule is `<folder>/.bdrive`-vs-home containment, exactly as the doc states, not folder-vs-home. |

---

## What I attacked and could not break

**Concurrency**
- 6 concurrent `RefreshWorkspace` + 4 concurrent `LoadWorkspace` readers + a
  concurrent `DesignateWorkspace`, on one root with 8 projects (40/200/30
  iterations): **`-race` clean, no torn read, no partial manifest**, final index
  correct. `writeJSON`'s `CreateTemp`+`Rename` gives each writer a unique temp
  name, so contention degrades to last-writer-wins over identical content.
- **Two real daemons under one root** (`daemon.Run` × 2, both firing their
  refresh goroutine at the same manifest), with the project set churning
  underneath for 20 rounds: `-race` clean, manifest never observed torn or
  missing its `kind`, converged on all 3 projects, **zero `.bdrive-tmp-*`
  litter** (`/tmp/r9repo/internal/daemon/zz_r9_twodaemons_test.go`).
- `-race` across the whole workspace test set (`internal/{config,daemon,agenthooks,syncer}`,
  `-run 'Workspace|Root|Designate|Mount|Project|HookGuard'`): all four **ok**.
- Daemon refresh racing a connect's designation: covered by the combined test
  above; designation is a no-op once the root exists, so there is nothing to lose.

**The manifest as untrusted input**
- **`LoadWorkspace` still has zero non-test callers** (21 test callers). Grepping
  `WorkspaceFile|workspace.json|workspaceConfigPath` over all non-test Go: the
  only production readers/writers/removers live in
  `internal/config/workspace.go`. **No path is ever built from an entry**, so the
  documented validation gap remains inert and nothing new consumes one.
- 15 hostile manifests through `IsWorkspaceRoot` + `LoadWorkspace`:
  `../../../../etc` and `/etc/passwd` as `Path`, NUL in `Path`, `../../evil` as
  `ID`, duplicate entries, invalid UTF-8, `kind` as array and as number,
  `projects` as a string, empty file, NUL-only file, 10 000- and 200 000-deep
  nesting, a 5 MB key, 100 000 entries. **No panic, no hang, no path built.**
  Deep nesting is refused by `encoding/json`'s max-depth check. Worst case 0.53 s.
  (The 100 MB case is slow rather than unsafe — reported under F9-1.)
- The same bodies planted as a project `config.json`, through `LoadProject`'s new
  `configKind` pre-pass: 200 k-deep → clean *"exceeded max depth"*;
  `{"kind":"WORKSPACE"}` and `{"kind":"workspace "}` load as ordinary projects
  (exact match, consistent with `IsWorkspaceRoot`); `{"Kind":"workspace"}` is
  refused via Go's case-insensitive field matching — harmless, no project has
  that field.

**Interaction with the rest of the product** — driven through the **real
binary** over a real root layout (`/tmp/r9cli.sh`, `/tmp/r9cli3.sh`)
- `bdrive sync` **at the root** syncs both projects under it and leaves the
  non-BearDrive sibling untouched; `sync` inside a project syncs just it.
- `log`, `grep`, `stale`, `scope`, `forget`, `share`, `url`, `stop` at a root all
  return the new workspace-root message instead of the dead-end
  *"run `bdrive init` there first"*. `share` from a **non-BearDrive sibling**
  under the root also lands on it (the `findProject` walk-up reaches `/`).
- `init` **at** a root: refused. `init` **above** a root: refused, naming it —
  in both the shipping layout `<root>/team` and the deep layout
  `<root>/a/b/team`, which is F7-5's case. `init` at `<root>/a` (no root strictly
  below) and at a fresh sibling **inside** the root: both pass the guards.
- The **documented known gap** reproduces exactly: a root whose projects this
  device never enrolled is invisible and `init` above it is not refused.
- **Renaming the root**: the manifest's relative paths stay valid, and the
  workspace-root message still fires at the new location.
  **Renaming a project inside the root** and **moving one out**: the manifest
  goes stale, which is what an index is allowed to do — nothing resolves from it.
- **`export`/`import`**: the archive is the project's *hub store* (journals +
  blobs), so it never contains `<root>/.bdrive/` — a root's manifest cannot
  travel between hubs, correctly.
- Asymmetry noted, not a finding: `sync` at a root fans out to every project
  under it, `stop` at a root refuses (it goes through `mustProject`). Pre-existing
  — before this change set `stop` refused with a worse message — and
  `project-files.md` already tells the user to run `stop` *in the projects*.

**The agent-hook guard, hand-run against a real `/bin/sh`** (guard string dumped
verbatim from `mountGuard()`; `/tmp/r9guardtest.sh`, 45 cases)
- Workspace root with and without a mount registered below; a non-BearDrive
  sibling under a root and a directory deep inside it; the project itself.
- **14 hostile root names**: spaces, `'`, `"`, `$(whoami)`, backticks, `*`,
  `[a-z]`, `;`, `|`, embedded newline, tab, `dot.dot`, leading `--`, non-ASCII.
  Every one: the project folder passes, the sibling skips.
- A root whose **name contains a newline**: the registry branch correctly
  refuses (the `case "$PWD" in *"\n"*` guard), and the walk-up still finds the
  project inside it.
- `$CLAUDE_PROJECT_DIR` pointing at a root (with and without a registered
  mount), at a sibling under a root, at `/`, and at a nonexistent path — all
  correct. The walk terminates at `/`.
- **No injection**: `$PWD` is expanded once into a double-quoted `grep -F`
  argument and never re-evaluated. Guard still spawns at most one `grep`, never
  `bdrive`, outside a mount. The change to `mountGuard` in this branch is
  **comment-only** — the shell text is byte-identical.

**Daemon-goroutine failure modes**
- Root `chmod 000` mid-refresh → silent no-op (the read fails, so the root reads
  as "not a root"); **no write**.
- Read-only `.bdrive` (stand-in for a full disk on `SaveWorkspace`) →
  `CreateTemp` fails, error returned and logged, **no partial file**.
- Manifest replaced by a **directory** → `IsWorkspaceRoot` false, refresh
  no-ops, `UndesignateWorkspace` removes it cleanly.
- **Zero `.bdrive-tmp-*` litter** after 50 clean refreshes and after the
  two-daemon churn. A refresh killed between `CreateTemp` and `Rename` would
  leave one behind, but the window is microseconds and the file is inert.
- A refresh outliving its daemon: the goroutine is never awaited, so it dies
  with the process; a kill mid-write costs at most one orphan temp file, never a
  torn manifest (rename is atomic).

**Repo invariants (CLAUDE.md §"Invariants — do not break these")** — checked
against the whole change set, all intact:
- Journals, blob-before-journal ordering, scan-before-pull, `journal.Less`/
  `Replay`, and materialize's dirty-file rule are **untouched by this branch**
  (no file under `internal/{journal,syncer}` is modified; the only syncer
  addition is a test).
- Atomic writes: `SaveWorkspace` → `writeJSON` → temp + `Rename`, temp prefix
  `.bdrive-tmp-`, which the scanner ignores. Verified empirically (no litter, no
  torn read under contention).
- Agent-hook guard stays pure shell: comment-only diff, verified by hand above.
- Daemon liveness via the `daemon.lock` flock, `Cycle` under the volume flock,
  and the pull/push→`Offline` degradation are untouched; the new refresh runs in
  its own goroutine and **cannot fail a cycle** (its error is logged only).
- `.bdrive/workspace.json` never syncs — proven by construction with a real
  two-device cycle, including from inside a mount (see F8-3 row).

---

## Suspicions (unproven)

- **`share.go`'s `findProject` now does two unbounded reads per ancestor.** It
  already did one (`LoadProject`); `config.IsWorkspaceRoot(cur)` adds a second,
  all the way to `/`, for any path not inside a project. I did not find a
  reachable hang that the pre-existing `LoadProject` read did not already
  expose, so I am not calling it a finding — but it doubles the exposure of a
  walk that reaches directories the user never named, and F9-1's fix would
  remove half of it for free.
- **`LoadProject` now unmarshals twice** for every `.bdrive/config.json`
  (`configKind`, then the real decode). Measured cost is negligible at real
  config sizes; I did not profile it in the syncer's per-directory walk, where
  `IsMount`'s stat — not `LoadProject` — is the hot path.
- **`IsWorkspaceRoot` returns false on *any* read error**, including `EACCES`
  and `EIO`. That is the safe direction for everything I could reach (a
  refresh no-ops, a re-designation just rewrites an index) and matches the
  stated "on disagreement the folder wins" posture, but I did not enumerate
  every consequence of a transient I/O error making a root momentarily invisible.
- **`project-files.md`: "every daemon start rebuilds the list".** True for
  projects that are immediate children of the root, which is the layout the page
  describes. For a project nested deeper (`<root>/team/wiki`),
  `RefreshWorkspaceOf` refreshes `<root>/team`, which is not a root, so the
  root's manifest is not rebuilt by that daemon. Harmless (such a project is not
  indexed anyway), and I could not turn it into an incorrect user-visible claim.

## What I could not check

- **Windows and Linux.** Everything here ran on macOS/APFS. `internal/autostart`'s
  Windows tests have still never executed, and `GOOS=windows go build ./...`
  still does not pass (pre-existing, unrelated to this branch:
  `store.Lock`/`daemon` use `syscall.Flock`/`Kill`/`Setsid`). The
  case-insensitive `underPath` fold is exercised only against APFS here.
- **Real TCC / real network stalls.** I used FIFOs, which are the project's own
  stand-in for both and which round 8 also used. A macOS consent dialog and a
  hung SMB mount block `open()` the same way, but I could not produce either.
- **The desktop UI itself.** F9-1's terminal state (409 forever) is derived from
  reading `handleDesktopInit` and reproducing the block in `DesignateWorkspace`;
  I did not drive the Tauri app.
- **A real hub.** All CLI exercise used `file://` remotes plus a dead-port
  server to make `init` fail fast after the guards. The guards themselves run
  before any network call, so this does not weaken the guard results.
- **Disk-full on `SaveWorkspace`.** Simulated with a read-only directory
  (`CreateTemp` fails) rather than a genuinely full filesystem; a partial write
  followed by `ENOSPC` on `Write` takes a different branch that I could only
  read, not run.
