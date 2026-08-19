import { test, expect } from "@playwright/test";
import { login, wikiId, MEMBER, READER, expectToast } from "./helpers";

// Phase 3: project home (connect guide + embedded dashboard), the dedicated
// dashboard route, and the history views. Ports the original parity checks
// from the pre-migration smoke suite.

test("landing is the project home (guide), not a dashboard redirect", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.waitForURL("/" + pid);
  await expect(page.locator(".guide")).toBeVisible();
  await expect(page.locator("#crumb")).toHaveText("wiki");
});

test("guide: one paste for every agent, one line of prose, details collapsed", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.waitForSelector(".guide");
  // No agent tabs anymore — the prompt is identical for every agent.
  await expect(page.locator(".gd-tab")).toHaveCount(0);
  const prompt = await page.$$eval(".gd-body > .gd-code code", (els) =>
    els.map((e) => e.textContent).join("\n"),
  );
  // The prompt points at the canonical instructions with the project id and
  // hub URL filled in; the agent fetches the doc for the actual steps.
  expect(prompt).toContain(
    "Follow https://raw.githubusercontent.com/runbear-io/beardrive/main/INSTALL_FOR_AGENTS.md",
  );
  expect(prompt).toContain(`project ${pid} on http://localhost:8993`);
  // The name rides along so the agent can recommend it as the folder name.
  expect(prompt).toContain('the project is named "wiki"');
  expect(prompt).not.toContain("brew install");
  // Detail lives behind the two collapsed sections.
  await expect(page.locator(".gd-manual > summary")).toHaveText([
    "What exactly happens",
    "Or run it yourself",
  ]);
  await expect(page.locator(".gd-manual[open]")).toHaveCount(0);
});

test("guide: manual fallback has the full command list and the docs link", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.waitForSelector(".guide");
  const manual = await page.$$eval(".gd-manual .gd-code code", (els) =>
    els.map((e) => e.textContent).join("\n"),
  );
  expect(manual).toContain("brew install runbear-io/tap/beardrive");
  expect(manual).toContain("bdrive login http://localhost:8993");
  expect(manual).toContain(`bdrive init --project ${pid}`);
  expect(manual).not.toContain("bdrive hooks install"); // init registers hooks itself
  await expect(page.locator('.gd-manual a[href="https://docs.beardrive.ai/manual/install/"]')).toHaveCount(1);
  // BEA-142: the prose above those three commands must not claim there is one,
  // nor hand `init` the sign-in step `bdrive login` is doing right beneath it.
  const desc = page.locator(".gd-manual", { hasText: "Or run it yourself" }).locator(".gd-desc").first();
  await expect(desc).not.toContainText("One command");
  await expect(desc).not.toContainText("signs this device in");
  await expect(desc).toContainText("registers the sync hooks and starts syncing");
});

test("home embeds the dashboard below the guide, for members too", async ({ page, browser }) => {
  await login(page);
  await page.waitForSelector(".guide");
  await expect(page.locator(".home-insights .insights")).toBeVisible();
  // Guide renders above the embedded dashboard
  const order = await page.evaluate(() => {
    const g = document.querySelector(".guide");
    const i = document.querySelector(".home-insights");
    return g && i ? (g.compareDocumentPosition(i) & Node.DOCUMENT_POSITION_FOLLOWING) !== 0 : false;
  });
  expect(order).toBe(true);
  await expect(page.locator(".in-treemap")).toBeVisible();
  await expect(page.locator(".in-hotpath .in-hp-row").first()).toBeVisible();

  const ctx = await browser.newContext();
  const p2 = await ctx.newPage();
  await login(p2, MEMBER);
  await p2.waitForSelector(".guide");
  await expect(p2.locator(".home-insights .insights")).toBeVisible();
  await ctx.close();
});

test("dedicated dashboard route still works and survives reload", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/dashboard`);
  await expect(page.locator("#crumb")).toHaveText("Dashboard — wiki");
  await expect(page.locator(".in-treemap")).toBeVisible();
  await page.reload();
  await expect(page.locator(".in-treemap")).toBeVisible();
});

test("a plain member gets the Dashboard, not a refusal", async ({ page }) => {
  await login(page, MEMBER);
  const pid = await wikiId(page);
  await page.click("#nav-dashboard");
  await page.waitForURL(`/${pid}/dashboard`);
  await expect(page.locator("#nav-dashboard")).toHaveClass(/active/);
  await expect(page.locator("#crumb")).toHaveText("Dashboard — wiki");
  await expect(page.locator(".in-treemap")).toBeVisible();
  await expect(page.locator("body")).not.toContainText("hub admins and org owners");
  // …and a scoped deep link resolves on a cold page, back/forward included.
  await page.goto(`/${pid}/dashboard/notes`);
  await expect(page.locator(".in-title .in-scope")).toContainText("notes");
  await page.goBack();
  await page.waitForURL(`/${pid}/dashboard`);
});

test("the retired /insights URL still lands on the Dashboard", async ({ page }) => {
  await login(page, MEMBER);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/insights`);
  await page.waitForURL(`/${pid}/dashboard`); // normalized, one live URL per page
  await expect(page.locator(".in-treemap")).toBeVisible();
  await page.goto(`/${pid}/insights/notes`);
  await page.waitForURL(`/${pid}/dashboard/notes`);
  await expect(page.locator(".in-title .in-scope")).toContainText("notes");
});

test("hot path row opens the file", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/dashboard`);
  await page.click(".in-hp-row:first-child");
  await page.waitForURL(/\/(index|guide)\.md$/);
  await expect(page.locator("#content h1")).toBeVisible();
});

test("vault name returns to the project home", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/index.md`);
  await page.click("#vault-name");
  await page.waitForURL("/" + pid);
  await expect(page.locator(".guide")).toBeVisible();
});

test("back/forward walks home → file → dashboard", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.waitForURL("/" + pid);
  await page.click('#tree .row[data-path="index.md"]');
  await page.waitForURL(`/${pid}/index.md`);
  await page.goto(`/${pid}/dashboard`);
  await page.goBack();
  await expect(page.locator("#content h1")).toHaveText("Wiki");
  await page.goBack();
  await expect(page.locator(".guide")).toBeVisible();
  await page.goForward();
  await expect(page.locator("#content h1")).toHaveText("Wiki");
});

test("history: whole project, newest first, and per-file versions", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.click("#history-btn"); // from home: whole project
  await page.waitForURL(`/${pid}/history`);
  await expect(page.locator("#crumb")).toContainText("History — all changes");
  await expect(page.locator(".history .hentry").first()).toBeVisible();
  expect(await page.locator(".history .hentry").count()).toBeGreaterThanOrEqual(6); // all seeded ops
  // guide.md has two versions
  await page.goto(`/${pid}/history/guide.md`);
  await expect(page.locator("#crumb")).toContainText("History — guide.md");
  await expect(page.locator(".history .hentry")).toHaveCount(2);
  await expect(page.locator(".history .hentry").first()).toContainText("edited");
  // clicking an entry opens THAT version of the file (BEA-7); aim at the
  // path cell — the row's center can land on its expandable note
  await page.click(".history .hentry.clickable >> nth=0 >> .hpath");
  await page.waitForURL(new RegExp(`/${pid}/guide\\.md\\?v=[0-9a-f]{64}$`));
});

test("per-file history: a version expands to show what changed", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/history/guide.md`);
  const rows = page.locator(".history .hentry");
  await expect(rows).toHaveCount(2);
  // Nothing is fetched until a row is expanded.
  const blobReqs: string[] = [];
  page.on("request", (r) => {
    if (r.url().includes("/blob?sha=")) blobReqs.push(r.url());
  });
  await expect(page.locator(".dv")).toHaveCount(0);

  // Newest version diffs against the one before it.
  await rows.nth(0).locator(".hdiff-btn").click();
  const dv = page.locator(".dv");
  await expect(dv).toBeVisible();
  await expect(dv.locator(".dv-rm")).toContainText("First version of the guide.");
  await expect(dv.locator(".dv-ins")).toContainText("Second version of the guide, with more detail.");
  await expect(dv.locator(".dv-add")).toHaveText("+1");
  await expect(dv.locator(".dv-del")).toHaveText("−1");
  expect(blobReqs.length).toBe(2); // exactly the two versions, once each

  // Collapsing and re-expanding is free: the blobs are cached by sha.
  await rows.nth(0).locator(".hdiff-btn").click();
  await expect(page.locator(".dv")).toHaveCount(0);
  await rows.nth(0).locator(".hdiff-btn").click();
  await expect(page.locator(".dv")).toBeVisible();
  expect(blobReqs.length).toBe(2);

  // The first version has nothing behind it, and says so.
  await expect(rows.nth(1).locator(".hdiff-btn")).toHaveCount(0);
  await expect(rows.nth(1).locator(".hdiff-none")).toContainText("nothing to compare against");

  // Expanding never navigates away.
  await expect(page).toHaveURL(`/${pid}/history/guide.md`);
});

test("per-file history: a binary version says so instead of diffing", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/history/assets/logo.png`);
  await page.locator(".history .hentry").nth(0).locator(".hdiff-btn").click();
  const dv = page.locator(".dv");
  await expect(dv).toContainText("Binary file — no diff available");
  // Both versions stay reachable by download.
  await expect(dv.locator("a", { hasText: "download previous" })).toHaveAttribute(
    "href",
    /blob\?sha=[0-9a-f]{64}&name=logo\.png&download=1/,
  );
  await expect(dv.locator("a", { hasText: "download this version" })).toBeVisible();
});

// BEA-58: the diff shipped, but only the per-file page passed the prop, so a
// reviewer who opened the whole-project feed first concluded the product had
// no diff at all. Every feed offers it now — and the assertion that matters is
// that a row in a MIXED-path feed diffs against its own path's predecessor.

test("the whole-project feed diffs a row against its own predecessor", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/history`);
  // Never nth(0): the newest row is the run's, not guide.md's.
  const row = page.locator(".history > .hentry", { hasText: "guide.md" }).first();
  await expect(row).toContainText("edited");
  await row.locator(".hdiff-btn").click();
  const dv = row.locator(".dv");
  await expect(dv).toBeVisible();
  // guide.md's own first version — not whichever row sits below it in the feed.
  await expect(dv.locator(".dv-rm")).toContainText("First version of the guide.");
  await expect(dv.locator(".dv-ins")).toContainText("Second version of the guide, with more detail.");
  await expect(dv.locator(".dv-add")).toHaveText("+1");
  await expect(dv.locator(".dv-del")).toHaveText("−1");
  // Expanding is not navigating.
  await expect(page).toHaveURL(`/${pid}/history`);

  // A delete has no content to diff, and says nothing at all — not "first
  // version".
  const del = page.locator(".hentry.delete", { hasText: "scratch.md" }).first();
  await expect(del.locator(".hdiff-btn")).toHaveCount(0);
  await expect(del.locator(".hdiff-none")).toHaveCount(0);
  // index.md has one version only, so it says so rather than rendering an
  // empty diff.
  const only = page.locator(".history > .hentry", { hasText: "index.md" }).first();
  await expect(only.locator(".hdiff-btn")).toHaveCount(0);
  await expect(only.locator(".hdiff-none")).toContainText("nothing to compare against");
  await expect(only.locator(".dv")).toHaveCount(0);

  // Rows inside a run card get it too: run.idx[k] indexes back into the flat
  // list, so the card's rows compare against their own paths — the rewritten
  // file against its 24h-old version, and the file the run CREATED against
  // nothing.
  const inCard = page.locator(".hrun-body .hentry", { hasText: "notes/readme.md" }).first();
  await inCard.locator(".hdiff-btn").click();
  await expect(inCard.locator(".dv-rm")).toContainText("Nested folder content.");
  await expect(inCard.locator(".dv-ins")).toContainText("Rewritten during the agent run.");
  const created = page.locator(".hrun-body .hentry", { hasText: "runbook.md" }).first();
  await expect(created.locator(".hdiff-btn")).toHaveCount(0);
  await expect(created.locator(".hdiff-none")).toContainText("nothing to compare against");
});

test("the folder feed diffs a row against its own predecessor", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/history/notes`);
  // The run touched one file inside notes/, so it is a bare row here, not a
  // card. Its predecessor is the 24h-old version, further down this feed.
  const row = page.locator(".history > .hentry", { hasText: "notes/readme.md" }).first();
  await row.locator(".hdiff-btn").click();
  const dv = row.locator(".dv");
  await expect(dv).toBeVisible();
  await expect(dv.locator(".dv-rm")).toContainText("Nested folder content.");
  await expect(dv.locator(".dv-ins")).toContainText("Rewritten during the agent run.");
  // Expanding is not navigating.
  await expect(page).toHaveURL(`/${pid}/history/notes`);
});

test("folder listing's Full history goes to the subtree feed", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/notes`);
  await page.click(".dl-more");
  await page.waitForURL(`/${pid}/history/notes`);
  await expect(page.locator("#crumb")).toContainText("History — notes/ (folder)");
  const paths = await page.$$eval(".history .hpath", (els) => els.map((e) => e.textContent));
  for (const p of paths) expect(p).toContain("notes/");
});

test("the dashboard scopes to the selected folder via the ⋯ menu", async ({ page }) => {
  await login(page, MEMBER); // members get the scoped entry too
  const pid = await wikiId(page);
  await page.goto(`/${pid}/notes`);
  await page.click("#more-btn");
  await page.click("#more-menu .more-item:has-text('Dashboard')");
  await page.waitForURL(`/${pid}/dashboard/notes`);
  await expect(page.locator(".in-title .in-scope")).toContainText("notes");
  // Scope note in the subtitle is the stable assertion.
  await expect(page.locator(".insights .dl-sub")).toContainText("notes and everything in it");
});

test("project menu pages each own a URL: Dashboard, Installation, Settings", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.click("#nav-dashboard");
  await page.waitForURL(`/${pid}/dashboard`);
  await expect(page.locator(".insights .in-title")).toContainText("Knowledge insights");
  await expect(page.locator("#nav-dashboard")).toHaveClass(/active/);
  await page.click("#nav-install");
  await page.waitForURL(`/${pid}/install`);
  await expect(page.locator("#crumb")).toHaveText("Installation");
  await expect(page.locator("#nav-install")).toHaveClass(/active/);
  await page.click("#nav-history");
  await page.waitForURL(`/${pid}/history`);
  await expect(page.locator("#nav-history")).toHaveClass(/active/);
  await page.click("#nav-settings");
  await page.waitForURL(`/${pid}/settings`);
  await expect(page.locator("#crumb")).toHaveText("Project settings");
  await expect(page.locator(".project-settings h2")).toHaveText("wiki");
  await page.click("#nav-dashboard");
  await page.waitForURL(`/${pid}/dashboard`);
  await expect(page.locator("#nav-dashboard")).toHaveClass(/active/);
  // Deep link + reload land on the page, like any URL.
  await page.goto(`/${pid}/settings`);
  await expect(page.locator(".project-settings h2")).toHaveText("wiki");
  await expect(page.locator("#nav-settings")).toHaveClass(/active/);
});

test("project settings: danger zone is owner-only", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/settings`);
  await expect(page.locator(".ps-danger")).toBeVisible();
  await expect(page.locator(".ps-danger .danger-btn")).toHaveText("Delete project");
});

test("project settings: a member sees no danger zone and cannot edit", async ({ page }) => {
  await login(page, MEMBER);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/settings`);
  await expect(page.locator(".project-settings h2")).toHaveText("wiki"); // page rendered
  await expect(page.locator(".ps-danger")).toHaveCount(0);
  // The General card is shown, disabled — not hidden, and with no way to submit.
  await expect(page.locator("#ps-name")).toBeDisabled();
  await expect(page.locator("#ps-desc")).toBeDisabled();
  await expect(page.locator("#ps-icon-btn")).toBeDisabled();
  await expect(page.locator("#ps-save")).toHaveCount(0);
  // The way out is a trust answer, so it is not owner-only: a member reading
  // About sees the export fact and the docs link.
  await expect(page.locator(".ps-export")).toContainText("bdrive export");
  await expect(page.locator(".ps-export a")).toHaveAttribute(
    "href",
    "https://docs.beardrive.ai/reference/migration/",
  );
});

test("project settings: icon + description save, and show in nav and dashboard", async ({
  page,
}) => {
  await login(page);
  const made = await (await page.request.post("/api/projects", { data: { name: "dressed" } })).json();
  const pid = made.project.id;
  await page.goto(`/${pid}/settings`);

  // Nothing dirty yet → nothing to save.
  await expect(page.locator("#ps-save")).toBeDisabled();
  // Placeholder until an icon is picked.
  await expect(page.locator(".ps-icon-row .proj-mark svg")).toHaveCount(1);

  await page.click("#ps-icon-btn");
  await page.click('.ps-icon-grid [aria-label="book-open"]');
  await page.fill("#ps-desc", "everything support needs");
  await expect(page.locator(".ps-count")).toHaveText("24 / 280");
  await expect(page.locator("#ps-save")).toBeEnabled();
  await page.click("#ps-save");
  await expectToast(page, "Saved");
  await expect(page.locator("#ps-save")).toBeDisabled(); // clean again

  // Both surfaces pick it up without a reload: the nav mark right here, and
  // the project header on the next SPA navigation (same header component the
  // project home renders).
  await expect(page.locator("#projects .proj-trigger .proj-mark svg")).toHaveCount(1);
  await page.click("#nav-install");
  await expect(page.locator(".in-desc")).toHaveText("everything support needs");
  await expect(page.locator(".gd-head .proj-mark svg")).toHaveCount(1);

  // …and it survives a reload, i.e. it really was persisted — on the project
  // home header, and back in the form.
  await page.goto(`/${pid}`);
  await expect(page.locator(".in-desc")).toHaveText("everything support needs");
  await expect(page.locator(".gd-head .proj-mark svg")).toHaveCount(1);
  await page.goto(`/${pid}/settings`);
  await expect(page.locator("#ps-desc")).toHaveValue("everything support needs");

  await page.request.delete("/api/projects/" + pid); // clean up
});

test("project settings: an over-long description is refused inline", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/settings`);
  await page.fill("#ps-desc", "x".repeat(281));
  await page.click("#ps-save");
  await expect(page.locator("#ps-desc-err")).toBeVisible();
  await expect(page.locator("#ps-desc")).toHaveValue("x".repeat(281)); // form not cleared
});

test("project settings: delete needs the exact name typed, then navigates away", async ({ page }) => {
  await login(page);
  await page.request.post("/api/projects", { data: { name: "condemned" } });
  await page.reload();
  const out = await (await page.request.get("/api/projects")).json();
  const pid = out.projects.find((p: { name: string }) => p.name === "condemned").id;

  await page.goto(`/${pid}/settings`);
  await page.click(".ps-danger .danger-btn");
  // Confirm stays disabled until the typed text matches exactly.
  await expect(page.locator(".modal .danger-btn")).toBeDisabled();
  await page.fill(".modal-input", "condemne");
  await expect(page.locator(".modal .danger-btn")).toBeDisabled();
  await page.fill(".modal-input", "condemned");
  await expect(page.locator(".modal .danger-btn")).toBeEnabled();
  await page.click(".modal .danger-btn");

  await expectToast(page, "Deleted");
  await expect(page).not.toHaveURL(new RegExp(pid));
  await expect(page.locator("#projects .row .label", { hasText: "condemned" })).toHaveCount(0);
});

test("project settings: People shows the default level and the grants", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/settings`);
  const people = page.locator(".ps-people");
  await expect(people).toBeVisible();
  // An admin gets live controls...
  await expect(people.locator('select[aria-label="Default access for workspace members"]')).toBeEnabled();
  await expect(people.locator("button", { hasText: "+ Add" })).toBeVisible();
  // ...the seeded read-only member is listed as an exception...
  await expect(people.locator(`select[aria-label="Access for ${READER}"]`)).toHaveValue("read");
  // ...and the workspace owner is shown as permanently admin, not editable.
  await expect(people.locator(".admin-item", { hasText: "Workspace owner" })).toBeVisible();
});

test("a read-only member: no Share, no danger zone, People is read-only", async ({ page }) => {
  await login(page, READER);
  const pid = await wikiId(page);

  // The project is fully visible and browsable.
  await page.goto(`/${pid}/index.md`);
  await expect(page.locator("#content h1")).toHaveText("Wiki");
  // ...but nothing that writes is offered.
  await expect(page.locator("#share-btn")).toHaveCount(0);

  await page.goto(`/${pid}/settings`);
  await expect(page.locator(".project-settings h2")).toContainText("wiki");
  await expect(page.locator(".ps-chip")).toHaveText("Read-only");
  await expect(page.locator(".ps-danger")).toHaveCount(0);
  // The People table is shown, disabled — same layout, no controls.
  await expect(page.locator(".ps-people")).toBeVisible();
  await expect(
    page.locator('.ps-people select[aria-label="Default access for workspace members"]'),
  ).toBeDisabled();
  await expect(page.locator(".ps-people button", { hasText: "+ Add" })).toHaveCount(0);
});

// BEA-130: reading back a link you can already see is not a write, so the
// copy control is NOT gated on revoke rights — the file page's banner
// already prints the whole URL to this same member.
test("a read-only member can copy a public link but not revoke it", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  const made = await (
    await page.request.post(`/api/p/${pid}/shares`, { data: { path: "index.md" } })
  ).json();

  await page.context().clearCookies();
  await login(page, READER);
  await page.goto(`/${pid}/settings`);
  const row = page.locator(".shares-table .admin-item", { hasText: "index.md" });
  await expect(row).toBeVisible();
  await row.locator("button[aria-label='Copy the public link to index.md']").click();
  await expectToast(page, "Copied.");
  await expect(row.locator(".ai-del")).toHaveCount(0);

  await page.context().clearCookies();
  await login(page);
  await page.request.delete(`/api/shares/${made.token}`);
});
