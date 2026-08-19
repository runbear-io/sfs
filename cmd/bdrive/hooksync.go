package main

import (
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/runbear-io/beardrive/internal/secrets"
	"github.com/runbear-io/beardrive/internal/store"
)

// `bdrive sync --hook <label>` is the agent-hook flavor of sync, run by the
// Claude Code UserPromptSubmit hook at every turn start. It does three
// things: pulls (a normal cycle), stamps the session note so every change
// this turn is attributed to the agent session, and — the part that keeps
// agents current no matter how stale their skill copy is — emits the
// project's gated-link formula as additionalContext, so the agent can
// append a hub link to any synced file path it mentions.
//
// One run can cover several mounts (a repo root whose wiki/ and docs/ are
// separate projects, see syncTargets), and the hook's stdout contract is a
// single JSON object — so every mount's link goes into one context, keyed
// by the path prefix an agent sees. Emitting only the first mount's URL
// made agents hang one project's base URL on another project's paths.
//
// Everything is best-effort: a hook must never fail the turn, so every
// error path is a silent, successful exit.

// hookNoteTTL mirrors `bdrive sync --note-ttl`'s default: the daemon's own
// scans keep stamping this session's changes for a while.
const hookNoteTTL = 30 * time.Minute

// hookLink pairs the path prefix an agent writes with the hub URL that
// prefix maps to, and carries what this mount pulled in since the last turn.
type hookLink struct {
	prefix string // "wiki/", or "" when the hook ran at or inside the mount
	base   string // https://hub/<project-id>[/<the run folder's subpath>]
	sub    string // the run folder's mount-relative path, "" at or above the mount
	paths  []store.InboundEvent
	// secrets is what the last cycle that read these files found in them.
	// Read from the store rather than the Result for the same reason paths
	// is: the daemon usually scanned the agent's write seconds ago, so this
	// cycle's own scan sees an unchanged file and finds nothing.
	secrets map[string][]secrets.Finding
}

// hookChangedMax caps the changed-file list the turn pays for. Past it the
// tail is a count — the first cycle on a fresh mount materializes the whole
// project, and no turn should carry that.
const hookChangedMax = 20

// hookSessionID reads the platform's event JSON from stdin — once per run,
// since stdin can only be consumed once and the sync loop may cover several
// mounts.
func hookSessionID(cmd *cobra.Command) string {
	data, _ := io.ReadAll(io.LimitReader(cmd.InOrStdin(), 1<<20))
	return eventSessionID(data)
}

// eventSessionID is that parse over an already-read payload — `bdrive
// read-log` consumes the same stdin for its own reasons and tags every read
// it spools with the same id, which is what lets the hub join a run's reads
// to its writes.
func eventSessionID(data []byte) string {
	var event struct {
		SessionID string `json:"session_id"`
	}
	_ = json.Unmarshal(data, &event) // malformed input: just sync
	return event.SessionID
}

// hookSync is one mount's contribution to the turn: where its files live on
// the hub, and which of them moved since the last turn.
type hookSync struct {
	base    string
	paths   []store.InboundEvent
	secrets map[string][]secrets.Finding
}

// runHookSync syncs one mount and reports its hub base URL, if it has one,
// plus the peer changes waiting for this turn.
func runHookSync(cmd *cobra.Command, target, sessionID, label string) (hookSync, bool) {
	sess, proj, err := openSession(cmd.Context(), target, true)
	if err != nil {
		return hookSync{}, false // not a mount / no session: fast no-op
	}
	defer closeSession(sess)

	if sessionID != "" {
		note := label + " session " + sessionID
		if err := sess.Store.SaveNote(note, hookNoteTTL); err == nil {
			sess.Note = note
		}
		// The hook is the ONLY writer of Op.Session — `bdrive sync --note`
		// cannot reach it, which is what makes a run card's identity
		// un-forgeable. Unlike the note it is not persisted with a TTL: a
		// later daemon scan should not credit its own changes to a session
		// that has moved on.
		sess.SessionID = sessionID
	}

	// The pull. Offline is fine — the link formula below is still valid
	// for teammates who are online.
	if _, err := sess.Cycle(cmd.Context()); err != nil {
		return hookSync{}, false // never break the turn
	}
	// Drained after the cycle, not from its Result: in the ordinary case the
	// daemon materialized the peer's change seconds ago, so this cycle saw
	// nothing and the spool is where the record is. Errors are ignored — the
	// links matter more than the list.
	paths, _ := sess.Store.DrainInbound()
	// Not drained: findings are state, not events. They stand until the file
	// changes without them, so every turn sees the ones still true.
	found, _ := sess.Store.LoadSecrets(sess.MountID)

	server, projectID, err := splitHubRemote(proj.Remote)
	if err != nil {
		return hookSync{}, false // non-hub remote: nothing to link to
	}
	return hookSync{base: server + "/" + projectID, paths: paths, secrets: found}, true
}

// hookLinkFor places one mount relative to the folder the hook ran in.
// Agents write paths as they see them from that folder, so the mount's
// position there is what turns a path into a URL: a mount BELOW the folder
// contributes a prefix to strip, while a run INSIDE a mount contributes a
// subpath that belongs in the base instead (there is no prefix to strip —
// every path the agent writes is already inside the mount).
func hookLinkFor(folder, target, base string) hookLink {
	// resolvePath: registry paths and the run folder can name the same
	// directory through different symlinks (macOS /tmp).
	rel, err := filepath.Rel(resolvePath(folder), resolvePath(target))
	if err != nil {
		return hookLink{base: base}
	}
	rel = filepath.ToSlash(rel)
	switch {
	case rel == ".":
		return hookLink{base: base}
	case rel == ".." || strings.HasPrefix(rel, "../"):
		sub, err := filepath.Rel(resolvePath(target), resolvePath(folder))
		if err != nil {
			return hookLink{base: base}
		}
		return hookLink{
			base: base + "/" + encodePathSegments(filepath.ToSlash(sub)),
			sub:  filepath.ToSlash(sub),
		}
	default:
		return hookLink{prefix: rel + "/", base: base}
	}
}

// emitHookContext writes the turn's additionalContext — one JSON object, no
// matter how many mounts the run covered.
func emitHookContext(cmd *cobra.Command, links []hookLink) {
	if len(links) == 0 {
		return
	}

	// Shared tail: what the links mean and when NOT to use them.
	const tail = "These links require hub sign-in + project membership, so they are safe to paste anywhere internal. " +
		"Only link files that actually sync (inside the shared scope, not ignored); keep paths inside code blocks or commands plain; give a raw URL only when the user needs to paste it outside this conversation. " +
		"`bdrive share <file>` mints PUBLIC no-account links — use it only when the user explicitly asks for a public link."

	var context string
	if len(links) == 1 && links[0].prefix == "" {
		// The common case: one project, paths already relative to its root.
		// Kept as short as possible — this is paid on every turn.
		b := links[0].base
		context = fmt.Sprintf(
			"beardrive: this folder syncs to %s (the project's hub page; files are at %s/<url-encoded path>). "+
				"Link convention: whenever you mention a synced file's path in prose, append its gated hub link on an emoji, formatted exactly as: `<path>` [🔗](%s/<url-encoded path>) — the path stays plain text, the hyperlink goes on the emoji only. "+
				"The URL path is the file's path relative to this folder, with each segment percent-encoded and the `/` separators left literal. "+
				tail, b, b, b)
	} else {
		parts := make([]string, len(links))
		for i, l := range links {
			p := l.prefix
			if p == "" {
				p = "./"
			}
			parts[i] = fmt.Sprintf("`%s` → %s", p, l.base)
		}
		context = fmt.Sprintf(
			"beardrive: hub URLs for the synced folders here — %s. "+
				"Link convention: whenever you mention a synced file's path in prose, append its gated hub link on an emoji, formatted exactly as: `<path>` [🔗](<that folder's URL>/<url-encoded path within it>) — the path stays plain text, the hyperlink goes on the emoji only. "+
				"Pick the folder whose prefix above matches the path longest, strip that prefix, then percent-encode each remaining segment and leave the `/` separators literal. A path matching none of these folders is not synced — do not link it, and never hang one folder's path on another folder's URL. "+
				tail, strings.Join(parts, ", "))
	}

	if changed := hookChanged(links); changed != "" {
		context += " " + changed
	}
	if found := hookSecrets(links); found != "" {
		context += " " + found
	}

	out := map[string]any{
		"hookSpecificOutput": map[string]any{
			"hookEventName":     "UserPromptSubmit",
			"additionalContext": context,
		},
	}
	enc, err := json.Marshal(out)
	if err != nil {
		return
	}
	fmt.Fprintln(cmd.OutOrStdout(), string(enc))
}

// hookChanged renders what teammates' devices pulled in since the last turn:
// the whole point of the spool, and the only defense an agent has against
// rewriting a file that moved underneath it. Advisory — nothing blocks.
//
// Each path is translated into what the agent sees from the folder the hook
// ran in, using the same placement hookLinkFor computed for the links: a
// mount below that folder prepends its prefix, a run inside a mount strips
// its own subpath (and paths outside it are not the agent's to re-read).
func hookChanged(links []hookLink) string {
	var paths []string
	over := 0
	for _, l := range links {
		for _, e := range l.paths {
			p, ok := hookAgentPath(l, e.Path)
			if !ok {
				continue
			}
			if len(paths) >= hookChangedMax {
				over++
				continue
			}
			if e.Deleted {
				p += " (deleted)"
			}
			paths = append(paths, "`"+p+"`")
		}
	}
	if len(paths) == 0 {
		return ""
	}
	s := "Changed since your last turn by a teammate or another device — re-read before editing: " + strings.Join(paths, ", ")
	if over > 0 {
		s += fmt.Sprintf(", +%d more", over)
	}
	return s + "."
}

// hookAgentPath maps one mount-relative spool path to the path an agent
// writes, reporting false for paths the agent cannot reach from here.
func hookAgentPath(l hookLink, path string) (string, bool) {
	switch {
	case l.prefix != "":
		return l.prefix + path, true
	case l.sub != "":
		// The session runs inside the mount: its own subpath is implicit in
		// every path it writes, so strip it — and a sibling folder's file is
		// outside this session's view entirely.
		rest, ok := strings.CutPrefix(path, l.sub+"/")
		return rest, ok
	default:
		return path, true
	}
}

// hookSecrets tells the turn which synced files looked like they hold
// credentials when they last changed. Advisory, exactly like hookChanged:
// they have ALREADY synced, so there is nothing to block — the useful move is
// telling the user, which is something an agent can do and a status line the
// user never runs cannot. Rule labels and line numbers only; the matched
// bytes never leave internal/secrets.
func hookSecrets(links []hookLink) string {
	var parts []string
	over := 0
	for _, l := range links {
		for _, rel := range slices.Sorted(maps.Keys(l.secrets)) {
			p, ok := hookAgentPath(l, rel)
			if !ok {
				continue
			}
			if len(parts) >= hookChangedMax {
				over++
				continue
			}
			f := l.secrets[rel][0]
			parts = append(parts, fmt.Sprintf("`%s` (%s, line %d)", p, secrets.Label(f.Rule), f.Line))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	s := "These synced files looked like they contain credentials when they last changed: " + strings.Join(parts, ", ")
	if over > 0 {
		s += fmt.Sprintf(", +%d more", over)
	}
	return s + ". They have already synced to the hub and to teammates, so this is not a blocker to work around — tell the user, and suggest rotating the credential and keeping it out of the folder."
}
