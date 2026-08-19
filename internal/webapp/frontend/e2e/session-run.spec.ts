import { test, expect } from "@playwright/test";
import { login, wikiId, expectToast, READER } from "./helpers";

/* One agent run, both halves (BEA-98). History used to show only what a run
   CHANGED; the reads lived in a daily aggregate with no session dimension and
   could not be joined to it. The seeded run reads four files and rewrites
   one of them — the fourth is a file the seed later deletes (BEA-152). */

test("a run card shows what the session read as well as what it changed", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/history`);

  const card = page.locator(".hrun").first();
  await expect(card).toBeVisible();
  // The header counts both halves now.
  await expect(card.locator(".hrun-meta")).toContainText("read 4");
  await expect(card.locator(".hrun-meta")).toContainText("changed 2");

  // The file the run read AND rewrote carries the read marker on its own row.
  const rewritten = card.locator(".hentry", { hasText: "notes/readme.md" });
  await expect(rewritten.locator(".hread")).toHaveText("read");
  // The file it created was never read, so that row has no marker.
  await expect(card.locator(".hentry", { hasText: "runbook.md" }).locator(".hread")).toHaveCount(0);

  // What it read and did not touch is its own list.
  const readOnly = card.locator(".hrun-read");
  await expect(readOnly).toHaveCount(3);
  await expect(readOnly.first()).toContainText("archive/retired-spec.md");
  await expect(readOnly.last()).toContainText("scratch.md");

  // BEA-152: a file the run read and the project no longer has stays on the
  // card, labelled the way the Dashboard labels the same file — the two
  // surfaces read one ledger, so they must not answer differently.
  const gone = card.locator(".hrun-read", { hasText: "scratch.md" });
  await expect(gone.locator(".in-hp-gone")).toHaveText("· no longer in the project");
  // ...and the footnote says what the card is actually scoped to, instead of
  // asserting a deleted-file filter that no code implements.
  await expect(card.locator(".hrun-foot")).toHaveText(
    "Reads shown are what this device reported for this session — a narrower set than the project's read totals.",
  );
});

test("a read-only row opens the file it names", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/history`);
  await page.locator(".hrun-read", { hasText: "index.md" }).click();
  await expect(page).toHaveURL(new RegExp(`/${pid}/index.md`));
});

/* BEA-82: the run-wide verb the card was grouped for. Every row inside the
   card already had an action; the header had none, so reverting a bad run
   meant clicking file by file and hoping you got them all.

   Driven against the seeded run (session 8f21e4 on device `seed`: one file
   rewritten, one created) and left exactly as it was found — the last step
   undoes the undo, which is also the point: the undo is itself a run card. */
test("undoing a whole run puts every file it touched back", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);

  // Somebody edits a file the run created, AFTER the run. The confirm has to
  // say that this change is about to be overwritten too — the one thing in
  // this feature that can burn a teammate.
  await page.request.put(`/api/p/${pid}/upload/content?path=runbook.md`, {
    data: "# Runbook\n\nEdited by a teammate after the run.\n",
  });

  await page.goto(`/${pid}/history`);
  const card = page.locator(".hrun", { hasText: "claude-code session 8f21e4" }).first();
  await expect(card).toBeVisible();

  // The header carries the action; the run's own rows still carry theirs.
  await card.locator(".hrun-undo").click();
  const modal = page.locator(".modal");
  await expect(modal).toContainText("Undo this run?");
  // Every path the run touched, with what will happen to each.
  const rows = modal.locator(".undo-row");
  await expect(rows).toHaveCount(2);
  await expect(rows.filter({ hasText: "notes/readme.md" })).toContainText("restore to pre-run version");
  await expect(rows.filter({ hasText: "runbook.md" })).toContainText("remove (the run created it)");
  // ...and the warning, named out loud.
  await expect(modal.locator(".undo-warn")).toContainText("changed by someone else after this run");

  // It writes to every synced device, so Cancel has to mean nothing happened.
  await modal.getByRole("button", { name: "Cancel" }).click();
  await page.goto(`/${pid}/runbook.md`);
  await expect(page.locator("#content")).toContainText("Edited by a teammate");

  await page.goto(`/${pid}/history`);
  await card.locator(".hrun-undo").click();
  await page.locator(".modal .danger-btn").click();
  await expectToast(page, /Undid 2 files/);

  // The file the run edited holds its pre-run content again, and the file it
  // created is gone.
  await page.goto(`/${pid}/notes/readme.md`);
  await expect(page.locator("#content")).toContainText("Nested folder content");
  await page.goto(`/${pid}/runbook.md`);
  await expect(page.locator("#content")).toContainText("isn't in this project");

  // The undo is itself a run card — same note on every op it wrote — so it
  // carries the same button and walks the whole thing back.
  await page.goto(`/${pid}/history`);
  const undoCard = page.locator(".hrun", { hasText: "undo run 8f21e4" }).first();
  await expect(undoCard).toBeVisible();
  await undoCard.locator(".hrun-undo").click();
  await page.locator(".modal .danger-btn").click();
  await expectToast(page, /Undid 2 files/);
  await page.goto(`/${pid}/notes/readme.md`);
  await expect(page.locator("#content")).toContainText("Rewritten during the agent run");

  // Put the fixture back: the teammate's post-run edit above is the one thing
  // the round trip legitimately restored, and later specs read this file.
  await page.request.put(`/api/p/${pid}/upload/content?path=runbook.md`, {
    data: "# Runbook\n\nCreated during the agent run.\n",
  });
});

// A write action, so a read-only member gets no button rather than one that
// 403s — the same rule the per-row restore and remove follow.
test("a read-only member sees no undo button on a run card", async ({ page }) => {
  await login(page, READER);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/history`);
  const card = page.locator(".hrun").first();
  await expect(card).toBeVisible();
  await expect(card.locator(".hrun-undo")).toHaveCount(0);
});
