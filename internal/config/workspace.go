package config

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// WorkspaceFile is the manifest at a workspace root, inside the root's
// .bdrive directory beside — never as — a project's config.json.
//
// The name is the whole guard. DESIGN.md put the manifest IN config.json and
// told the readers apart by a "kind" field. That is fine for Go and wrong for
// the agent-hook walk-up, which is pure shell running on every tool call of
// every agent session on the machine: it would have to inspect each ancestor's
// config to decide whether to stop there.
//
// Not because it is unaffordable — `read` is a builtin, so a shell-only check
// costs no processes — but because it makes correctness in that guard depend
// on knowing what a workspace is. A distinct file name makes the walk-up,
// IsMount and LoadProject right by construction: nothing has to have heard of
// workspaces to behave correctly around one.
const WorkspaceFile = "workspace.json"

// WorkspaceKind is the manifest's self-description, kept for the case the two
// files ever meet anyway: a hand-written or older root that put the manifest
// in config.json is refused by LoadProject and is not a mount to IsMount,
// rather than reading as a project with no identity.
const WorkspaceKind = "workspace"

func workspaceConfigPath(root string) string {
	return filepath.Join(root, ProjectDir, WorkspaceFile)
}

// WorkspaceProject is one entry of the index: where a project folder sits
// (relative to the root, so the root can be renamed or moved) and which mount
// it is.
type WorkspaceProject struct {
	Path string `json:"path"`
	ID   string `json:"id"`
}

// Workspace is the manifest at a workspace root — the one place that answers
// "what on this machine is BearDrive, and what isn't".
//
// It is an INDEX, not the source of truth. Identity stays in each project
// folder's own .bdrive/config.json, which is what keeps a folder meaningful
// on its own: copy it to another machine and `bdrive init` resumes the same
// project. Move identity up here and a folder alone means nothing.
//
// So nothing is ever resolved FROM this file — no volume path, no journal, no
// mount id, no permission. On disagreement the folder wins: the manifest is
// rebuilt from a scan (ScanWorkspace) and a stale or hand-edited entry is
// corrected, never obeyed.
type Workspace struct {
	Kind     string             `json:"kind"`
	Projects []WorkspaceProject `json:"projects"`
}

// configKind reads the "kind" of a raw .bdrive/config.json body. An
// unparseable body has no kind, so it reads as a project — the safe
// direction, since the cost of mistaking a root for a project is a refusal
// and the cost of the reverse is a scanner treating a mount as plain files.
func configKind(data []byte) string {
	var h struct {
		Kind string `json:"kind"`
	}
	_ = json.Unmarshal(data, &h)
	return h.Kind
}

// IsWorkspaceRoot reports whether folder carries a workspace manifest — a
// file that parses and says so.
//
// IT READS THE FILE, so it can block: a FIFO, a device node or a file on a
// stalled network mount at that path stops it forever. Use HasManifest where
// only existence matters, which is everywhere a UI or a command would hang.
func IsWorkspaceRoot(folder string) bool {
	data, err := readManifest(workspaceConfigPath(folder))
	return err == nil && configKind(data) == WorkspaceKind
}

// HasManifest reports whether anything at all occupies the manifest path.
//
// A stat, so it cannot block, and that is the point: it answers the question
// every caller on a critical path actually has — "is this folder already
// spoken for as a root?" — without opening anything. It is deliberately
// coarser than IsWorkspaceRoot: a file there that is not a manifest still
// counts, because the right response to an unknown file at that path is to
// leave it alone, never to overwrite it.
func HasManifest(folder string) bool {
	_, err := os.Stat(workspaceConfigPath(folder))
	return err == nil
}

// maxManifestBytes bounds a manifest read. The file is an index of a folder's
// immediate children; anything larger is not one, and reading it is time a
// caller may not have (a 100 MB file cost 5.4s per call).
const maxManifestBytes = 1 << 20

// readManifest reads at most maxManifestBytes of p.
func readManifest(p string) ([]byte, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, maxManifestBytes))
}

// LoadWorkspace reads <root>/.bdrive/workspace.json; ok is false if there is
// none. A file that does not describe itself as a workspace is an error, not
// an empty manifest.
func LoadWorkspace(root string) (Workspace, bool, error) {
	var w Workspace
	data, err := readManifest(workspaceConfigPath(root))
	if err != nil {
		if os.IsNotExist(err) {
			return w, false, nil
		}
		return w, false, err
	}
	if err := json.Unmarshal(data, &w); err != nil {
		return Workspace{}, false, fmt.Errorf("parse %s: %w", workspaceConfigPath(root), err)
	}
	if w.Kind != WorkspaceKind {
		return Workspace{}, false, fmt.Errorf("%s: not a workspace root", workspaceConfigPath(root))
	}
	return w, true, nil
}

// SaveWorkspace writes the manifest at root.
//
// It will not resurrect a root the user has deleted: MkdirAll happily rebuilds
// the whole chain, so a refresh racing an `rm -rf` of the root recreated the
// directory (observed 9 times in 300 runs) — leaving a folder the user removed
// with a .bdrive in it. The root itself must already exist; only the .bdrive
// inside it is created here.
func SaveWorkspace(root string, w Workspace) error {
	w.Kind = WorkspaceKind
	if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
		return fmt.Errorf("%s is not a directory: not writing a workspace manifest into it", root)
	}
	if err := os.Mkdir(filepath.Join(root, ProjectDir), 0o755); err != nil && !os.IsExist(err) {
		return err
	}
	return writeJSON(workspaceConfigPath(root), w)
}

// ScanWorkspace builds the manifest from what is on disk: the root's
// immediate children that are mounts, in name order (os.ReadDir sorts).
//
// Immediate children only, and deliberately so. Nested mounts DO exist in this
// product (see syncer's vNested handling) — the manifest simply does not index
// them: it answers "which folders in my workspace root are projects", and a
// project inside a project is that project's business.
//
// This is the only way a manifest is ever produced, which is what makes the
// folder the source of truth.
func ScanWorkspace(root string) (Workspace, error) {
	w := Workspace{Kind: WorkspaceKind}
	ents, err := os.ReadDir(root)
	if err != nil {
		return w, err
	}
	for _, e := range ents {
		if ReservedDir(e.Name()) {
			continue
		}
		// os.ReadDir reports a symlink's OWN type, so e.IsDir() is false for a
		// symlink to a project — a shape people really do keep beside their
		// folders. Stat through it; LoadProject below is the real test anyway,
		// and it fails harmlessly on anything that is not a directory.
		if !e.IsDir() {
			fi, serr := os.Stat(filepath.Join(root, e.Name()))
			if serr != nil || !fi.IsDir() {
				continue
			}
		}
		p, ok, err := LoadProject(filepath.Join(root, e.Name()))
		if err != nil || !ok {
			continue
		}
		w.Projects = append(w.Projects, WorkspaceProject{Path: e.Name(), ID: p.ID})
	}
	return w, nil
}

// RefreshWorkspace rewrites root's manifest from a scan, and is a no-op
// unless root already is a workspace root: a manifest is never created by
// stumbling onto a parent directory, only by the connect flow that
// deliberately designates one (DesignateWorkspace).
//
// ONLY CALL THIS WHERE BLOCKING IS HARMLESS. ScanWorkspace reads a directory
// the user chose plus one config per child, none of it bounded — a FIFO, a
// TCC-gated folder or a dead network path stops it forever. Today its one
// caller is the goroutine daemon.Run starts, where a stall costs a stale
// index and nothing else. Putting it in front of a command or a UI step has
// hung `bdrive init`, `startSync` and the desktop connect, once each.
func RefreshWorkspace(root string) error {
	if !IsWorkspaceRoot(root) {
		return nil
	}
	w, err := ScanWorkspace(root)
	if err != nil {
		return err
	}
	// Re-checked after the scan, which may have taken a long time: the user
	// can delete the manifest to un-root the folder while one is in flight,
	// and writing here would silently undo that. The window is narrow rather
	// than closed — a delete landing between this check and the write still
	// loses — so `bdrive stop` before deleting is the reliable order.
	if !IsWorkspaceRoot(root) {
		return nil
	}
	return SaveWorkspace(root, w)
}

// checkRootHere applies the placement rules that need NOTHING but the folder
// itself:
//
//   - a root is never itself a mount (it would sync, and everything in it),
//   - a root's .bdrive never contains, and is never inside, the beardrive home.
//
// Cost: one stat of the folder's own config.json, path arithmetic, and the
// lstat-per-component that canonicalising the folder and the home costs
// (resolveExisting). It never OPENS a file and never lists a directory, and
// every path it touches is one the caller named — so it is safe on a UI's
// critical path. That property is the whole reason it is separate from
// CheckRootPlacement, which walks ancestors nobody named; see there.
func checkRootHere(folder string) error {
	if IsMount(folder) {
		return fmt.Errorf("%s is a project folder: a workspace root is never itself a mount", folder)
	}
	return checkRootAllowed(folder)
}

// CheckRootPlacement is checkRootHere plus the rules that require looking at
// ANCESTORS: roots do not nest, and a root is never inside a project.
//
// IT READS ONE FILE PER ANCESTOR, up to the filesystem root, and those are
// directories the caller never named — so any one of them being a FIFO, a
// dead network mount or a TCC-gated folder blocks it forever. That is why
// DesignateWorkspace does NOT call this: it runs inside the desktop connect,
// where blocking means a UI stuck at "connecting" with no cancel, no undo,
// and a 409 on every retry.
//
// SO NOTHING IN THE SHIPPED PRODUCT CALLS THIS. Its only caller is
// InitWorkspace, which has no production callers either — `bdrive init` never
// designates a root, it only refuses to mount one. Both ancestor rules are
// therefore dormant today, enforced by tests and waiting for a gesture that
// can afford to block ("designate this existing tree"). The connect flow
// applies checkRootHere and, as DESIGN.md states, does not enforce them.
func CheckRootPlacement(folder string) error {
	if err := checkRootHere(folder); err != nil {
		return err
	}
	for cur := filepath.Dir(folder); ; cur = filepath.Dir(cur) {
		// HasManifest, not IsWorkspaceRoot: these are directories nobody
		// named, so one of them holding a FIFO must not stop the walk dead.
		if HasManifest(cur) {
			return fmt.Errorf("%s is already inside the workspace root at %s: roots do not nest", folder, cur)
		}
		if IsMount(cur) {
			// Not because the manifest would sync — .bdrive is a ReservedDir
			// at any depth, so it never does — but because a root inside a
			// project describes folders that are already somebody else's
			// content, and the two answers to "what is BearDrive here" then
			// disagree.
			return fmt.Errorf("%s is inside the project at %s: a workspace root indexes folders "+
				"that project already syncs", folder, cur)
		}
		if filepath.Dir(cur) == cur {
			return nil
		}
	}
}

// checkRootAllowed refuses a folder whose manifest would land in the
// beardrive home — at $HOME, `<folder>/.bdrive` IS the home by default,
// holding this device's token, its identity and every project's journals.
// Writing an index in there conflates the two stores and makes
// IsWorkspaceRoot($HOME) depend on a file in $BDRIVE_HOME.
//
// Containment, not equality: a custom $BDRIVE_HOME may sit deeper, and the
// same conflation applies to anything under it. A home that does not exist yet
// still counts, which is why both sides go through resolveExisting rather than
// EvalSymlinks — the latter gives up entirely on a missing last component, and
// that is exactly the case here (a .bdrive about to be created, a home on a
// machine that has never synced), so an aliased spelling walked straight past
// a comparison built on it.
func checkRootAllowed(folder string) error {
	home, err := Home()
	if err != nil {
		return nil // no home to protect
	}
	dir := filepath.Join(folder, ProjectDir)
	// Compare through symlinks, on BOTH sides. Two aliases of one directory
	// must not answer differently, and either side can be the aliased one:
	// Home() returns $BDRIVE_HOME/$HOME verbatim, which is whatever the user
	// or the launcher spelled, and the folder comes from a UI or a shell.
	//
	// The .bdrive itself usually does not exist yet and EvalSymlinks fails on
	// a missing path, so resolve the FOLDER — which does exist — and rebuild
	// the path under it. Resolving only one side was tried and left the hole
	// open from the other direction.
	return homeConflict(resolveExisting(dir), resolveExisting(home))
}

// resolveExisting resolves as much of p as exists and rejoins the rest. Both
// paths here routinely end in a directory that has not been created yet — the
// .bdrive of a folder about to become a root, or a $BDRIVE_HOME on a machine
// that has never synced — and plain EvalSymlinks fails outright on those,
// which is how an aliased spelling slipped past a comparison that resolved
// only one side.
//
// Falls back to p unchanged when nothing along the path resolves.
func resolveExisting(p string) string {
	p = filepath.Clean(p)
	var tail []string
	for cur := p; ; {
		if real, err := filepath.EvalSymlinks(cur); err == nil {
			for i := len(tail) - 1; i >= 0; i-- {
				real = filepath.Join(real, tail[i])
			}
			return real
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p
		}
		tail = append(tail, filepath.Base(cur))
		cur = parent
	}
}

// homeConflict reports whether dir and the beardrive home contain one another.
func homeConflict(dir, home string) error {
	if samePath(dir, home) || underPath(home, dir) || underPath(dir, home) {
		return fmt.Errorf("%s cannot be a workspace root: it and the beardrive home (%s) "+
			"contain one another, and the home holds this device's credentials",
			filepath.Dir(dir), home)
	}
	return nil
}

// underPath reports whether p is inside root, lexically, after cleaning both.
// Unresolved on purpose: it must answer for directories that do not exist.
//
// Case-insensitively, because it guards a refusal and the primary filesystems
// here fold case (APFS, NTFS): on a default macOS volume `/Users/x` and
// `/USERS/X` are the same directory, and a case-sensitive check let the second
// spelling write an index into the beardrive home. Over-refusing a genuinely
// distinct path on a case-sensitive volume costs one unusable workspace-root
// location; under-refusing costs a manifest beside the device token.
func underPath(root, p string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(p))
	if err != nil {
		return false
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		// Try again folded, for the same paths spelled in different case.
		rel, err = filepath.Rel(strings.ToLower(filepath.Clean(root)), strings.ToLower(filepath.Clean(p)))
		return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
	}
	return true
}

// DesignateWorkspace makes root a workspace root holding exactly p, and
// reports whether it created the manifest (false when root already was one).
//
// Scan-free on purpose, and that is the whole point of it existing beside
// InitWorkspace: this runs inside the desktop connect flow, where a blocking
// call is a UI wedged forever with no way back. It writes what the caller
// already knows — the project it just created — and leaves discovering
// everything else to the daemon's refresh, which can afford to block.
//
// The caller must already have proven root is reachable (the connect flow
// stats and writes inside it before reaching here); this adds one stat and
// one atomic write.
func DesignateWorkspace(root string, p WorkspaceProject) (bool, error) {
	// HasManifest, not IsWorkspaceRoot: the latter OPENS the file, and a FIFO
	// or a stalled network mount at that exact path would hang the connect
	// forever — onboarding.running never clears, so every retry 409s for the
	// life of the sidecar. Nothing earlier in the flow touches <root>/.bdrive,
	// so this is the first thing to reach it. A stat answers the question this
	// function has, and leaves an unknown file alone rather than overwriting.
	if HasManifest(root) {
		return false, nil
	}
	// checkRootHere, NOT CheckRootPlacement: the ancestor half reads a file per
	// level of directories nobody named, and one wedged ancestor hangs the
	// connect forever at "connecting" — no cancel, no undo, 409 on retry.
	//
	// What this keeps is what matters most and costs one stat: a folder that
	// is already a mount must not also become a root. A clone carrying
	// .bdrive/config.json, picked as the connect root, otherwise becomes both,
	// after which `bdrive init` there refuses forever with no CLI route back.
	//
	// What it gives up is nesting: the connect flow can produce a root inside
	// a root. That is an index with two owners, not a sync fault, and it is
	// stated in DESIGN.md rather than paid for with a hang.
	if err := checkRootHere(root); err != nil {
		return false, err
	}
	return true, SaveWorkspace(root, Workspace{Projects: []WorkspaceProject{p}})
}

// UndesignateWorkspace removes root's manifest, un-rooting the folder. Used
// by the connect flow to hand a folder back when a connect it designated
// then failed.
// It removes the manifest and nothing else. Removing the .bdrive directory
// too was tried: "only if now empty" is also true of an empty .bdrive the
// user already had, and os.Remove unlinks a symlink whatever it points at. An
// empty .bdrive left behind is inert — not a mount, not a root — which is a
// far better outcome than deleting a directory this run did not create.
func UndesignateWorkspace(root string) error {
	if err := os.Remove(workspaceConfigPath(root)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// RefreshWorkspaceOf refreshes the manifest of folder's parent, if the parent
// is a root. It is what a project-creating flow calls: only the root directly
// above a new project can gain an entry, since roots do not nest.
func RefreshWorkspaceOf(folder string) error {
	return RefreshWorkspace(filepath.Dir(folder))
}

// InitWorkspace designates folder as a workspace root and writes the manifest
// from a scan of the projects already inside it.
//
// IT SCANS, so it inherits ScanWorkspace's warning: only call it where
// blocking forever is harmless. It has NO production callers — the connect
// flow uses DesignateWorkspace, which is scan-free — and it exists for tests
// and for a future "designate this existing tree" gesture that can afford to
// block. If you are reaching for it from a command or a request handler, you
// want DesignateWorkspace.
//
// Placement rules are CheckRootPlacement's, shared with DesignateWorkspace.
func InitWorkspace(folder string) error {
	if IsWorkspaceRoot(folder) {
		return RefreshWorkspace(folder)
	}
	if err := CheckRootPlacement(folder); err != nil {
		return err
	}
	w, err := ScanWorkspace(folder)
	if err != nil {
		return err
	}
	return SaveWorkspace(folder, w)
}
