package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/runbear-io/beardrive/internal/config"
	"github.com/runbear-io/beardrive/internal/secrets"
)

// shareCmd mints public URLs for synced files: `bdrive share report.html`
// prints a link anyone can open — no account needed. Links serve the file's
// latest synced content and live until revoked.
func shareCmd() *cobra.Command {
	var expires time.Duration
	var list bool
	var revoke string
	var force bool
	c := &cobra.Command{
		Use:   "share [file]",
		Short: "Share a synced file publicly by URL",
		Long: `Create a public link for a file in a bdrive project. Anyone with the URL
can view it — HTML renders as a page, markdown renders like the viewer,
PDFs open inline — with no account. The link always serves the file's
latest synced content and lives until revoked (use --expires to limit it).

The file must be inside an initialized project and already synced (the
daemon usually gets it there within seconds of saving).

Before minting, the hub reads the first 1 MiB of the file and refuses if it
finds credential-shaped strings — an AWS key, a private key block, a GitHub
or Slack or GitLab token. That check happens at the moment you share: a link
serves the file's latest content, so later changes are never re-checked.
Use --force to share anyway.`,
		Example: `  bdrive share wiki/report.html
  bdrive share deck.pdf --expires 168h    # link dies after a week
  bdrive share deploy.md --force          # share despite a credentials warning
  bdrive share --list                     # this project's links
  bdrive share --revoke <token-or-url>`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			settings, err := config.LoadSettings()
			if err != nil {
				return err
			}
			switch {
			case revoke != "":
				return revokeShare(settings, revoke)
			case list:
				return listShares(settings)
			case len(args) == 0:
				return fmt.Errorf("what to share? bdrive share <file> (or --list / --revoke)")
			}

			abs, err := filepath.Abs(args[0])
			if err != nil {
				return err
			}
			root, proj, err := findProject(filepath.Dir(abs))
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(root, abs)
			if err != nil || strings.HasPrefix(rel, "..") {
				return fmt.Errorf("%s is outside the project at %s", abs, root)
			}
			server, projectID, err := splitHubRemote(proj.Remote)
			if err != nil {
				return err
			}
			body := map[string]any{"path": filepath.ToSlash(rel)}
			if expires > 0 {
				body["expires_in"] = expires.String()
			}
			if force {
				body["confirm"] = true
			}
			data, _ := json.Marshal(body)
			resp, err := serverDo(http.MethodPost, server+"/api/p/"+projectID+"/shares", settings.Token, data)
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			if resp.StatusCode == http.StatusNotFound {
				return fmt.Errorf("%s (if you just saved it, wait a few seconds for the daemon or run `bdrive sync`)", strings.TrimSpace(readBody(resp)))
			}
			// The server writes this one as a sentence to be read (e.g. "share
			// links are per-file"); httpBodyError would prefix "400 Bad Request: ".
			if resp.StatusCode == http.StatusBadRequest {
				return fmt.Errorf("%s", strings.TrimSpace(readBody(resp)))
			}
			// Before the generic fallthrough: httpBodyError would print the raw
			// JSON, and this is the one status the user can act on.
			if resp.StatusCode == http.StatusConflict {
				return secretsFound(filepath.ToSlash(rel), resp)
			}
			if resp.StatusCode != http.StatusOK {
				return httpBodyError(resp)
			}
			var out struct {
				URL     string    `json:"url"`
				Expires time.Time `json:"expires"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
				return err
			}
			fmt.Println(out.URL)
			if !out.Expires.IsZero() {
				fmt.Printf("  expires: %s\n", out.Expires.Local().Format(time.RFC1123))
			}
			if u, err := url.Parse(out.URL); err == nil && isPrivateHost(u.Hostname()) {
				fmt.Fprintf(os.Stderr, "note: this link is only reachable where %s is (private address)\n", u.Hostname())
			}
			return nil
		},
	}
	c.Flags().BoolVar(&force, "force", false, "share it even if the file looks like it contains credentials")
	c.Flags().DurationVar(&expires, "expires", 0, "make the link expire (e.g. 24h, 168h); default: lives until revoked")
	c.Flags().BoolVar(&list, "list", false, "list this project's share links")
	c.Flags().StringVar(&revoke, "revoke", "", "revoke a share link (token or full URL)")
	return c
}

// secretsFound turns the hub's 409 into the message that stops the share.
// The wording is load-bearing: a link serves the file's LATEST content
// forever, so this says the file was checked *at the moment you shared it* —
// never that the file is clean.
func secretsFound(rel string, resp *http.Response) error {
	var out struct {
		Findings []struct {
			Rule string `json:"rule"`
			Line int    `json:"line"`
		} `json:"findings"`
	}
	// Not readBody: that does a single 256-byte Read and would truncate a
	// long findings list mid-JSON.
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || len(out.Findings) == 0 {
		return fmt.Errorf("%s looks like it contains credentials; nothing was shared (re-run with --force if that is intentional)", rel)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s looks like it contains credentials (checked at the moment you shared it):\n", rel)
	for _, f := range out.Findings {
		// Both: the web dialog has always said "an AWS access key" while the
		// terminal said "aws_access_key_id", and the id is what someone greps
		// for or quotes in a report.
		fmt.Fprintf(&b, "  line %-4d %s (%s)\n", f.Line, secrets.Label(f.Rule), f.Rule)
	}
	b.WriteString("Nothing was shared. Re-run with --force if that is intentional.")
	return fmt.Errorf("%s", b.String())
}

func listShares(settings config.Settings) error {
	root, proj, err := findProject(".")
	if err != nil {
		return err
	}
	server, projectID, err := splitHubRemote(proj.Remote)
	if err != nil {
		return err
	}
	resp, err := serverDo(http.MethodGet, server+"/api/p/"+projectID+"/shares", settings.Token, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return httpBodyError(resp)
	}
	var out struct {
		Shares []struct {
			Path    string    `json:"path"`
			URL     string    `json:"url"`
			Creator string    `json:"creator"`
			Expires time.Time `json:"expires"`
		} `json:"shares"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return err
	}
	if len(out.Shares) == 0 {
		fmt.Printf("no share links for %s (create one with `bdrive share <file>`)\n", root)
		return nil
	}
	for _, s := range out.Shares {
		line := fmt.Sprintf("%s  %s", s.URL, s.Path)
		if s.Creator != "" {
			line += "  (by " + s.Creator + ")"
		}
		if !s.Expires.IsZero() {
			line += "  expires " + s.Expires.Local().Format("2006-01-02 15:04")
		}
		fmt.Println(line)
	}
	return nil
}

func revokeShare(settings config.Settings, tokenOrURL string) error {
	token := tokenOrURL
	if i := strings.LastIndex(tokenOrURL, "/s/"); i >= 0 {
		token = strings.Trim(tokenOrURL[i+3:], "/")
	}
	_, proj, err := findProject(".")
	if err != nil {
		return err
	}
	server, _, err := splitHubRemote(proj.Remote)
	if err != nil {
		return err
	}
	resp, err := serverDo(http.MethodDelete, server+"/api/shares/"+url.PathEscape(token), settings.Token, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return httpBodyError(resp)
	}
	fmt.Println("revoked")
	return nil
}

// findProject walks up from dir to the folder holding .bdrive/config.json.
func findProject(dir string) (string, config.Project, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", config.Project{}, err
	}
	for cur := abs; ; cur = filepath.Dir(cur) {
		if proj, ok, err := config.LoadProject(cur); err != nil {
			return "", proj, err
		} else if ok {
			// keep the registry pointing at the right place
			config.ResolveMount(cur)
			return cur, proj, nil
		}
		if filepath.Dir(cur) == cur {
			return "", config.Project{}, fmt.Errorf("not inside a bdrive project (run `bdrive init` first)")
		}
	}
}

// Same loose id shape as remote/http.go: UUID today, legacy `p-xxxxxxxx` on
// older hubs — the hub validates, this only splits the URL.
var hubRemoteRe = regexp.MustCompile(`^(https?://[^/]+)/p/([A-Za-z0-9._-]{4,64})$`)

// splitHubRemote splits an https://host/p/<id> remote into server + project.
func splitHubRemote(remote string) (string, string, error) {
	m := hubRemoteRe.FindStringSubmatch(remote)
	if m == nil {
		return "", "", fmt.Errorf("this project does not sync through a bdrive server (remote %q); sharing needs a hub", remote)
	}
	return m[1], m[2], nil
}

func readBody(resp *http.Response) string {
	data := make([]byte, 256)
	n, _ := resp.Body.Read(data)
	return string(data[:n])
}

func isPrivateHost(host string) bool {
	if host == "localhost" || host == "127.0.0.1" || host == "::1" {
		return true
	}
	return strings.HasPrefix(host, "10.") || strings.HasPrefix(host, "192.168.") || strings.HasPrefix(host, "172.")
}
