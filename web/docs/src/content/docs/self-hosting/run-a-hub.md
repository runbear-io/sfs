---
title: Run a hub
description: Self-host a BearDrive hub in about ten minutes — one static Go binary, one config file, any object store.
---

Self-hosting is a first-class, fully supported path forever: AGPL, no features
held back. One static Go binary, one config file, any object store (or a plain
directory).

## 1. Install the binary

```sh
brew install runbear-io/tap/beardrive     # macOS / Linuxbrew
# or: go install github.com/runbear-io/beardrive/cmd/bdrive@latest
# or: a release tarball — https://github.com/runbear-io/beardrive/releases
```

## 2. Pick storage

Any of `s3://bucket/prefix`, `gs://bucket/prefix`, an S3-compatible endpoint,
or — simplest for a first run — a plain directory
(`file:///var/lib/bdrive/storage`).

Files and change journals live there. Hub metadata (accounts, projects, orgs)
lives in the database you pick in step 3. Clients never see this storage; they
sync through the hub over HTTPS.

Credentials use each provider's standard chain — nothing BearDrive-specific:

| Storage | Credentials |
|---|---|
| `s3://bucket/prefix` | `AWS_PROFILE`, `~/.aws/credentials`, env vars, IAM roles. S3-compatible stores via `AWS_ENDPOINT_URL`. |
| `gs://bucket/prefix` | Application Default Credentials (`gcloud auth application-default login`) or `GOOGLE_APPLICATION_CREDENTIALS`. |
| `file:///path` | none — any local or network-mounted directory |

## 3. Write config.json

```jsonc
{
  "remote": "file:///var/lib/bdrive/storage",
  "addr": ":4173",
  "upload": true,
  "auth": {
    // Signup is invite-only by default — the safe posture for a public URL.
    // Your first account: start once with signup gated (or use an org
    // invite), then invite teammates from the web UI.
    "admins": ["you@example.com"],
    "users_db": "/var/lib/bdrive/auth.json"
  },
  "reads": { "enabled": true },              // agent read analytics (Dashboard)
  "database": { "driver": "sqlite", "dsn": "/var/lib/bdrive/hub.db" }
}
```

Every knob: [Hub config](/reference/hub-config/). Signup postures and SMTP:
[Authentication](/self-hosting/authentication/). Storage for metadata:
[Database](/self-hosting/database/).

## 4. Run it

```sh
bdrive serve -c config.json
```

Put TLS in front — Caddy, nginx, or your platform's load balancer. Device tokens
travel as bearer tokens.

:::caution[Don't let your proxy buffer the change stream]
The hub pushes "these paths changed" to browsers and devices over server-sent
events (`GET /api/p/<id>/events`), which is what makes a teammate's edit land in
about a second instead of on the next poll. A proxy that buffers responses holds
those frames until the connection closes, which turns the feature off without
any error appearing anywhere — sync still works, just at its old pace.

Caddy needs nothing. For nginx, turn buffering off and let the connection idle:

```nginx
location /api/ {
    proxy_pass http://127.0.0.1:4173;
    proxy_buffering off;
    proxy_cache off;
    proxy_read_timeout 1h;
    proxy_set_header Connection "";
    proxy_http_version 1.1;
}
```

The hub already sends `X-Accel-Buffering: no`, which nginx honours on its own,
so the block above is belt-and-braces for setups that override it. Cloudflare
does not buffer SSE. If you cannot control the proxy, nothing breaks: clients
fall back to polling on their normal intervals.
:::

For containers, the repository ships a `Dockerfile` (distroless, CGO-free) with
a Cloud Run recipe in
[`deploy/`](https://github.com/runbear-io/beardrive/tree/main/deploy). Any
container platform works the same way.

## 5. First sign-in and first project

1. Open `https://your-hub/` and create your account. Use the signup posture you
   configured; hub admins are the `admins` emails.
2. On any machine, `bdrive login https://your-hub`, then in the folder you want
   synced: `bdrive init --name wiki --yes` — or, inside a repository,
   `bdrive init docs --yes` to make that subfolder the project.
3. Invite a teammate: sidebar footer → **Manage** → **New invite**. The join
   link both creates their account and adds them to your org.
4. Connect agents: the project's home page shows one-paste setup for Claude
   Code, Hermes, and Codex, with the hub URL and project id already filled in.
   See [Set up with your agent](/start/setup/).

## How the hub is organized

In hub mode the server hosts many **projects** on one storage root. Each
project's data lives under its own prefix (`<root>/<project-id>/`), and a
registry maps project ids to names. Client devices sync whole folders through
the hub without ever knowing where the storage is or holding cloud credentials —
the server is the only machine configured with the bucket.

Projects are walled by **organization**. Every project belongs to one org, and
only that org's members — accounts with the `owner` or `member` role — can see,
browse, or sync it. Your first `bdrive init` creates an org automatically; an
owner invites teammates from the web UI (the org name in the sidebar footer →
Invite mints an expiring `/join/<token>` link). Public share links stay outside
the wall on purpose.

A hub upgraded from a pre-org version sweeps its existing projects into a
`default` org that all existing accounts join, so nothing breaks.

## Backup

Everything irreplaceable is in two places: the **storage root** (blobs and
journals — files and their entire history) and the **metadata database**
(accounts, projects, orgs, shares). Snapshot both; restore is copy-back.

## Upgrading

`brew upgrade beardrive`. Clients and hub are the same binary — keep them
roughly in step. The sync protocol is append-only journals plus blobs, which old
clients read forward.
