// Package agenthooks detects which AI agent platforms a user works with and
// registers BearDrive's sync hooks in each platform's own hook config, so
// files sync at turn boundaries no matter which agent edits them.
//
// Every supported platform runs command hooks the same way — spawn a shell
// command, pipe event JSON (with a session_id) on stdin — so one hook command
// works everywhere; only the config file format and event names differ:
//
//	claude  ~/.claude/settings.json  UserPromptSubmit / PostToolUse
//	codex   ~/.codex/hooks.json      UserPromptSubmit / PostToolUse
//	gemini  ~/.gemini/settings.json  BeforeAgent / AfterTool
//	hermes  ~/.hermes/config.yaml    pre_llm_call / post_tool_call
//
// Every config is USER-level, written once per machine. Platforms read hook
// config only from the directory a session starts in — never a parent, never
// a subfolder — so a per-project file would fire only for sessions that
// happen to start there, and (living inside a mount) would sync to the whole
// team. One user-level registration covers every session in every folder;
// the guard below makes it a no-op outside BearDrive projects. Install
// migrates away any project-level hooks earlier versions wrote.
//
// The hook syncs the project and stamps changes with "<agent> session <id>"
// (see `bdrive sync --note`), so hub history links every change to the agent
// session that made it. A third hook runs `bdrive read-log` on each
// platform's read-shaped tools — native file reads, grep-style searches
// (the files the matches came from), and shell commands (the existing files
// they name) — queueing agent file reads for the hub's read heatmap
// (drained on the next sync — the hook itself never touches the network).
// Listing tools (glob, ls) are deliberately unmatched: seeing a file's name
// is not reading it. Hooks are fast no-ops outside bdrive projects, and
// reinstalling upgrades a registered hook's matcher in place when coverage
// grows.
package agenthooks

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/runbear-io/beardrive/internal/store"
)

// Markers identify our hooks inside a config, for idempotency and status.
// The sync and read hooks are separate groups (different matchers), so each
// carries its own marker — re-running install on a config that predates the
// read hook adds just the missing group.
const (
	marker     = "bdrive sync"
	readMarker = "bdrive read-log"
)

// ourEvents is every event name any platform above registers under. Removal is
// scoped to it, because `bdrive hooks uninstall` promises to leave other hooks
// untouched and the marker alone cannot keep that promise: it is a substring
// hunt for "bdrive sync", so a user's own `cd ~/wiki && bdrive sync .` — under
// SessionStart, an event this package has never written to — was deleted from
// their machine-wide agent config.
//
// Scoping by event, not by exact command, because previous versions wrote
// different command shapes and those still have to be removable (see
// removeProjectHooks). Add an event here whenever a platform above gains one,
// or uninstall silently stops removing it.
var ourEvents = map[string]bool{
	// claude, codex
	"UserPromptSubmit": true, "PostToolUse": true,
	// gemini
	"BeforeAgent": true, "AfterTool": true,
	// hermes
	"pre_llm_call": true, "post_tool_call": true,
}

// Agent names, in the order they are reported.
var Agents = []string{"claude", "codex", "gemini", "hermes"}

// Result reports what Install did for one agent platform.
type Result struct {
	Agent   string
	Path    string // config file the hooks live in
	Changed bool   // false = already registered
	Note    string // extra step the user must take, if any
	// Migrated names a project-level config an earlier version wrote, whose
	// hooks this run removed. Empty when there was nothing to clean up.
	Migrated string
}

// mountGuard is the fast pre-check every hook opens with. It runs on every
// tool call in every folder, so it stays pure shell — no process spawn on the
// common "not a bdrive folder" path. It answers in both directions, because a
// session's directory is rarely the mount root: walk up for an enclosing
// mount (editor opened inside the synced wiki/), else ask the registry whether
// any mount lives below (editor opened at the repo root above wiki/ and
// docs/). Matching on .bdrive/config.json rather than the directory keeps
// $BDRIVE_HOME (~/.bdrive, which has no config.json) from reading as a mount.
func mountGuard() string {
	return `cd "${CLAUDE_PROJECT_DIR:-.}" 2>/dev/null || exit 0; ` +
		// Matching config.json is also what keeps a WORKSPACE ROOT from
		// stopping the walk: the root's manifest is .bdrive/workspace.json,
		// deliberately a different name (internal/config/workspace.go), so a
		// non-BearDrive folder beside the projects climbs straight past it and
		// falls through to the registry check below — without this guard
		// having to know what a workspace is.
		//
		// The consequence, stated: a manifest hand-written into config.json
		// instead DOES stop the walk here, and this guard will not notice.
		// Nothing writes that layout; if something ever does, this is the line
		// that has to learn about it.
		`d=$PWD; while [ ! -f "$d/.bdrive/config.json" ]; do case "$d" in ""|/) d=; break;; esac; d=${d%/*}; done; ` +
		// $PWD is interpolated into the grep pattern below, and grep -F reads
		// every LINE of its pattern as a separate alternative — a directory
		// name containing a newline splits it, and a blank line among the
		// pieces is the empty pattern, which matches every mount. No mount
		// path can contain a newline anyway, so such a folder is never one.
		"[ -n \"$d\" ] || case \"$PWD\" in *\"\n\"*) exit 0;; esac; " +
		`[ -n "$d" ] || grep -qF "\"$PWD/" "${BDRIVE_HOME:-$HOME/.bdrive}/mounts.json" 2>/dev/null || exit 0; ` +
		`command -v bdrive >/dev/null || exit 0; `
}

// hookCommand is the one shell command every platform runs: sync the project
// if it is a bdrive mount, stamping changes with the agent session id parsed
// from the hook's stdin JSON. POSIX sh only — no jq, no bashisms.
func hookCommand(label string) string {
	return `sh -c '` + mountGuard() +
		`s=; [ -t 0 ] || s=$(head -c 8192 2>/dev/null | tr -d \" | sed -n "s/.*session_id[[:space:]]*:[[:space:]]*\([a-zA-Z0-9_-]*\).*/\1/p" | head -n 1); ` +
		`if [ -n "$s" ]; then bdrive sync . --note "` + label + ` session $s" >/dev/null 2>&1 || true; ` +
		`else bdrive sync . >/dev/null 2>&1 || true; fi'`
}

// hookPullCommand is Claude Code's turn-start hook: `bdrive sync --hook`
// pulls, stamps the session note, and emits the project's gated-link
// formula as additionalContext (hookSpecificOutput JSON on stdout — which
// is why stdout must NOT be discarded here). Claude-only: the JSON
// contract is Claude Code's.
func hookPullCommand(label string) string {
	return `sh -c '` + mountGuard() +
		`bdrive sync . --hook ` + label + ` 2>/dev/null'`
}

// readHookCommand queues agent file reads for the hub's read heatmap:
// `bdrive read-log` parses the hook's stdin JSON itself and only appends to
// a local spool, so this stays cheap enough to run on every read-tool call.
func readHookCommand() string {
	return `sh -c '` + mountGuard() +
		`bdrive read-log . >/dev/null 2>&1 || true'`
}

type platform struct {
	label      string // session-note label
	projectDir string // presence of this dir (project or home) = detected
	userLevel  bool   // config lives in the home dir, not the project
	install    func(folder string) (path string, changed bool, err error)
	note       string
}

var platforms = map[string]platform{
	"claude": {
		label:      "claude-code",
		projectDir: ".claude",
		install: func(string) (string, bool, error) {
			// Reads happen through more than the Read tool: Grep consumes
			// the files its matches come from, and Bash reads whatever
			// files the command names (`read-log` mines both payloads).
			// Glob stays unmatched on purpose — listing names isn't reading.
			return mergeJSONHooks(ConfigPath("", "claude"),
				"UserPromptSubmit", "PostToolUse", "Write|Edit|MultiEdit", "Read|Grep|Bash", "claude-code", 30, true,
				hookPullCommand("claude-code"))
		},
	},
	"codex": {
		label:      "codex",
		projectDir: ".codex",
		install: func(string) (string, bool, error) {
			// Codex reads mostly happen through shell commands; read-log
			// mines the command line for the files it names.
			return mergeJSONHooks(ConfigPath("", "codex"),
				"UserPromptSubmit", "PostToolUse", "apply_patch", "read_file|shell", "codex", 30, false, "")
		},
		// Codex hooks are experimental and off by default, and Codex asks
		// the user to trust each hook definition once.
		note: "enable hooks in ~/.codex/config.toml ([features] codex_hooks = true), then trust the hook when Codex asks",
	},
	"gemini": {
		label:      "gemini",
		projectDir: ".gemini",
		install: func(string) (string, bool, error) {
			// Gemini uses its own event names and millisecond timeouts.
			return mergeJSONHooks(ConfigPath("", "gemini"),
				"BeforeAgent", "AfterTool", "write_file|replace|edit",
				"read_file|read_many_files|search_file_content|run_shell_command", "gemini", 30000, false, "")
		},
	},
	"hermes": {
		label:     "hermes",
		userLevel: true,
		install:   installHermes,
	},
}

// Detect reports which agent platforms are in use, judged by their config
// dirs existing in the project or the home directory.
func Detect(folder string) []string {
	home, _ := os.UserHomeDir()
	var found []string
	for _, name := range Agents {
		p := platforms[name]
		switch {
		case p.userLevel:
			if dirExists(filepath.Join(home, "."+name)) {
				found = append(found, name)
			}
		case dirExists(filepath.Join(folder, p.projectDir)) ||
			(home != "" && dirExists(filepath.Join(home, p.projectDir))):
			found = append(found, name)
		}
	}
	return found
}

// Registered reports whether an agent's config already carries our hooks.
func Registered(folder, agent string) bool {
	data, err := os.ReadFile(ConfigPath(folder, agent))
	return err == nil && strings.Contains(string(data), marker)
}

// ConfigPath returns where an agent's hooks are (or would be) registered:
// always the platform's USER-level config. The folder argument is ignored and
// kept only so callers read naturally; see the package doc for why hooks are
// no longer written per project.
func ConfigPath(_ string, agent string) string {
	home, _ := os.UserHomeDir()
	switch agent {
	case "claude":
		return filepath.Join(home, ".claude", "settings.json")
	case "codex":
		return filepath.Join(home, ".codex", "hooks.json")
	case "gemini":
		return filepath.Join(home, ".gemini", "settings.json")
	case "hermes":
		return filepath.Join(home, ".hermes", "config.yaml")
	}
	return ""
}

// projectConfigPath is where PREVIOUS versions wrote a platform's hooks. Only
// the migration in Install reads these, to strip blocks it left behind.
func projectConfigPath(folder, agent string) string {
	switch agent {
	case "claude":
		return filepath.Join(folder, ".claude", "settings.json")
	case "codex":
		return filepath.Join(folder, ".codex", "hooks.json")
	case "gemini":
		return filepath.Join(folder, ".gemini", "settings.json")
	}
	return "" // hermes was always user-level
}

// Install registers the sync hooks for the given agents ("auto"/empty =
// every detected platform). Merging is idempotent and preserves whatever
// hooks the config already has.
func Install(folder string, agents []string) ([]Result, error) {
	if len(agents) == 0 || (len(agents) == 1 && agents[0] == "auto") {
		agents = Detect(folder)
	}
	var out []Result
	for _, name := range agents {
		p, ok := platforms[name]
		if !ok {
			return out, fmt.Errorf("unknown agent %q (supported: %s)", name, strings.Join(Agents, ", "))
		}
		path, changed, err := p.install(folder)
		if err != nil {
			return out, fmt.Errorf("%s: %w", name, err)
		}
		// Earlier versions wrote hooks into the project. Leaving those behind
		// would run every hook twice — double-counting agent reads in the
		// hub's heatmap — so installing also migrates them away.
		migrated, err := removeProjectHooks(folder, name)
		if err != nil {
			return out, fmt.Errorf("%s: %w", name, err)
		}
		out = append(out, Result{Agent: name, Path: path, Changed: changed, Note: p.note, Migrated: migrated})
	}
	return out, nil
}

// Uninstall removes BearDrive's hooks from each agent's user config, leaving
// every other hook in the file untouched.
func Uninstall(agents []string) ([]Result, error) {
	if len(agents) == 0 || (len(agents) == 1 && agents[0] == "auto") {
		agents = Agents
	}
	var out []Result
	for _, name := range agents {
		if _, ok := platforms[name]; !ok {
			return out, fmt.Errorf("unknown agent %q (supported: %s)", name, strings.Join(Agents, ", "))
		}
		path := ConfigPath("", name)
		changed, err := removeHooks(path, name == "hermes")
		if err != nil {
			return out, fmt.Errorf("%s: %w", name, err)
		}
		// The same residual Install migrates away. Uninstall used to touch only
		// the user config, so a user who upgraded and then asked for the hooks
		// to be removed was told "removed" while a `bdrive sync` kept firing on
		// every turn from a project-level registration this package knows how
		// to find.
		cwd, _ := os.Getwd()
		migrated, err := removeProjectHooks(cwd, name)
		if err != nil {
			return out, fmt.Errorf("%s: %w", name, err)
		}
		out = append(out, Result{Agent: name, Path: path, Changed: changed || migrated != "", Migrated: migrated})
	}
	return out, nil
}

// removeProjectHooks strips our blocks from every pre-user-scope location:
// earlier versions wrote the mount, its enclosing repo root, and the working
// directory init ran from.
func removeProjectHooks(folder, agent string) (string, error) {
	var cleaned []string
	user := ConfigPath("", agent)
	for _, dir := range legacyHookDirs(folder) {
		path := projectConfigPath(dir, agent)
		// The USER config is the project config of whatever directory it sits
		// in: when $HOME is a git repo (dotfiles) or init runs from $HOME, it
		// lands in legacyHookDirs and the "migration" would delete the hooks
		// this same Install call just wrote — silently, machine-wide.
		if path == "" || samePath(path, user) {
			continue
		}
		changed, err := removeHooks(path, false)
		if err != nil {
			return "", err
		}
		if changed {
			cleaned = append(cleaned, path)
		}
	}
	return strings.Join(cleaned, ", "), nil
}

// legacyHookDirs lists the directories a previous version may have written.
func legacyHookDirs(folder string) []string {
	dirs := []string{folder}
	if root := gitRootOf(folder); root != "" && root != folder {
		dirs = append(dirs, root)
	}
	if cwd, err := os.Getwd(); err == nil {
		if abs, err := filepath.Abs(cwd); err == nil && abs != folder {
			dirs = append(dirs, abs)
		}
	}
	return dirs
}

// gitRootOf is the repository containing folder, if any. The walk stops at
// $HOME: above it nothing is "this project's repo", and a dotfiles repo at
// $HOME would hand back the home directory itself — whose agent config is the
// user config, not a legacy project one.
func gitRootOf(folder string) string {
	home, _ := os.UserHomeDir()
	for cur := folder; ; cur = filepath.Dir(cur) {
		if _, err := os.Stat(filepath.Join(cur, ".git")); err == nil {
			return cur
		}
		if samePath(cur, home) || filepath.Dir(cur) == cur {
			return ""
		}
	}
}

// samePath reports whether two paths name the same file or directory even when
// they spell it differently. $HOME comes from the environment and spells
// /var/... on macOS while a folder resolved with filepath.Abs (or os.Getwd)
// spells the same place /private/var/... — a string compare misses that, and
// the two guards above then let the user config through as a "legacy project
// config" and delete the hooks Install just wrote.
func samePath(a, b string) bool {
	if a == b {
		return true
	}
	fa, err := os.Stat(a)
	if err != nil {
		return false
	}
	fb, err := os.Stat(b)
	if err != nil {
		return false
	}
	return os.SameFile(fa, fb)
}

// removeHooks deletes every hook group carrying one of our markers, and
// rewrites the file only if something was removed. A file we never touched is
// left byte-identical.
func removeHooks(path string, isYAML bool) (bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	root := map[string]any{}
	unmarshal, marshal := json.Unmarshal, func() ([]byte, error) { return json.MarshalIndent(root, "", "  ") }
	if isYAML {
		unmarshal, marshal = yaml.Unmarshal, func() ([]byte, error) { return yaml.Marshal(root) }
	}
	if err := unmarshal(data, &root); err != nil {
		return false, fmt.Errorf("parse %s: %w", path, err)
	}
	hooks, ok := root["hooks"].(map[string]any)
	if !ok {
		return false, nil
	}
	changed := false
	for event, v := range hooks {
		if !ourEvents[event] {
			continue
		}
		arr, _ := v.([]any)
		var kept []any
		for _, it := range arr {
			left, dropped := stripOwnHooks(it)
			changed = changed || dropped
			if left != nil {
				kept = append(kept, left)
			}
		}
		if len(kept) == 0 {
			delete(hooks, event)
			continue
		}
		hooks[event] = kept
	}
	if !changed {
		return false, nil
	}
	if len(hooks) == 0 {
		delete(root, "hooks")
	}
	return true, writeConfig(path, marshal)
}

// stripOwnHooks removes this package's own hooks from one hook group and
// returns what is left (nil when the whole group was ours), plus whether
// anything was dropped.
//
// Removal is per HOOK, not per group. The old rule judged the whole
// serialized group, so a group holding beardrive's command next to the user's
// — which is what anyone gets after tidying settings.json by hand — lost both,
// and the user's hook was collateral for a removal it was never part of.
//
// Hermes' YAML shape has no inner array (a group IS a command), so it is
// judged as a leaf.
func stripOwnHooks(group any) (any, bool) {
	m, ok := group.(map[string]any)
	if !ok {
		return group, false
	}
	inner, ok := m["hooks"].([]any)
	if !ok {
		if ownHook(m) {
			return nil, true
		}
		return group, false
	}
	var kept []any
	dropped := false
	for _, h := range inner {
		hm, ok := h.(map[string]any)
		if ok && ownHook(hm) {
			dropped = true
			continue
		}
		kept = append(kept, h)
	}
	if !dropped {
		return group, false
	}
	if len(kept) == 0 {
		return nil, true
	}
	m["hooks"] = kept
	return m, true
}

// ownHook reports whether one hook entry is one this package wrote. Only the
// command is read (never the whole serialized group), and only inside an event
// this package registers under — see ourEvents.
func ownHook(h map[string]any) bool {
	cmd, _ := h["command"].(string)
	return strings.Contains(cmd, marker) || strings.Contains(cmd, readMarker)
}

// mergeJSONHooks adds the pull + push + read hook trio to a Claude-style
// hooks JSON file (Claude, Codex, and Gemini all use this shape:
// hooks.<Event> is an array of {matcher?, hooks: [{type: "command", ...}]}
// groups). Push and read share the tool-use event under different matchers,
// each idempotent on its own marker.
func mergeJSONHooks(path, pullEvent, pushEvent, pushMatcher, readMatcher, label string, timeout int, async bool, pullCmd string) (string, bool, error) {
	root := map[string]any{}
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &root); err != nil {
			return path, false, fmt.Errorf("parse %s: %w", path, err)
		}
	} else if !os.IsNotExist(err) {
		return path, false, err
	}
	hooks, ok := root["hooks"].(map[string]any)
	if !ok {
		hooks = map[string]any{}
		root["hooks"] = hooks
	}
	cmd := hookCommand(label)
	if pullCmd == "" {
		pullCmd = cmd
	}
	pull := map[string]any{"hooks": []any{map[string]any{
		"type": "command", "command": pullCmd, "timeout": timeout,
		"statusMessage": "beardrive: pulling latest files",
	}}}
	pushHook := map[string]any{"type": "command", "command": cmd, "timeout": timeout}
	readHook := map[string]any{"type": "command", "command": readHookCommand(), "timeout": timeout}
	if async {
		pushHook["async"] = true
		readHook["async"] = true
	}
	push := map[string]any{"matcher": pushMatcher, "hooks": []any{pushHook}}
	read := map[string]any{"matcher": readMatcher, "hooks": []any{readHook}}

	changed := false
	for _, g := range []struct {
		event  string
		group  map[string]any
		marker string
	}{
		{pullEvent, pull, marker},
		{pushEvent, push, marker},
		{pushEvent, read, readMarker},
	} {
		arr, _ := hooks[g.event].([]any)
		if idx := indexOfMarkerGroup(arr, g.marker); idx >= 0 {
			// Already registered. These are OUR managed groups (marker-
			// identified): converge them to the current shape so command,
			// matcher, and flag improvements reach existing projects on
			// reinstall instead of being frozen by the idempotency check.
			if !jsonEqual(arr[idx], g.group) {
				arr[idx] = g.group
				hooks[g.event] = arr
				changed = true
			}
			continue
		}
		hooks[g.event] = append(arr, g.group)
		changed = true
	}
	if !changed {
		return path, false, nil
	}
	return path, true, writeConfig(path, func() ([]byte, error) {
		return json.MarshalIndent(root, "", "  ")
	})
}

// installHermes merges the hook pair into ~/.hermes/config.yaml
// (hooks.<event> is an array of {matcher?, command, timeout}).
func installHermes(string) (string, bool, error) {
	path := ConfigPath("", "hermes")
	root := map[string]any{}
	if data, err := os.ReadFile(path); err == nil {
		if err := yaml.Unmarshal(data, &root); err != nil {
			return path, false, fmt.Errorf("parse %s: %w", path, err)
		}
	} else if !os.IsNotExist(err) {
		return path, false, err
	}
	hooks, ok := root["hooks"].(map[string]any)
	if !ok {
		hooks = map[string]any{}
		root["hooks"] = hooks
	}
	cmd := hookCommand("hermes")
	groups := []struct {
		event  string
		group  map[string]any
		marker string
	}{
		{"pre_llm_call", map[string]any{"command": cmd, "timeout": 30}, marker},
		{"post_tool_call", map[string]any{"matcher": "write_file|patch", "command": cmd, "timeout": 30}, marker},
		{"post_tool_call", map[string]any{"matcher": "read_file|grep|bash", "command": readHookCommand(), "timeout": 30}, readMarker},
	}
	changed := false
	for _, g := range groups {
		arr, _ := hooks[g.event].([]any)
		if idx := indexOfMarkerGroup(arr, g.marker); idx >= 0 {
			if !jsonEqual(arr[idx], g.group) {
				arr[idx] = g.group
				hooks[g.event] = arr
				changed = true
			}
			continue
		}
		hooks[g.event] = append(arr, g.group)
		changed = true
	}
	if !changed {
		return path, false, nil
	}
	return path, true, writeConfig(path, func() ([]byte, error) {
		return yaml.Marshal(root)
	})
}

// containsMarker reports whether a hook array already holds the hook the
// marker identifies. Serializing sidesteps walking every platform's nesting
// by hand.
func containsMarker(v any, m string) bool {
	data, err := json.Marshal(v)
	return err == nil && strings.Contains(string(data), m)
}

// indexOfMarkerGroup returns the index of the hook group carrying the
// marker (so the group can be converged in place), or -1.
func indexOfMarkerGroup(arr []any, m string) int {
	for i, it := range arr {
		if grp, ok := it.(map[string]any); ok && containsMarker(grp, m) {
			return i
		}
	}
	return -1
}

// jsonEqual compares two values by canonical JSON (map keys sorted), so a
// group loaded from disk and a freshly-built one compare structurally.
func jsonEqual(a, b any) bool {
	da, err1 := json.Marshal(a)
	db, err2 := json.Marshal(b)
	return err1 == nil && err2 == nil && string(da) == string(db)
}

func writeConfig(path string, marshal func() ([]byte, error)) error {
	data, err := marshal()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	// Write THROUGH a symlink, never over it. ~/.claude/settings.json pointing
	// into a dotfiles repo is the normal shape for anyone who versions their
	// machine config, and WriteFileAtomic's rename replaced the link with a
	// regular file: the change landed somewhere the user does not deploy from,
	// every other machine sharing the repo kept the old config, and both
	// install and uninstall reported success. Reads already follow the link
	// (os.ReadFile), so only the write disagreed.
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	return store.WriteFileAtomic(path, append(data, '\n'), 0o644)
}

func dirExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}
