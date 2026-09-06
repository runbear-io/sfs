## What & why

<!-- One paragraph: what changes, and the problem it solves. -->

## Checklist

- [ ] `go build ./... && go vet ./... && go test ./...` green
- [ ] Sync behavior changes have a multi-device test in `internal/syncer`
- [ ] Frontend changes: `npm run build` re-committed `internal/webapp/static` and `npm run e2e` is green
- [ ] CLI behavior changes updated `README.md`, `INSTALL_FOR_AGENTS.md` and `web/docs/src/content/docs/`
