import { test, expect } from "@playwright/test";

// The onboarding wizard end to end (storyboard frames 2 and 5-9), against
// TestE2EDesktopOnboarding (:8996): signed in, zero mounts, a fake hub and a
// scratch Claude Code project at /tmp/bdrive-e2e-onboard/acme-app. The native
// folder chooser cannot be driven from a browser, so the spec types the path —
// the seam the sidecar deliberately keeps open.
//
// Headline copy is asserted verbatim from the storyboard, so drift fails here
// rather than in a review.

test.use({ baseURL: "http://localhost:8996" });

const ROOT = "/tmp/bdrive-e2e-onboard/acme-app";

test("signed in with no folders, /setup leads to the connect step", async ({ page }) => {
  await page.goto("/setup");
  await expect(page).toHaveURL(/\/setup\/connect$/);
  await expect(page.getByRole("heading", { name: "Add a shared folder to your project" })).toBeVisible();
  await expect(page.getByText("RECOMMENDED")).toBeVisible();
  await expect(page.getByText("Your project stays yours.")).toBeVisible();
});

test("the tree preview follows what is typed, and names the private/shared split", async ({ page }) => {
  await page.goto("/setup/connect");
  await page.locator("#setup-root").fill(ROOT);
  // Detection and the preview both come from /api/desktop/inspect.
  await expect(page.getByText(/Claude Code project detected/)).toBeVisible();
  const tree = page.locator(".setup-tree");
  await expect(tree.getByText("acme-app/")).toBeVisible();
  await expect(tree.getByText("CLAUDE.md")).toBeVisible();
  await expect(tree.getByText("src/")).toBeVisible();
  // The shared row is its own element (the footnote mentions the name too).
  await expect(tree.locator(".setup-tree-shared")).toHaveText(/team\/\s+shared/);
  await expect(tree.getByText(/Only team\/ syncs\. Everything else never leaves this Mac\./)).toBeVisible();

  // Renaming re-renders the preview and the button.
  await page.locator("#setup-name").fill("wiki");
  await expect(tree.locator(".setup-tree-shared")).toHaveText(/wiki\/\s+shared/);
  await expect(page.locator("#setup-go")).toHaveText("Create wiki/ and start syncing");
});

test("a name the org already uses flips the screen into join mode", async ({ page }) => {
  await page.goto("/setup/connect");
  await page.locator("#setup-root").fill(ROOT);
  await page.locator("#setup-name").fill("shared");
  await expect(page.getByText(/already shares a .shared. space/)).toBeVisible();
  await expect(page.locator("#setup-go")).toHaveText("Join shared/ and start syncing");
});

test("a name that is a path is refused before anything is created", async ({ page }) => {
  await page.goto("/setup/connect");
  await page.locator("#setup-root").fill(ROOT);
  await page.locator("#setup-name").fill("../escape");
  await expect(page.locator(".setup-err")).toBeVisible();
  await expect(page.locator("#setup-go")).toBeDisabled();
});

// Runs last: it actually connects the folder, so it changes the harness state
// (the mount then exists and the connect screen would refuse a second one).
test("connecting reaches the success screen with the agent prompt", async ({ page }) => {
  await page.goto("/setup/connect");
  await page.locator("#setup-root").fill(ROOT);
  await page.locator("#setup-name").fill("team");
  await expect(page.locator("#setup-go")).toBeEnabled();
  await page.locator("#setup-go").click();

  await expect(page).toHaveURL(/\/setup\/syncing$/);
  await expect(page.getByRole("heading", { name: /Syncing/ })).toBeVisible();
  await expect(page.getByText("You can close this window — syncing continues from the menu bar.")).toBeVisible();

  // The first cycle runs against an unreachable hub (offline) and still
  // finishes: the folder is connected either way.
  await expect(page).toHaveURL(/\/setup\/done$/, { timeout: 60_000 });
  await expect(page.getByRole("heading", { name: "team/ is live" })).toBeVisible();
  await expect(page.getByText("Tell your agent")).toBeVisible();
  await expect(page.getByText("Invite teammates")).toBeVisible();

  // The invite card asks the hub and reports what it says (this harness hub
  // serves no orgs, so the honest answer is an error, not a fake link).
  await page.locator("#setup-invite").click();
  await expect(page.locator(".setup-err")).toBeVisible();

  // The prompt a teammate pastes into their agent is on the clipboard.
  await page.locator("#setup-copy-prompt").click();
  const copied = await page.evaluate(() => navigator.clipboard.readText());
  expect(copied).toContain("INSTALL_FOR_AGENTS.md");
  expect(copied).toContain("team/");

  // And the connected folder is now a real mount the app knows about.
  const status = await (await page.request.get("/api/desktop/status")).json();
  expect(status.mounts.length).toBe(1);
  expect(status.mounts[0].path).toBe(ROOT + "/team");
});
