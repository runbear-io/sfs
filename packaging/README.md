# Self-hosted app-store packaging (Umbrel / CasaOS / TrueNAS)

Thin, per-store manifests that wrap the repo's existing hub image (built from
the root [`Dockerfile`](../Dockerfile) — distroless, CGO-free) so a BearDrive
**hub** installs as a one-click app in the self-hosted app-store catalogs.
None of these change the core image.

| Dir | Store | Upstream PR target | Status |
|---|---|---|---|
| [`umbrel/`](umbrel/)   | Umbrel App Store   | `getumbrel/umbrel-apps`        | **DRAFT — blocked** (see below) |
| [`casaos/`](casaos/)   | CasaOS App Store   | `IceWhaleTech/CasaOS-AppStore` | **DRAFT — blocked** |
| [`truenas/`](truenas/) | TrueNAS community  | `truenas/apps`                 | **DRAFT — blocked** |

> `packaging/homebrew` is the existing sibling — the CLI distribution manifest.

## ⚠️ Two blockers before any of these can be PR'd upstream

These manifests are **NOT yet known-good**. Both stores' contributing guides
require a real-instance install test, and all three reference a published
container image that does not exist yet.

1. **No published container image.** Every store installs by pulling a public,
   versioned, multi-arch image by tag. The repo publishes **none** — CI
   (`.github/workflows/{ci,bump-cloud,docs}.yml`) has no image-publish job, and
   the only build path is `deploy/gcp-cloudrun.sh` → a **private** GCP Artifact
   Registry for Cloud Run. The `image:` fields below point at a **placeholder**
   `ghcr.io/runbear-io/beardrive:<version>` that must first be built and pushed
   (multi-arch `linux/amd64` + `linux/arm64` — Umbrel runs on Raspberry Pi).
   Owner: **CTO/eng** (tracked as the child issue filed off BEA-49).

2. **No real-instance test environment.** Each store's guide requires the app
   be installed + reach its web UI on an actual instance of that platform
   (Umbrel OS / CasaOS / TrueNAS SCALE) before the PR is accepted. Eng has
   Docker (the underlying image is smoke-tested — see below) but **no
   Umbrel/CasaOS/TrueNAS instances and no local VM tooling**
   (qemu/vagrant/multipass absent; Docker Desktop on macOS can't practically
   nest those OSes). Owner: **CEO** — needs cloud-VM budget/credentials or a
   hosted test env. Untested PRs get rejected and burn reviewer goodwill, so
   we hold rather than ship blind (per BEA-49's "Known risk" instruction).

## What IS verified (Docker smoke test, not a store instance)

The underlying hub image was built from the root `Dockerfile` and run locally
under Docker with the self-host config below; the web UI was confirmed
reachable. This validates the **runtime invocation and defaults** the manifests
encode — it does **not** substitute for the per-store real-instance test.

## Install defaults chosen (for the listing copy — CMO)

The hub is configured by flags **plus** a JSON config file. Critically, the
**admin email can only be set via the config file's `auth.admins`** — there is
no `--admins` flag — and the image is **distroless (no shell)**, so each
manifest uses a tiny `busybox` init container to render `/data/config.json`
from the user-supplied admin email into the shared data volume before the hub
starts. The core image is untouched.

| Setting | Value | Notes |
|---|---|---|
| **Admin email** | user-supplied; placeholder `admin@example.com` | `auth.admins`. On a zero-account hub this email can self-sign-up first (invite-only afterward). |
| **Listen port (container)** | `8080` (`--addr :8080`) | Suggested host/web port `4173` (matches `docs/self-hosting.md`). |
| **Storage backend** | `file:///data/storage` | Local filesystem blob+journal store — the simplest self-host backend (no S3/GCS creds). |
| **Metadata DB** | sqlite at `/data/hub.db` | `database.driver = sqlite`. |
| **Users DB** | `/data/auth.json` | `auth.users_db`. |
| **Uploads** | enabled (`--upload`) | A hub must accept client uploads. |
| **Read analytics** | enabled (`reads.enabled`) | Powers the Insights dashboard. |
| **Device/home state** | `BDRIVE_HOME=/data/home` | Overrides the image default `/tmp/bdrive` (ephemeral) so identity persists. |
| **Persistent volume** | one dir mounted at `/data` | Holds `storage/`, `home/`, `hub.db`, `auth.json`, `config.json`. |

Rendered config (only the parts flags can't set — flags supply remote/addr/upload):

```json
{
  "auth": { "admins": ["admin@example.com"], "users_db": "/data/auth.json" },
  "reads": { "enabled": true },
  "database": { "driver": "sqlite", "dsn": "/data/hub.db" }
}
```

Launch command: `serve --remote file:///data/storage --addr :8080 --upload -c /data/config.json`

## Remaining per-store work (needs a real instance)

- **UID/permissions:** distroless runs as nonroot UID **65532**; the init
  container `chown`s `/data` to match. Each platform mounts app-data with its
  own owner (Umbrel commonly 1000) — must be verified live.
- **TrueNAS:** `truenas/apps` expects the full `ix-dev` chart scaffold
  (`app.yaml` + `questions.yaml` + `templates/`); the draft here is the core,
  and the complete scaffold should be generated + validated with their tooling
  on a real TrueNAS SCALE box.
- **Health checks, port-conflict defaults, gallery assets** per each store's
  lint/CI.
