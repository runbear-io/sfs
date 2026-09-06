import { test, expect } from "@playwright/test";

// Desktop-mode parity: the same SPA served by the `bdrive desktop` sidecar
// (TestE2EDesktop in cmd/bdrive, :8994) over local volume-store state — no
// auth gate, read-only projects, hub-proxied heat/shares/restore. This is
// the "tour" check the parity matrix in .claude/desktop-goal.md points at.

const HUB_ID = "11111111-2222-4333-8444-555555555555";

test("desktop mode: no auth gate, read-only project keyed by hub id", async ({ request }) => {
  const cfg = await (await request.get("/api/config")).json();
  expect(cfg.mode).toBe("hub");
  expect(cfg.desktop).toBe(true);
  expect(cfg.auth.enabled).toBe(false);
  expect(cfg.reads.enabled).toBe(true);
  const out = await (await request.get("/api/projects")).json();
  expect(out.projects).toHaveLength(1);
  expect(out.projects[0].id).toBe(HUB_ID);
  expect(out.projects[0].perm).toBe("read");
});

test("renders local markdown; links navigate in-app; reload keeps the deep link", async ({ page }) => {
  await page.goto(`/${HUB_ID}/index.md`);
  await expect(page.locator("h1")).toContainText("Wiki-local");
  await page.locator("main a:visible", { hasText: "the plan" }).first().click();
  await expect(page).toHaveURL(/notes\/plan\.md/);
  await expect(page.locator("h1")).toContainText("Plan");
  await page.reload();
  await expect(page).toHaveURL(/notes\/plan\.md/);
  await expect(page.locator("h1")).toContainText("Plan");
});

test("history comes from the local journals; any version is addressable", async ({ page, request }) => {
  await page.goto(`/${HUB_ID}/history`);
  await expect(page.getByText("e2e@example.com").first()).toBeVisible();
  const hist = await (await request.get(`/api/p/${HUB_ID}/history?path=index.md`)).json();
  expect(hist.entries.length).toBe(2);
  const oldest = hist.entries[hist.entries.length - 1];
  const v1 = await (await request.get(`/api/p/${HUB_ID}/blob?sha=${oldest.blob}`)).text();
  expect(v1).toContain("first draft");
});

test("restore is offered on desktop (hub-backed)", async ({ page }) => {
  await page.goto(`/${HUB_ID}/history`);
  await expect(page.locator(".hrestore-btn").first()).toBeVisible();
});

test("share mints through the hub proxy and lands on the clipboard", async ({ page }) => {
  await page.goto(`/${HUB_ID}/index.md`);
  await page.locator("#share-btn").click();
  await expect(page.getByText("e2e-share").first()).toBeVisible();
  // The copy control is the whole point of a share link on desktop.
  const copied = await page.evaluate(() => navigator.clipboard.readText());
  expect(copied).toContain("/s/e2e-share");
});

test("heat reaches the dashboard through the hub proxy", async ({ page }) => {
  await page.goto(`/${HUB_ID}/dashboard`);
  await expect(page.getByText("Knowledge insights")).toBeVisible();
  await expect(page.getByText("index.md").first()).toBeVisible();
});

test("settings reflect the hub's real permission, not the local read-only", async ({ page }) => {
  // The fake hub answers /permissions with me:"admin" — so this account may
  // edit, and the grants list is the hub's, not the empty local registry's.
  await page.goto(`/${HUB_ID}/settings`);
  await expect(page.getByText("reader@example.com").first()).toBeVisible();
  await expect(page.locator(".ps-chip", { hasText: "Read-only" })).toHaveCount(0);
});

test("new project runs the connect flow, not a hub-only create dialog", async ({ page }) => {
  // On desktop a project without a local folder cannot appear in the list at
  // all (it is built from this machine's mounts), so the + goes where a
  // project actually gets made: pick a folder, name the shared one, sync.
  await page.goto(`/${HUB_ID}/`);
  await page.waitForSelector("#sidebar");
  await page.locator(".nav-add").click();
  await expect(page).toHaveURL(/\/setup\/connect$/);
  await expect(page.getByRole("heading", { name: "Add a shared folder to your project" })).toBeVisible();
  await expect(page.getByText("Docs + decision records")).toHaveCount(0);
});

test("command palette opens and finds files", async ({ page }) => {
  await page.goto(`/${HUB_ID}/`);
  await page.waitForSelector("#sidebar");
  await page.keyboard.press("ControlOrMeta+k");
  const input = page.locator("[cmdk-input]");
  await expect(input).toBeVisible();
  await input.fill("plan");
  await expect(page.locator("[cmdk-item]", { hasText: "plan.md" }).first()).toBeVisible();
});
