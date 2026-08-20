package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/runbear-io/beardrive/internal/config"
)

// The browser login flow: the CLI listens on a random loopback port, opens
// the server's sign-in page with ?redirect= pointing back at that port, and
// the page bounces a one-time code to us once the user is signed in (they
// can sign up right there). We exchange the code for a long-lived device
// token. Headless machines use the code flow instead: show a short code,
// the user approves it at /auth/device from any browser, we poll.

// loginCmd signs this device in to a bdrive web server and remembers it.
// Bare `bdrive login` uses the remembered server, or beardrive.ai.
func loginCmd() *cobra.Command {
	var useDevice, status bool
	c := &cobra.Command{
		Use:   "login [server-url]",
		Short: "Sign this device in to a bdrive web server",
		Long: `Sign this device in to a bdrive web server and remember it (settings.json
under the bdrive home); every later "bdrive init" uses it.

If the server requires accounts, login opens its sign-in page in a browser
(sign up right there if you don't have an account); once you sign in, the
terminal finishes automatically. On headless machines, or with --device,
you are shown a short code to approve from any browser instead.

With no argument the remembered server is used, or ` + config.DefaultServer + `.`,
		Example: `  bdrive login                       # remembered server, or beardrive.ai
  bdrive login https://drive.example.com:4173
  bdrive login --device              # no local browser (SSH)
  bdrive login --status              # show server + signed-in account`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			settings, err := config.LoadSettings()
			if err != nil {
				return err
			}
			if status {
				if settings.Server == "" {
					return fmt.Errorf("no server configured; run `bdrive login`")
				}
				fmt.Println(settings.Server)
				if settings.Token != "" {
					if u, err := whoAmIOnServer(settings.Server, settings.Token); err == nil {
						// The hub chose both of these; they reach a terminal.
						fmt.Printf("signed in as %s <%s>\n", safeField(u.Name, 64), safeField(u.Email, 64))
					} else {
						fmt.Println("token no longer valid — run `bdrive login` again")
					}
				}
				return nil
			}
			server := settings.Server
			if len(args) > 0 {
				server = strings.TrimSuffix(args[0], "/")
			}
			if server == "" {
				server = config.DefaultServer
			}
			u, err := url.Parse(server)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				return fmt.Errorf("invalid server URL %q (want https://host:port)", server)
			}
			cfg, err := fetchServerConfig(server)
			if err != nil {
				return fmt.Errorf("cannot reach bdrive server at %s: %w", server, err)
			}
			if cfg.Mode != "hub" {
				fmt.Println("note: this server serves a single folder (no projects); `bdrive init` needs a hub (a server started on a storage root)")
			}
			if !cfg.Auth.Enabled {
				settings = config.Settings{Server: server}
				if err := config.SaveSettings(settings); err != nil {
					return err
				}
				fmt.Printf("logged in to %s (no sign-in required by this server)\n", server)
				return nil
			}
			return runLogin(server, cfg, useDevice)
		},
	}
	c.Flags().BoolVar(&useDevice, "device", false, "use the code flow instead of a local browser (SSH/headless)")
	c.Flags().BoolVar(&status, "status", false, "show the current server and signed-in account")
	return c
}

// logoutNote is printed after every logout. It must stay honest: there is
// still no device-list page and device tokens carry no expiry (see
// internal/webapp/authlocal.go authToken), and a daemon already running keeps
// the copy it started with until it exits. login_test.go guards the wording.
const logoutNote = "note: a sync daemon already running keeps its own copy of the token until it exits (`bdrive stop`)"

// revokeOnServer ends this device's token on the hub. The token authenticates
// its own revocation, so signing out needs nothing else.
func revokeOnServer(server, token string) error {
	req, err := http.NewRequest(http.MethodDelete, server+"/api/auth/token", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := initClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return nil // the hub does not know this credential: already ended
	}
	if resp.StatusCode != http.StatusOK {
		return httpBodyError(resp)
	}
	return nil
}

func logoutCmd() *cobra.Command {
	var forget bool
	c := &cobra.Command{
		Use:   "logout",
		Short: "Sign this device out (clear the saved token)",
		Long: `Clear this device's saved sign-in — the token and account — so it is no
longer authenticated to the bdrive server. The remembered server is kept so
"bdrive login" re-authenticates to it; pass --forget to clear that too.

To switch to a different server, just run "bdrive login <new-server-url>".
Your synced folders are untouched; this only affects this device's session.`,
		Example: `  bdrive logout            # sign out, keep the server remembered
  bdrive logout --forget   # sign out and forget the server`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			settings, err := config.LoadSettings()
			if err != nil {
				return err
			}
			if settings.Token == "" && settings.Email == "" && !(forget && settings.Server != "") {
				fmt.Println("already signed out")
				return nil
			}
			who, server := settings.Email, settings.Server
			// End it on the hub FIRST, and say so when that fails: a token
			// cleared only locally is a live credential with nobody watching
			// it — the case an operator reaches for this command in (a lost
			// laptop, a token in a log) is exactly the one where the local
			// file no longer matters.
			if settings.Token != "" && server != "" {
				if err := revokeOnServer(server, settings.Token); err != nil {
					fmt.Printf("warning: could not end this token on %s: %v\n", server, err)
					fmt.Println("         it is cleared locally but the server may still accept it; run `bdrive logout` again when the server is reachable")
				}
			}
			settings.Token, settings.Email, settings.Name = "", "", ""
			if forget {
				settings.Server = ""
			}
			if err := config.SaveSettings(settings); err != nil {
				return err
			}
			switch {
			case who != "" && server != "" && !forget:
				fmt.Printf("signed out %s from %s\n", who, server)
			case who != "":
				fmt.Printf("signed out %s\n", who)
			default:
				fmt.Println("signed out")
			}
			if forget {
				fmt.Println("forgot the remembered server (run `bdrive login <url>` to set a new one)")
			}
			fmt.Println(logoutNote)
			return nil
		},
	}
	c.Flags().BoolVar(&forget, "forget", false, "also forget the remembered server")
	return c
}

// runLogin executes the sign-in flow against a server known to require auth
// and persists server + token + account to settings.
func runLogin(server string, cfg serverConfig, useDevice bool) error {
	// Here, not in loginCmd's RunE. The warning used to sit above this function
	// and only `bdrive login` reached it, while `bdrive init --server <url>`
	// came through ensureLogin straight into this same credential exchange and
	// said nothing — and INSTALL_FOR_AGENTS.md step 2 is titled "Do not run a
	// login command", so every onboarding agent was routed onto the silent
	// path. One sign-in door, one warning.
	if u, err := url.Parse(server); err == nil && u.Scheme == "http" &&
		u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost" && u.Hostname() != "::1" {
		fmt.Println("warning: signing in over plain http — credentials travel unencrypted; prefer https (reverse proxy or tailscale)")
	}
	loginPath := cfg.Auth.CLILogin
	if loginPath == "" {
		loginPath = "/auth/cli"
	}
	// Headless shells (agents, CI, SSH) can't complete the loopback-callback
	// flow — the browser would open nowhere and the CLI would hang. Fall back
	// to the device-code flow automatically instead of waiting.
	if !useDevice && !stdinIsTTY() {
		fmt.Println("no interactive terminal detected — using the device-code sign-in flow")
		useDevice = true
	}
	var token string
	var user serverUser
	var err error
	if useDevice {
		token, user, err = deviceCodeLogin(server)
	} else {
		token, user, err = browserLogin(server, loginPath)
		if errors.Is(err, errNoBrowser) {
			fmt.Println("could not open a browser — switching to the device-code sign-in flow")
			token, user, err = deviceCodeLogin(server)
		}
	}
	if err != nil {
		return err
	}
	settings := config.Settings{Server: server, Token: token, Email: user.Email, Name: user.Name}
	if err := config.SaveSettings(settings); err != nil {
		return err
	}
	fmt.Printf("logged in to %s as %s <%s>\n", server, safeField(user.Name, 64), safeField(user.Email, 64))
	return nil
}

type serverUser struct {
	ID    string `json:"id"`
	Email string `json:"email"`
	Name  string `json:"name"`
}

type serverConfig struct {
	Mode string `json:"mode"`
	Auth struct {
		Enabled  bool   `json:"enabled"`
		CLILogin string `json:"cli_login"`
	} `json:"auth"`
}

func fetchServerConfig(server string) (serverConfig, error) {
	var cfg serverConfig
	resp, err := initClient.Get(server + "/api/config")
	if err != nil {
		return cfg, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return cfg, httpBodyError(resp)
	}
	if err := json.NewDecoder(resp.Body).Decode(&cfg); err != nil || cfg.Mode == "" {
		return cfg, fmt.Errorf("not a bdrive server (bad /api/config)")
	}
	return cfg, nil
}

func whoAmIOnServer(server, token string) (serverUser, error) {
	var u serverUser
	// Through serverDo like every other token-carrying CLI call, so the origin
	// binding lives in exactly one place.
	resp, err := serverDo(http.MethodGet, server+"/api/auth/me", token, nil)
	if err != nil {
		return u, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return u, httpBodyError(resp)
	}
	err = json.NewDecoder(resp.Body).Decode(&u)
	return u, err
}

// postAsDevice posts a login-flow request carrying this machine's device
// identity. The hub binds that id to the account when it mints the token
// (webapp.DeviceRegistry.Bind), and that binding is the ONLY thing that makes
// the id this account's — so every mint point has to send it, not just the one
// a fix happens to name: the loopback browser flow, the device-code flow, and
// the login `bdrive init` runs inside itself all route through here.
func postAsDevice(url string, body []byte) (*http.Response, error) {
	req, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if dev, err := config.LoadDevice(); err == nil && dev.ID != "" {
		req.Header.Set("X-Bdrive-Device", dev.ID)
		req.Header.Set("X-Bdrive-Device-Name", dev.Name)
		req.Header.Set("X-Bdrive-Os", runtime.GOOS+"/"+runtime.GOARCH)
	}
	return initClient.Do(req)
}

func deviceName() string {
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "cli"
}

// errNoBrowser signals that the loopback flow can't proceed because no local
// browser could be opened; the caller falls back to the device-code flow.
var errNoBrowser = errors.New("no local browser")

// browserLogin runs the loopback-callback flow.
func browserLogin(server, loginPath string) (string, serverUser, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", serverUser{}, err
	}
	defer ln.Close()

	var stateBuf [16]byte
	rand.Read(stateBuf[:])
	state := hex.EncodeToString(stateBuf[:])
	// PKCE (RFC 7636, and RFC 8252 for exactly this loopback flow). `state`
	// binds nothing: it is printed to stdout and handed to `open`/`xdg-open`
	// as argv[1], so every local account can read it with `ps` — and with it
	// and the listener port, any local process can walk a sign-in of ITS OWN
	// account into this CLI's callback, after which the user's folders sync
	// into somebody else's project. The verifier never leaves this process,
	// so a code minted for any other flow cannot be redeemed here.
	var verifierBuf [32]byte
	rand.Read(verifierBuf[:])
	verifier := hex.EncodeToString(verifierBuf[:])
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	redirect := fmt.Sprintf("http://%s/callback", ln.Addr().String())
	loginURL := fmt.Sprintf("%s%s?redirect=%s&state=%s&code_challenge=%s&code_challenge_method=S256",
		server, loginPath, redirect, state, challenge)

	codeCh := make(chan string, 1)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/callback" || r.URL.Query().Get("state") != state {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<!doctype html><body style="font-family:sans-serif;background:#1e1e24;color:#ddd;text-align:center;padding-top:20vh">
<h2>Signed in ✓</h2><p>You can close this tab and return to the terminal.</p></body>`)
		select {
		case codeCh <- r.URL.Query().Get("code"):
		default:
		}
	})}
	go srv.Serve(ln)
	defer srv.Shutdown(context.Background())

	fmt.Println("opening your browser to sign in (sign up there if you don't have an account):")
	// The path came from the hub's /api/config — the first thing a server we
	// have never talked to gets to print on this machine.
	fmt.Println("  " + safeField(loginURL, 512))
	if err := openBrowser(loginURL); err != nil {
		// The loopback callback only works from a browser on this machine, so
		// a URL the user pastes elsewhere would dead-end — use the code flow.
		return "", serverUser{}, errNoBrowser
	}
	fmt.Println("waiting for you to approve the sign-in in your browser… (no browser here? Ctrl-C and run `bdrive login --device`)")

	select {
	case code := <-codeCh:
		if code == "" {
			return "", serverUser{}, fmt.Errorf("sign-in was rejected")
		}
		return exchangeCode(server, code, verifier)
	case <-time.After(5 * time.Minute):
		return "", serverUser{}, errors.New("timed out waiting for the browser sign-in (try `bdrive login --device`)")
	}
}

func exchangeCode(server, code, verifier string) (string, serverUser, error) {
	body, _ := json.Marshal(map[string]string{
		"code": code, "device": deviceName(), "code_verifier": verifier,
	})
	resp, err := postAsDevice(server+"/api/auth/exchange", body)
	if err != nil {
		return "", serverUser{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", serverUser{}, httpBodyError(resp)
	}
	var out struct {
		Token string     `json:"token"`
		User  serverUser `json:"user"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", serverUser{}, err
	}
	return out.Token, out.User, nil
}

// deviceCodeLogin runs the headless flow: print one approval link, poll until
// the user opens it in a signed-in browser. The link carries the secret, so
// there is no code to read off this screen and type into another — and the
// page it opens names this device, so the approver can see what they're
// approving.
func deviceCodeLogin(server string) (string, serverUser, error) {
	body, _ := json.Marshal(map[string]string{"device": deviceName(), "os": runtime.GOOS})
	resp, err := postAsDevice(server+"/api/auth/device/start", body)
	if err != nil {
		return "", serverUser{}, err
	}
	var start struct {
		Code      string `json:"code"`
		VerifyURL string `json:"verify_url"`
		Interval  int    `json:"interval"`
	}
	err = json.NewDecoder(resp.Body).Decode(&start)
	resp.Body.Close()
	if err != nil || start.Code == "" {
		return "", serverUser{}, fmt.Errorf("server did not start a device login: %v", err)
	}
	if start.Interval <= 0 {
		start.Interval = 2
	}
	// Older hubs (pre-0.13) hand back a short code and expect it typed into
	// /auth/device; keep that instruction for them.
	// The code and the link are the hub's strings, printed to a terminal.
	if start.VerifyURL == "" {
		fmt.Printf("on any signed-in browser, open:\n  %s/auth/device\nand approve code: %s\n", server, safeField(start.Code, 64))
	} else {
		fmt.Printf("to finish signing in, open this link in any browser:\n  %s\n", safeField(sameOriginLink(server, start.VerifyURL), 300))
	}

	deadline := time.Now().Add(10 * time.Minute)
	for time.Now().Before(deadline) {
		time.Sleep(time.Duration(start.Interval) * time.Second)
		body, _ := json.Marshal(map[string]string{"code": start.Code, "device": deviceName()})
		resp, err := postAsDevice(server+"/api/auth/device/poll", body)
		if err != nil {
			continue // transient; keep polling
		}
		if resp.StatusCode == http.StatusUnauthorized {
			resp.Body.Close()
			return "", serverUser{}, errors.New("the code expired before it was approved")
		}
		var out struct {
			Pending bool       `json:"pending"`
			Token   string     `json:"token"`
			User    serverUser `json:"user"`
		}
		err = json.NewDecoder(resp.Body).Decode(&out)
		resp.Body.Close()
		if err != nil || out.Pending {
			continue
		}
		if out.Token != "" {
			return out.Token, out.User, nil
		}
	}
	return "", serverUser{}, errors.New("timed out waiting for approval")
}

// sameOriginLink returns the hub's chosen sign-in link if it lives on the hub
// being signed in to, and the hub's own /auth/device otherwise.
//
// safeField scrubs control characters and truncates; it never looked at the
// ORIGIN, so the hub could point the person at any host it liked — and the
// sentence framing it ("to finish signing in, open this link in any browser")
// comes from the trusted local CLI, not from the hub. The runbook makes init
// non-interactive, so this is the DEFAULT path, and its step 5 has the agent
// hand init's output to the user: a credential-harvesting page reaches a human
// relayed by their own agent, in the CLI's voice.
//
// Falling back rather than failing: the link is a convenience over a page the
// hub always serves, so a hub that names someone else's host loses the
// convenience and the sign-in still completes.
func sameOriginLink(server, link string) string {
	fallback := strings.TrimSuffix(server, "/") + "/auth/device"
	su, err := url.Parse(server)
	if err != nil {
		return fallback
	}
	lu, err := url.Parse(link)
	if err != nil || lu.Scheme != su.Scheme || lu.Host != su.Host {
		return fallback
	}
	return link
}

// openBrowser opens url in the user's default browser. A var so the desktop
// login test can stand in for the user completing the sign-in.
var openBrowser = func(url string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", url).Start()
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	default:
		return exec.Command("xdg-open", url).Start()
	}
}
