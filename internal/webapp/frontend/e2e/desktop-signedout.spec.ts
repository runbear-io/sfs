import { test, expect } from "@playwright/test";

// The fresh-install first screen (TestE2EDesktopSignedOut, :8995 — virgin
// BDRIVE_HOME, no session, no mounts). Found in the 2026-08-20 install test:
// this screen used to claim "You're signed in" while signed out.

test.use({ baseURL: "http://localhost:8995" });

test("signed-out first run leads with Sign in, not a false 'signed in'", async ({ page, request }) => {
  const cfg = await (await request.get("/api/config")).json();
  expect(cfg.desktop).toBe(true);
  expect(cfg.me).toBeUndefined();
  const sess = await (await request.get("/api/desktop/session")).json();
  expect(sess.signed_in).toBe(false);

  await page.goto("/");
  await expect(page.getByText("Sign in to your hub to see your projects here.")).toBeVisible();
  await expect(page.locator("#ob-signin")).toBeVisible();
  await expect(page.getByText("You're signed in")).toHaveCount(0);
  // The agent path stays available — an agent can run the sign-in itself.
  await expect(page.getByText("Follow https://raw.githubusercontent.com").first()).toBeVisible();
});
