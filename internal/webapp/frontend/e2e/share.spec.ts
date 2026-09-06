import { expect, test } from "@playwright/test";
import { READER, login, wikiId } from "./helpers";

/* The Share dialog on a FOLDER — the surface that did not exist before: the
   toolbar's Share button was hidden on directories, and folder access could
   only be set from an admin page nobody opens from the folder they are
   looking at. */

test("a folder can be restricted from the folder itself", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/notes`);

  await page.click("#share-btn");
  const dialog = page.locator(".modal");
  await expect(dialog).toContainText("Who can open this");
  // A folder has no link of its own, and the dialog says so rather than
  // hiding the section — which is what keeps it one dialog and not two.
  await expect(dialog).toContainText("Public links are per file");
  await expect(dialog.locator("#share-public")).toHaveCount(0);

  // Everyone starts on the project's own level.
  const level = dialog.locator("select").first();
  await expect(level).toHaveValue("");
  await level.selectOption("read");
  await page.click(".modal button:has-text('Done')");

  const got = await (await page.request.get(`/api/p/${pid}/folders`)).json();
  const rule = got.folders.find((f: { prefix: string }) => f.prefix === "notes/");
  expect(rule.default).toBe("read");

  /* ...and the folder now says so where you can see it, which is the only
     place it is announced now that Settings does not enumerate rules.

     For a TOP-LEVEL folder that place is the tree, not a listing: the project
     root renders the connect guide and the dashboard, so `notes` never appears
     in a .dl-row at all. A rule on a nested folder gets the listing pill
     instead — both are asserted, because dropping either one leaves a whole
     class of restricted folder with nowhere to announce itself. */
  await page.goto(`/${pid}`);
  await expect(page.locator(".file-tree .trestricted, nav .trestricted").first()).toBeVisible();

  await page.request.put(`/api/p/${pid}/folders`, { data: { prefix: "notes/deep", default: "read" } });
  await page.goto(`/${pid}/notes`);
  await expect(
    page
      .locator(".dl-row")
      .filter({ has: page.locator(".dl-name", { hasText: /^deep$/ }) })
      .locator(".dl-restricted"),
  ).toBeVisible();
  await page.request.delete(`/api/p/${pid}/folders?prefix=notes%2Fdeep%2F`);

  await page.request.delete(`/api/p/${pid}/folders?prefix=notes%2F`);
});

/* A file has no access of its own — there are no per-file grants — so the
   dialog names the rule that decides and offers to open it, rather than
   showing a control that would quietly rewrite every sibling file's access. */
test("a file's dialog points at the folder rule that governs it", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.request.put(`/api/p/${pid}/folders`, {
    data: { prefix: "notes", default: "read" },
  });
  await page.goto(`/${pid}/notes/readme.md`);

  await page.click("#share-btn");
  const dialog = page.locator(".modal");
  await expect(dialog).toContainText("Access comes from the rule on");
  await expect(dialog).toContainText("notes/");
  // The file's own section is the public link, and it is live here.
  await expect(dialog.locator("#share-public")).toBeVisible();

  await dialog.locator("button:has-text('Open sharing for notes/')").click();
  await expect(page.locator(".modal")).toContainText("Public links are per file");

  await page.request.delete(`/api/p/${pid}/folders?prefix=notes%2F`);
});

// Sharing is a write. A read-only member is not offered it — the same rule
// that hid it from them before folders were shareable.
test("a read-only member is offered no Share button on a folder", async ({ page }) => {
  await login(page, READER);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/notes`);
  await expect(page.locator("#share-btn")).toHaveCount(0);
});
