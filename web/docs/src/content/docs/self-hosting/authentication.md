---
title: Authentication
description: The three signup postures, admin controls, device sign-in, and SMTP for a self-hosted hub.
---

Hubs always require sign-in — every change is attributed to a real account. The
whole API (web UI, uploads, project creation, device sync) needs a session; only
`/api/config` and the auth pages stay open. The plain-folder viewer,
`bdrive serve ./folder`, remains auth-free.

Accounts are email, password, and name, kept in a file-backed registry
(`auth.json`), atomically rewritten. No plaintext credentials are stored on the
server — passwords are bcrypt-hashed and tokens are kept as SHA-256 digests. On
a client device, the sync token is stored at `~/.bdrive/settings.json` with
`0600` permissions and can be revoked from that device with `bdrive logout`.

## Signup is invite-only by default

This is the safe posture for a hub on a public URL. New people get in only
through an expiring invite link an owner mints; the link lets them create an
account — bypassing the gates below — and join, in one step.

An owner who mints the link from a project's **Settings → People** ("Invite a
teammate") gets one scoped to that project: the newcomer lands straight on that
project's install page, with the agent paste prompt already naming this hub and
this project, instead of on a list of projects with nothing saying which one
they were invited for. A link minted from Organization settings still joins the
org and lands on the normal home view.

To allow self-service signup instead, set `"allow_signup": true` **with a gate**.
The server refuses to start an open hub that has none, so a fake email can never
just walk in.

### The three postures

- **Invite-only** (default) — `allow_signup` unset or false. Only invite links
  create accounts.
- **Approval-gated** — `allow_signup: true` + `require_approval: true`. Anyone
  can sign up, but a hub admin approves each new account before it works. No
  SMTP needed.
- **Domain-restricted and verified** — `allow_signup: true` +
  `allowed_domains: ["you.com"]` + `require_verification: true`, which needs
  `smtp`. Only your company's addresses may sign up, each confirming an emailed
  link.

Verification without SMTP is refused at startup — the link would otherwise only
reach the server log.

## Config

```jsonc
"auth": {
  "allow_signup": true,
  "allowed_domains": ["example.com"],
  "require_approval": true,
  "require_verification": true,
  "users_db": "/var/lib/bdrive/auth.json",
  "admins": ["admin@example.com"],
  "smtp": { "host": "smtp.example.com", "port": 587,
            "user": "drive@example.com", "pass": "…", "from": "drive@example.com" }
}
```

Admins tune verification and approval live from the web UI (**Admin → Signup &
access**). `allowed_domains`, the admin list, and `allow_signup` are
server-config-owned, so a browser session can never widen who gets in.

## Device sign-in

Both sign-in flows end the same way: **an approval page you have to click.** It
names the account the machine would act as, because that is what approving
grants — whichever account the browser is signed in as is the one the terminal
becomes, and it is often not the one you meant (a personal login left open, a
teammate's session on a shared machine). **Switch account** signs you out and
returns to the same pending sign-in, so picking the right one costs a click
rather than a re-run. Approval is a POST from that page, so merely opening a
sign-in link grants nothing.

`bdrive login <url>` opens the server's sign-in page in a browser — sign up
right there if needed — and lands on that approval page. Approving bounces a
one-time code to the CLI's loopback listener and the terminal finishes on its
own, storing a long-lived per-device token that is revocable server-side.

On headless or SSH machines login falls back to the device-code flow
automatically (no TTY, or no browser can open): it prints one approval link to
open in any signed-in browser. The link carries the request, so there is no code
to retype; the page it opens names the account you would be granting, the device
asking, its OS, and the address it came from, so an unexpected approval request
is visible rather than anonymous. Approving is a POST from that page — a link
alone can't grant. `bdrive login --device` forces that flow.

Every sync and every `bdrive init` then authenticates with that token. The hub's
device registry records per-device name, OS, account, and the IP the server
observed. History shows the device name and OS; the IP stays in the registry
and is never reported to project members.

## Password reset

"Forgot password" emails a one-hour reset link via the `auth.smtp` block. Plain
SMTP, so any provider works. With no SMTP configured the link is printed to the
server log so an admin can hand it over — reset is never fully broken.

## TLS

Put a hub behind TLS, via reverse proxy or tailscale. `bdrive login` warns when
signing in over plain http to a non-localhost address.

## Swapping the provider

Internally all of this sits behind an `AuthProvider` interface. The open-source
server ships the built-in email/password provider; alternative identity backends
can be swapped in without touching the CLI or the API.
