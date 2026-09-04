package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

// ProjectDir is the per-folder settings directory at the mount root. It
// carries the mount's stable identity, so a project keeps syncing after the
// folder is renamed or moved — nothing is keyed by the path. It travels with
// the folder (copy the folder to a new machine and `bdrive init` resumes the
// same project) but is never synced, and it holds no session credentials —
// those stay in the bdrive home.
const ProjectDir = ".bdrive"

// ReservedDirs are directory names BearDrive never syncs, at any depth in a
// mount: .bdrive is the mount's own identity (syncing it would let one device
// silently repoint another) and .git carries hook scripts that would run on a
// teammate's next commit. The rule lives here beside ProjectDir because two
// packages enforce it — the sync engine on scan and on materialize, the hub
// on every destination path a client names — and two copies would drift.
//
// Match through ReservedDir, never by indexing this map: the comparison is
// case-insensitive because BearDrive's primary filesystems (APFS, NTFS) are.
// An exact-match guard lets ".GIT/hooks/pre-commit" through, and the
// filesystem then resolves it into the real .git/hooks.
var ReservedDirs = map[string]bool{".git": true, ProjectDir: true}

// ReservedDir reports whether a path segment names a reserved directory,
// under every spelling a filesystem folds onto the same directory.
//
// Case is one such folding (APFS, NTFS). Trailing dots and spaces are another:
// NTFS and SMB strip them when opening a path, so ".git./hooks/pre-commit" IS
// .git/hooks/pre-commit there — the same executable-hook plant an exact-match
// guard let through as ".GIT".
func ReservedDir(name string) bool {
	name = strings.TrimRight(name, ". ")
	for reserved := range ReservedDirs {
		if strings.EqualFold(name, reserved) {
			return true
		}
	}
	return false
}

// ReservedName reports whether a bare file name never syncs. Case-insensitive
// for the same reason as ReservedDir.
func ReservedName(name string) bool {
	lower := strings.ToLower(name)
	return lower == ".ds_store" || strings.HasPrefix(lower, ".bdrive-tmp-")
}

// agentHookConfigs are the per-project files a coding agent reads as
// EXECUTABLE configuration when a session starts in the folder: hook
// definitions, which are shell commands the agent runs on its own turns. Keyed
// agent-config-dir → file name, both matched through ReservedDir/ReservedName's
// folding rules.
//
// Why these are reserved rather than merely warned about, and why only these:
//
// This is the one shape in a synced folder that is not "content an agent reads"
// — the product's whole premise — but "code the agent runs, chosen by whoever
// wrote the file". internal/agenthooks already refuses to write hook config
// into a project, in its own words because a per-project file "living in a
// mount, would sync to the team". BearDrive knew the shape was dangerous when
// IT was the writer and said nothing about a teammate being one, so a peer's
// .claude/settings.json materialized silently into every member's folder.
//
// Reserving is symmetric — never scanned, never journaled, never materialized,
// the same treatment .git/hooks gets for the identical reason — which is what
// keeps it comprehensible: there is no half-synced file and no one-way drop.
// The cost is that a team cannot share project-level hook config through
// BearDrive, and that cost is one this product already declared it wanted: the
// documented place for hooks is each machine's USER config, installed once by
// `bdrive init`.
//
// Deliberately NOT reserved: CLAUDE.md, .claude/skills, .claude/commands,
// .claude/agents, and every other instruction a teammate writes for an agent to
// READ. Sharing those is the product. That they are trusted input the moment
// they land is a real and unstated design consequence, and the answer to it is
// documentation (INSTALL_FOR_AGENTS.md, the docs' Start-here path), not a
// filter that would break the feature.
//
// Derived from what each supported platform actually LOADS from a project
// folder, not from internal/agenthooks' table of what BearDrive itself writes.
// That mismatch is how `.mcp.json` — Claude Code's project-scoped MCP server
// list, whose {"command", "args"} pairs are processes the agent launches on
// session start — synced for a round while `.claude/settings.json` did not.
// The question this list answers is "what does the agent execute because the
// file is in the folder", and agenthooks only ever writes a subset of that.
var agentHookConfigs = map[string][]string{
	".claude": {"settings.json", "settings.local.json"},
	".codex":  {"hooks.json", "config.toml"},
	".gemini": {"settings.json"},
	".hermes": {"config.yaml"},
}

// AgentConfigDir reports whether a path segment names an agent's
// configuration directory — the keys of agentHookConfigs — under the same
// case and trailing-dot folding ReservedDir explains.
//
// It exists for one caller: `bdrive init` refusing such a directory as a
// MOUNT ROOT. The reserved-path rule only covers segments BELOW a root, so
// mounting ~/.claude leaves its settings.json a top-level file with no
// directory segment to match on — along with .credentials.json and every
// saved session under projects/. Only that direction leaks: a mount that
// CONTAINS ~/.claude sees .claude/settings.json, reserved at any depth.
//
// Exported here rather than spelled as a literal list in cmd/bdrive for the
// reason agentHookConfigs' own comment gives: a second copy of that list is
// how .mcp.json drifted out of it once already.
func AgentConfigDir(name string) bool {
	name = strings.TrimRight(name, ". ")
	for dir := range agentHookConfigs {
		if strings.EqualFold(name, dir) {
			return true
		}
	}
	return false
}

// agentHookFiles are the same thing at the folder ROOT, with no agent config
// directory to key on: `.mcp.json` is Claude Code's project-scoped MCP server
// definition. Reserved at any depth rather than at the root only, for the
// reason ReservedName is: the name is the whole signal, and a rule that holds
// in one directory and not another is one nobody can check.
var agentHookFiles = []string{".mcp.json"}

// AgentHookConfig reports whether a slash-separated path is an agent's
// project-level hook configuration. See agentHookConfigs.
func AgentHookConfig(p string) bool {
	dir, file := path.Split(p)
	// Same trailing-dot/space folding ReservedDir explains: NTFS and SMB open
	// ".mcp.json." as .mcp.json.
	bare := strings.TrimRight(file, ". ")
	for _, f := range agentHookFiles {
		if strings.EqualFold(bare, f) {
			return true
		}
	}
	dir = strings.TrimSuffix(dir, "/")
	if i := strings.LastIndexByte(dir, '/'); i >= 0 {
		dir = dir[i+1:]
	}
	dir = strings.TrimRight(dir, ". ")
	for agentDir, files := range agentHookConfigs {
		if !strings.EqualFold(dir, agentDir) {
			continue
		}
		for _, f := range files {
			if strings.EqualFold(strings.TrimRight(file, ". "), f) {
				return true
			}
		}
	}
	return false
}

// ReservedPath reports whether a slash-separated path is one BearDrive never
// carries: under a reserved directory, named like one, a reserved file name, or
// an agent's project-level hook config.
func ReservedPath(p string) bool {
	for _, part := range strings.Split(p, "/") {
		if ReservedDir(part) {
			return true
		}
	}
	return ReservedName(path.Base(p)) || AgentHookConfig(p)
}

// Project holds the settings stored in <folder>/.bdrive/config.json.
type Project struct {
	// ID is the stable mount identity (m-xxxxxxxx). The volume store, the
	// daemon, and the registry are keyed by it, never by the folder path.
	ID     string `json:"id"`
	Volume string `json:"volume,omitempty"`
	Remote string `json:"remote,omitempty"`
	// Include optionally narrows what syncs: when non-empty, only paths
	// matching one of these patterns (gitignore-style, same syntax as
	// .bdriveignore) are scanned and materialized.
	Include []string `json:"include,omitempty"`
	// PostSync is a shell command run on THIS device after a cycle applies a
	// teammate's changes, with the applied batch as JSON on stdin — the event
	// a local index, cache or notifier can hang off instead of polling.
	//
	// It lives here, and only here, on purpose: .bdrive is in ReservedDirs and
	// never syncs, so no hub response and no peer's journal can put a command
	// on someone else's machine.
	PostSync string `json:"post_sync,omitempty"`
}

// mountIDRe is the shape of a mount identity. The id is read verbatim from a
// folder's .bdrive/config.json — a file that arrives with the folder (a zip, a
// clone, a colleague's copy) — and is then joined straight onto $BDRIVE_HOME
// by VolumeDir and onto the volume dir by the store's state cache. Checking it
// here, where it is read, is what stops the whole volume store (cached blobs
// of every synced file, journals, the daemon's pid and lock) being created
// wherever the config's author chose.
var mountIDRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// ValidMountID reports whether id may be used as a mount identity.
func ValidMountID(id string) bool {
	return id != "." && id != ".." && mountIDRe.MatchString(id)
}

// NewMountID mints a stable mount identity.
func NewMountID() string {
	b := make([]byte, 4)
	rand.Read(b)
	return "m-" + hex.EncodeToString(b)
}

func projectConfigPath(folder string) string {
	return filepath.Join(folder, ProjectDir, "config.json")
}

// IsMount reports whether folder is a BearDrive mount root, i.e. has a
// .bdrive/config.json — even an unparseable or unreadable one, so callers
// that must not treat a mount as plain files (e.g. a parent mount's scanner)
// stay safe.
//
// A workspace root is NOT a mount, and needs nothing here: its manifest has
// its own name (WorkspaceFile), so a root has no config.json to stat. Reading
// the file to check its kind was tried and reverted — this runs per directory
// in the syncer's walk, where one unreadable or wedged config would hang a
// scan that a stat completes. The agent-hook walk-up matches on the same file
// name for the same reason, so Go and shell agree: a manifest hand-planted in
// config.json reads as a mount to both.
func IsMount(folder string) bool {
	_, err := os.Stat(projectConfigPath(folder))
	return err == nil
}

// LoadProject reads <folder>/.bdrive/config.json; ok is false if it does not
// exist.
func LoadProject(folder string) (Project, bool, error) {
	var p Project
	data, err := os.ReadFile(projectConfigPath(folder))
	if err != nil {
		if os.IsNotExist(err) {
			return p, false, nil
		}
		return p, false, err
	}
	// A manifest that landed here rather than in WorkspaceFile carries no id,
	// which the empty-id rule below would read as a legacy pre-id project —
	// handing every caller a project whose volume path is built from "".
	if configKind(data) == WorkspaceKind {
		return Project{}, false, fmt.Errorf("%s: workspace root, not a project", projectConfigPath(folder))
	}
	if err := json.Unmarshal(data, &p); err != nil {
		return p, false, fmt.Errorf("parse %s: %w", projectConfigPath(folder), err)
	}
	// An empty id is a config written before one was assigned; anything else
	// has to be a mount id, since everything downstream builds a path from it.
	if p.ID != "" && !ValidMountID(p.ID) {
		return Project{}, false, fmt.Errorf("%s: invalid mount id", projectConfigPath(folder))
	}
	p.Include = normalizeInclude(p.Include)
	return p, true, nil
}

// normalizeInclude anchors bare single-segment include entries to the mount
// root, so a config written before the fix ("wiki/") stops matching nested
// directories of the same name without needing a re-init. Only single-segment
// entries need it: compile() already anchors anything containing a slash.
// Entries with glob syntax are left alone — a hand-written pattern is a
// deliberate pattern.
func normalizeInclude(include []string) []string {
	for n, i := range include {
		s := strings.TrimSuffix(i, "/")
		if s == "" || strings.ContainsAny(s, "/*?[!") {
			continue
		}
		include[n] = "/" + i
	}
	return include
}

// mountLivesAt reports whether path still holds the config of mount id.
func mountLivesAt(path, id string) bool {
	p, ok, err := LoadProject(path)
	return err == nil && ok && p.ID == id
}

// samePath reports whether two spellings name the same directory (macOS
// /var vs /private/var, a symlinked home): a spelling difference is not a
// move, and must not read as one in either direction.
func samePath(a, b string) bool {
	if a == b {
		return true
	}
	ra, err1 := filepath.EvalSymlinks(a)
	rb, err2 := filepath.EvalSymlinks(b)
	return err1 == nil && err2 == nil && ra == rb
}

// SaveProject writes <folder>/.bdrive/config.json, assigning a mount ID on
// first save.
func SaveProject(folder string, p Project) (Project, error) {
	if p.ID == "" {
		p.ID = NewMountID()
	}
	if err := os.MkdirAll(filepath.Join(folder, ProjectDir), 0o755); err != nil {
		return p, err
	}
	return p, writeJSON(projectConfigPath(folder), p)
}

// ResolveMount loads a folder's project settings and self-heals the
// registry: if the folder was renamed or moved, the registry entry is
// updated to the new path so `bdrive status` and the daemon find it again.
//
// It never CREATES a row. Enrolling this device in a project is `bdrive init`
// (EnrollMount) and nothing else: .bdrive/config.json travels with a folder —
// a clone, an unpacked archive, a colleague's copy — so its presence is not
// consent to sync, and syncBlocked's "init" arm is the gate that says so.
// Creating the row here made that gate unreachable for every command that
// resolves a folder before consulting it (`bdrive restore`, `bdrive forget`):
// one run inside an attacker-supplied folder put an attacker-chosen remote in
// the registry, and the login autostart runs `bdrive resume`, which starts a
// daemon for every enrolled row. A function every folder-taking command calls
// must not be a write with a read-shaped name.
func ResolveMount(folder string) (Project, bool, error) {
	p, ok, err := LoadProject(folder)
	if err != nil || !ok {
		return p, ok, err
	}
	mounts, err := LoadMounts()
	if err != nil {
		return p, true, err
	}
	mi, registered := mounts[p.ID]
	if !registered {
		return p, true, nil
	}
	// The self-heal follows a mount that MOVED, and .bdrive/config.json
	// travels with the folder — a clone, an unpacked archive, a colleague's
	// copy — so "some folder carries this id" is not "this mount is now
	// there". If the recorded path still holds this mount's own config, the
	// mount did not move and the arriving folder is a copy: re-pointing the
	// row would hand the real project's Path, Volume and Remote to it, and
	// `bdrive resume` (and the login autostart) start the daemon from that
	// row. Enrolling a folder is what `bdrive init` is for.
	//
	// "Still holds a config" alone was too strong in the other direction: it
	// made the guard a denial primitive with no attacker in it. Anything that
	// RE-CREATES the recorded path holding this mount's config — a backup
	// restore, an interrupted `cp -r`, a file-sync client putting a deleted
	// directory back — stranded the genuinely moved folder, with no way out,
	// because `bdrive init` (the remedy the error itself names) resolves the
	// mount before it does anything else and failed identically.
	//
	// Both folders can carry byte-identical settings, so the discriminator
	// cannot be their contents. It is the filesystem's identity for the
	// directory (dirID): a rename keeps it, a copy never reproduces it. If the
	// arriving folder IS the directory this row was written for, the mount
	// moved and the self-heal is exactly right; if it is not, the recorded path
	// still holding this mount's config means somebody else is claiming the
	// row. Rows with no recorded identity (written before this, or a platform
	// that has none) keep the conservative answer.
	dev, ino := dirID(folder)
	moved := dev != 0 && dev == mi.Dev && ino == mi.Ino
	if !samePath(mi.Path, folder) && !moved {
		if mountLivesAt(mi.Path, p.ID) {
			return p, false, fmt.Errorf("%s carries the settings of project %s, which this device already "+
				"syncs at %s — a copy of a project folder is not that project; run `bdrive init` here to "+
				"connect this folder to a project", folder, p.ID, mi.Path)
		}
		if mi.Dev != 0 && dev != 0 {
			// The recorded path did not answer. That is every ordinary reason
			// a path stops answering for a moment — an external volume not
			// mounted yet at login, a rename in flight, a restore — and it is
			// NOT evidence that this folder is the mount. This folder provably
			// is not the directory the row was written for, so it uses the
			// settings and leaves the row alone; taking it would overwrite the
			// identity that is the whole point of the field, and the real
			// folder would come back as the one that cannot prove itself.
			// Re-pointing a mount is `bdrive init` (EnrollMount).
			return p, true, nil
		}
	}
	if mi.Path != folder || mi.Volume != p.Volume || mi.Remote != p.Remote ||
		mi.Dev != dev || mi.Ino != ino {
		mounts[p.ID] = MountInfo{Path: folder, Volume: p.Volume, Remote: p.Remote, Dev: dev, Ino: ino}
		if err := SaveMounts(mounts); err != nil {
			return p, true, err
		}
	}
	return p, true, nil
}

// EnrollMount is ResolveMount plus the one thing ResolveMount refuses to do:
// create this device's registry row for a project. It is the enrollment
// gesture, so exactly one caller has any business using it — `bdrive init`
// (startSync).
func EnrollMount(folder string) (Project, bool, error) {
	p, ok, err := ResolveMount(folder)
	if err != nil || !ok {
		return p, ok, err
	}
	mounts, err := LoadMounts()
	if err != nil {
		return p, true, err
	}
	// init is also the repair gesture: it re-points a row unconditionally, which
	// is the documented remedy every "run `bdrive init` here" message names —
	// including for the one case ResolveMount deliberately will not decide (a
	// move whose old path is gone and whose new directory has a new identity,
	// e.g. across filesystems).
	dev, ino := dirID(folder)
	mounts[p.ID] = MountInfo{Path: folder, Volume: p.Volume, Remote: p.Remote, Dev: dev, Ino: ino}
	return p, true, SaveMounts(mounts)
}
