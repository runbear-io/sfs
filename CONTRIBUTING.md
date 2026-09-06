# Contributing to BearDrive

Thanks for wanting to help. BearDrive is early (pre-1.0) and moving fast —
small, focused PRs land quickest, and an issue or discussion before a big
change saves everyone time.

## Build & test

```sh
go build ./...        # everything, no CGO, no Node needed
go vet ./...
go test ./...         # full suite
go test ./internal/syncer -run TestConflict -v   # one test
```

Set `BDRIVE_HOME=/some/tmp/dir` when testing the CLI by hand so you never
touch your real `~/.bdrive`.

## Run a local hub

```sh
go build -o bdrive ./cmd/bdrive
mkdir -p /tmp/hub-storage
./bdrive serve /tmp/hub-storage --addr :8080 --upload   # plain-folder viewer
```

For hub mode with accounts, see the self-hosting guide
([docs/self-hosting.md](docs/self-hosting.md)). The e2e test harness
(`BDRIVE_E2E_SERVE=1 go test -run TestE2EServe ./internal/webapp`) starts a
seeded hub on :8993 with test accounts — handy for frontend work.

## The rules that matter here

- **Sync changes need multi-device tests.** The real coverage lives in
  `internal/syncer/syncer_test.go`: simulated devices syncing through a
  shared remote, driven cycle by cycle. A new sync behavior without a
  multi-device test is untested where it matters.
- **Never break sync.** Errors degrade to offline and retry next cycle;
  a cycle must not fail because a side feature (telemetry, hooks) did.
  Read the invariants section in [CLAUDE.md](CLAUDE.md) before touching
  `internal/syncer`, `internal/journal`, or `internal/store` — replay
  determinism and journal ownership are the whole concurrency story.
- **Frontend changes rebuild the committed assets.** The web UI lives in
  `internal/webapp/frontend` (React + TS, Vite); its build output is
  committed at `internal/webapp/static` so `go build` needs no Node.
  After changing `frontend/src`: `npm run build`, commit the new
  `static/`, and keep `npm run e2e` green. `frontend/check-dist.sh`
  verifies freshness.
- **Docs travel with behavior.** Changing CLI commands, flags, or output
  means updating `README.md`, `INSTALL_FOR_AGENTS.md`, and
  `web/docs/src/content/docs/` — `INSTALL_FOR_AGENTS.md` is the runbook
  agents follow to set themselves up, and `web/docs` is the end-user
  reference published at docs.beardrive.ai.

## Where to start

[ROADMAP.md](ROADMAP.md) marks items we'd love help with, and issues
labeled `good first issue` / `help wanted` are curated to be approachable.
Bug reports with a reproduction (the issue form asks for `bdrive version`,
OS, and hub vs plain-folder mode) are gold.

## Conduct

Be kind, be direct, assume good intent. Maintainers reserve the right to
moderate. Security issues: email snow@runbear.io rather than opening a
public issue.
