import { test, expect } from "@playwright/test";
import { login, wikiId } from "./helpers";

/* BEA-119: the Dashboard's hot-and-stale verdict has to reach the screens the
   document is actually read on. The seeded hub holds exactly three flagged
   files in archive/ — read a lot, unchanged for months — plus moved-guide.md,
   which is fresh and unread and must stay unmarked everywhere. */

const FLAGGED = ["archive/retired-spec.md", "archive/old-runbook.md", "archive/legacy-notes.md"];
const NOT_FLAGGED = "archive/moved-guide.md";

test("a hot-and-stale file says so on its own page, in words and elapsed time", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/${FLAGGED[0]}`);
  const meta = page.locator("#meta");
  // The warning, not the raw-date arithmetic the reader used to have to do.
  await expect(meta.locator(".meta-stale")).toContainText("stale");
  await expect(meta.locator(".meta-stale")).toContainText(/last changed \d+ (day|month|year)s? ago/);
  await expect(meta).toContainText("⚠");
  // The exact timestamp and the read count stay — the badge adds, never replaces.
  await expect(meta).toContainText("/ 30d");
});

test("a fresh file gets no warning, and neither does a historical version", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/${NOT_FLAGGED}`);
  await page.waitForSelector("#meta");
  await expect(page.locator("#meta .meta-stale")).toHaveCount(0);

  // Read counts belong to the path, not to one version, so neither does the
  // verdict derived from them.
  const hist = await (await page.request.get(`/api/p/${pid}/history?path=${FLAGGED[0]}`)).json();
  const sha = hist.entries[0].blob; // the history API calls it "blob"
  await page.goto(`/${pid}/${FLAGGED[0]}?v=${sha}`);
  // Wait on the CONTENT, not on #meta: with nothing to say the meta line is
  // empty, and `#meta:empty { display: none }` means a visibility wait here
  // would hang on exactly the passing case.
  await expect(page.getByRole("heading", { name: "Retired spec" })).toBeVisible();
  await expect(page.locator("#meta .meta-stale")).toHaveCount(0);
});

test("the folder listing marks every flagged file and nothing else", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/archive`);
  await page.waitForSelector(".dl-items");

  for (const path of FLAGGED) {
    const row = page.locator(".dl-row").filter({ hasText: path.split("/").pop()! });
    // A real aria-label, not a hover-only title: touch and screen readers
    // never get the hover.
    await expect(row.locator(".stalemark")).toHaveAttribute("aria-label", /Warning: stale/);
  }
  const fresh = page.locator(".dl-row").filter({ hasText: NOT_FLAGGED.split("/").pop()! });
  await expect(fresh.locator(".stalemark")).toHaveCount(0);
  await expect(page.locator(".dl-items .stalemark")).toHaveCount(FLAGGED.length);

  // Folders have no single mtime to be stale against, so a folder row never
  // carries the mark however hot its subtree is.
  await page.goto(`/${pid}/notes`);
  await page.waitForSelector(".dl-items");
  const deepRow = page.locator(".dl-row").filter({ hasText: "deep" });
  await expect(deepRow).toHaveCount(1);
  await expect(deepRow.locator(".stalemark")).toHaveCount(0);
});

test("the Dashboard still flags exactly that set — no threshold drift", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/dashboard`);
  await page.waitForSelector(".in-chart");

  const dots = page.locator(".in-pt.danger");
  await expect(dots).toHaveCount(FLAGGED.length);
  const paths = await page.locator(".in-tm-label", { hasText: "⚠" }).evaluateAll((els) =>
    els.map((e) => e.getAttribute("data-path")),
  );
  expect(paths.sort()).toEqual([...FLAGGED].sort());
});
