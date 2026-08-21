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

  // A fresh install opens on the welcome step (storyboard frame 2), not on a
  // project list it has nothing to put in.
  await page.goto("/");
  await expect(page).toHaveURL(/\/setup$/);
  await expect(page.getByRole("heading", { name: "Welcome to BearDrive" })).toBeVisible();
  await expect(page.getByText("One shared drive for your team and your AI agents")).toBeVisible();
  await expect(page.locator("#setup-start")).toBeVisible();
  await expect(page.getByText("You're signed in")).toHaveCount(0);
});
