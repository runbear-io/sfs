package main

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/runbear-io/beardrive/internal/syncer"
)

// urlCmd prints a file's internal hub link — the viewer URL colleagues with
// project access open after signing in. The counterpart to `bdrive share`:
// share mints a PUBLIC link, url points at the permission-walled viewer.
func urlCmd() *cobra.Command {
	var doSync bool
	c := &cobra.Command{
		Use:   "url [path]",
		Short: "Print a file's internal hub link (sign-in + membership required to view)",
		Long: `Print the hub viewer URL for a file or folder in a bdrive project — the
link to hand teammates: it requires signing in to the hub and membership in
the project's organization, and always shows the latest synced content.
With no path (or "."), prints the project's home page URL.

This is the internal counterpart to "bdrive share": share mints a public
URL anyone can open; url points at the permission-walled viewer.

The link resolves once the file has synced — the daemon usually pushes
within seconds of saving; --sync pushes right now instead of waiting.

Computed locally from the folder's config; no network unless --sync.`,
		Example: `  bdrive url wiki/report.md
  bdrive url wiki/report.md --sync   # push first, so the link works immediately
  bdrive url wiki/                   # the folder's listing
  bdrive url                         # the project's home page`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target := "."
			if len(args) == 1 {
				target = args[0]
			}
			abs, err := filepath.Abs(target)
			if err != nil {
				return err
			}
			// findProject walks up from a directory; start at the target
			// itself only when it is one.
			start := filepath.Dir(abs)
			if fi, err := os.Stat(abs); err == nil && fi.IsDir() {
				start = abs
			}
			root, proj, err := findProject(start)
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(root, abs)
			if err != nil || strings.HasPrefix(rel, "..") {
				return fmt.Errorf("%s is outside the project at %s", abs, root)
			}
			rel = filepath.ToSlash(rel)
			if rel != "." {
				// A link to something the project doesn't sync would 404 for
				// everyone — refuse it here instead.
				filter, err := syncer.LoadFilter(root, proj.Include)
				if err != nil {
					return err
				}
				if filter.Skip(rel) {
					return fmt.Errorf("%s is not synced (ignored, or outside the project's shared scope)", rel)
				}
			}
			server, projectID, err := splitHubRemote(proj.Remote)
			if err != nil {
				return err
			}
			if doSync {
				sess, _, err := openSession(cmd.Context(), root, true)
				if err != nil {
					return err
				}
				defer closeSession(sess)
				res, err := sess.Cycle(cmd.Context())
				if err != nil {
					return err
				}
				// --sync exists so the link resolves immediately, so a refused
				// push breaks the one promise the flag makes. The link itself
				// is still the right URL — it just points at content the hub
				// does not have — so it goes to stdout as asked and the warning
				// goes to stderr, where a `bdrive url --sync > link.txt` still
				// shows it. Same silent-failure class as `bdrive sync` exiting
				// 0 on a refusal (BEA-183), one file over.
				if res.ReadOnly || res.NoAccess {
					fmt.Fprintf(cmd.ErrOrStderr(),
						"warning: the hub refused this device's changes, so this link will not resolve yet\n")
					if reason := res.Reason(); reason != "" {
						fmt.Fprintf(cmd.ErrOrStderr(), "         %s\n", safeField(reason, 300))
					}
				}
			}
			link := server + "/" + projectID
			if rel != "." {
				link += "/" + encodePathSegments(rel)
			}
			fmt.Fprintln(cmd.OutOrStdout(), link)
			return nil
		},
	}
	c.Flags().BoolVar(&doSync, "sync", false, "run a sync first so a just-created file is pushed and the link resolves immediately")
	return c
}

// encodePathSegments percent-encodes each path segment while keeping the
// "/" separators literal, matching the viewer's routing (no %2F).
func encodePathSegments(p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return strings.Join(segs, "/")
}
