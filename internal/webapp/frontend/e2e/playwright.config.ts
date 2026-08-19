import { defineConfig } from "@playwright/test";

// The suite runs against the committed Go e2e harness (e2e_serve_test.go):
// a deterministic seeded hub on :8993, freshly wiped on every start. It
// exercises the BUILT frontend served by the Go binary — run `npm run
// build` before `npm run e2e`, or test against stale assets.
export default defineConfig({
  testDir: ".",
  timeout: 30_000,
  retries: 0,
  workers: 1, // specs share one hub with mutable state (uploads, shares)
  use: {
    baseURL: "http://localhost:8993",
    // Headless Chromium withholds the clipboard permission a real browser
    // grants the focused tab. Without it every copy control in the app
    // reports failure, so a spec that clicks one measures the "select and
    // copy it yourself" fallback and never the path a user is on.
    permissions: ["clipboard-write"],
  },
  webServer: {
    command:
      "cd ../../../.. && BDRIVE_E2E_SERVE=1 go test -count=1 -timeout 3h -run TestE2EServe ./internal/webapp",
    url: "http://localhost:8993/",
    // Never reuse. The hub serves the assets it was BUILT with, so a server
    // left over from a previous run answers with the previous frontend — every
    // spec then measures code that is not on disk any more. That cost round 11
    // hours of false positives and round 12 re-derived the same rule; a 5s
    // start beats a result nobody can trust.
    reuseExistingServer: false,
    timeout: 60_000,
  },
});
