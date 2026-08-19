# BearDrive — system overview

The whole repo on one page: every package and surface, and which detail
diagram drills into it. Reflects the code as of this commit; update this
file in any PR that adds/removes a package or changes how the pieces
connect. Detail diagrams: [cli-sync.md](cli-sync.md),
[webapp-server.md](webapp-server.md),
[webapp-frontend.md](webapp-frontend.md).

```mermaid
flowchart LR
    subgraph device["User device"]
        wf["working folder<br/>(real files + .bdrive/config.json)"]
        cli["cmd/bdrive<br/>CLI commands"]
        dmn["internal/daemon<br/>background loop"]
        eng["internal/syncer Session.Cycle<br/>internal/journal ops + replay"]
        vs["volume store ~/.bdrive/volumes/id<br/>internal/store: blobs, journals,<br/>state, paused marker"]
        cfg["internal/config<br/>device.json, settings.json, mounts.json"]
    end

    subgraph agents["Agent platforms (claude / codex / gemini / hermes)"]
        hooks["internal/agenthooks<br/>turn-boundary sync hooks"]
    end

    subgraph hub["bdrive serve hub"]
        srv["internal/webapp Server<br/>auth, orgs, projects, shares,<br/>history, read heat, store proxy"]
        fe["webapp/frontend React SPA<br/>committed dist go:embed'ed at webapp/static"]
        meta["MetaStore: file JSON (default)<br/>or sqlite / postgres (db_sql)"]
    end

    store["object store (hub-owned)<br/>internal/remote: file:// s3:// gs://<br/>blobs + per-device journals"]

    tpl["internal/templates<br/>go:embed'ed starting structures<br/>(docs, wiki, para, skills: skeleton + AGENTS.md)"]

    sec["internal/secrets<br/>six credential rules, stdlib only<br/>shared by the share gate and the sync scan"]

    docs["web/docs — docs.beardrive.ai<br/>Astro/Starlight, deploys separately"]
    cloud["cloud/ (PRIVATE nested repo, gitignored)<br/>managed beardrive.ai: swaps AuthProvider,<br/>QuotaProvider, MetaStore seams"]

    wf <-->|scan / materialize| eng
    cli --> eng
    dmn --> eng
    cli --> cfg
    eng --> vs
    eng <-->|"https:// backend (internal/remote/http.go)<br/>device token, /api/p/id/store/*"| srv
    hooks -->|"bdrive sync --hook / --note, read-log<br/>gated: enrolled + not paused"| cli
    srv --> store
    srv --> meta
    eng -->|"scan: warn, never hold"| sec
    srv -->|"share mint: refuse · markdown render: badge"| sec
    cli -->|"init --template: seed locally"| tpl
    srv -->|"POST /api/projects template:<br/>seed as ops under the hub's device"| tpl
    fe -->|/api/config, /api/projects, viewer APIs| srv
    cloud -.->|imports OSS packages,<br/>replaces providers| srv
    docs -.->|documents| cli
```

Not drawn in any detail diagram (deliberately): `web/docs` (content site, no
Go/TS application code) and `cloud/` (private repo — its architecture lives
there; here it only consumes the provider seams drawn in webapp-server.md).
