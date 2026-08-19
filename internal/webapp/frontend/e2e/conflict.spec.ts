import { test, expect } from "@playwright/test";
import { login, wikiId } from "./helpers";

// BEA-128: a conflict copy is the guarantee the shared-folder promise rests
// on, and it used to appear nowhere in the hub. Two surfaces: the listing
// row says what the file is, the file page explains it.

const COPY = "archive/old-runbook.md.bdrive-conflict-mira-laptop-20260814T060945Z";
const NAME = COPY.split("/").pop()!;

test("the file listing flags a conflict copy", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/archive`);
  const row = page.locator(".dl-row").filter({ hasText: NAME });
  await expect(row.locator(".dl-conflict")).toHaveText("conflict copy");
  // The label has to travel without hover — touch and screen readers get
  // neither the title nor the badge's own two words.
  await expect(row.locator(".dl-conflict")).toHaveAttribute(
    "aria-label",
    /concurrent edit from mira-laptop/,
  );
  // Ordinary files keep their ordinary row.
  await expect(
    page.locator(".dl-row").filter({ hasText: /^old-runbook\.md/ }).first().locator(".dl-conflict"),
  ).toHaveCount(0);
});

test("the conflict copy's page explains it and links the other version", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/${COPY}`);
  const banner = page.locator(".vbanner").filter({ hasText: "Conflict copy" });
  await expect(banner).toContainText("mira-laptop");
  await expect(banner).toContainText("2026"); // the moment, in local time
  await expect(banner).toContainText("archive/old-runbook.md");
  await banner.getByRole("button", { name: /other version/i }).click();
  await page.waitForURL(`/${pid}/archive/old-runbook.md`);
  await expect(page.locator("#content")).toContainText("Still read, never maintained");
});

test("an ordinary file gets no conflict banner", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/archive/old-runbook.md`);
  await expect(page.locator(".vbanner").filter({ hasText: "Conflict copy" })).toHaveCount(0);
});
