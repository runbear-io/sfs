import { test, expect } from "@playwright/test";
import { login, wikiId, expectToast, READER } from "./helpers";

// Phase 2: tree, folder listings (heat dots + change feed), file views
// (markdown/wikilinks/images), breadcrumbs, upload, share, palette.

test("tree lists the seeded folders and files", async ({ page }) => {
  await login(page);
  await expect(page.locator('#tree .row[data-path="notes"]')).toBeVisible();
  await expect(page.locator('#tree .row[data-path="index.md"]')).toBeVisible();
  await expect(page.locator('#tree .row[data-path="guide.md"]')).toBeVisible();
});

test("markdown file: rendered content, crumb, meta, download + share buttons", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.click('#tree .row[data-path="index.md"]');
  await page.waitForURL(`/${pid}/index.md`);
  await expect(page.locator("#content h1")).toHaveText("Wiki");
  await expect(page.locator("#crumb")).toContainText("index.md");
  // The full whoChanged() string, not just the address: the seed's Author
  // equals its User, so "alice@x.io" alone passed even when the viewer was
  // rendering the git/OS identity instead of the signed-in account.
  await expect(page.locator("#meta")).toContainText("Alice <alice@x.io>");
  // Download lives in the ⋯ menu now; the hidden anchor powers it.
  await expect(page.locator("#download")).toHaveCount(1);
  await expect(page.locator("#more-btn")).toBeVisible();
  await expect(page.locator("#share-btn")).toBeVisible();
});

test("wikilink navigates to the target file", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/index.md`);
  const link = page.locator('#content a:has-text("guide")');
  // BEA-136: the href itself, not just the click. Copy-link-address,
  // middle-click and open-in-new-tab all read this attribute, and it used to
  // be the unresolvable string "wiki:guide".
  await expect(link).toHaveAttribute("href", `/${pid}/guide.md`);
  await page.evaluate(() => ((window as Window & { __spa?: number }).__spa = 1));
  await link.click();
  await page.waitForURL(`/${pid}/guide.md`);
  await expect(page.locator("#content")).toContainText("Second version");
  // Still the same document: a plain click must SPA-route, not reload.
  expect(await page.evaluate(() => (window as Window & { __spa?: number }).__spa)).toBe(1);
});

// BEA-136: everything the rendered anchor has to get right besides the click.
test("wikilinks: modified click is the browser's, a dangling one has no href", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/index.md`);
  // A cmd/ctrl-click belongs to the browser (new tab), so THIS page stays put.
  await page.locator('#content a:has-text("guide")').click({ modifiers: ["ControlOrMeta"] });
  await page.waitForTimeout(300);
  expect(new URL(page.url()).pathname).toBe(`/${pid}/index.md`);
  // [[nowhere]] matches no file: unresolved, and no dead href to copy.
  const missing = page.locator("#content a.wiki-missing");
  await expect(missing).toHaveText("nowhere");
  expect(await missing.getAttribute("href")).toBeNull();
  expect(await page.locator("#content").innerHTML()).not.toContain("wiki:");
});

test("folder listing: counts, change feed, heat dot on a read file", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.click('#tree .row[data-path="notes"]');
  await page.waitForURL(`/${pid}/notes`);
  await expect(page.locator(".dl-title")).toContainText("notes");
  await expect(page.locator(".dl-sub")).toContainText("1 folder");
  await expect(page.locator(".dl-sub")).toContainText("1 file");
  await expect(page.locator(".dl-history .dl-h3")).toHaveText("Recent changes");
  await expect(page.locator(".dl-history .hentry").first()).toBeVisible();
  // notes/readme.md has seeded agent reads → a heat dot on its row
  await expect(page.locator('.dl-row[title="notes/readme.md"] .heatdot')).toBeVisible();
});

// BEA-28: copying a folder URL hands you a trailing slash, and that URL used
// to 404 while the sidebar showed the folder populated right next to it.
test("folder URL with a trailing slash renders the listing and drops the slash", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}`);
  await page.goto(`/${pid}/notes/`);
  await expect(page.locator(".dl-title")).toContainText("notes");
  await expect(page.locator(".dl-history .dl-h3")).toHaveText("Recent changes");
  await expect(page).toHaveURL(`/${pid}/notes`);
  // Replaced, not pushed: Back leaves the folder instead of bouncing off the
  // slashed URL and landing right back here.
  await page.goBack();
  await expect(page).toHaveURL(`/${pid}`);
});

// BEA-17: the kind glyph read as a disclosure toggle. It is now a text
// badge, the row's only real expander is the note, and clicking the badge
// navigates like the rest of the row — no dead zone, no second behavior.
test("history row: kind is a badge, not a disclosure control", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/history/guide.md`);
  const row = page.locator(".history .hentry").first();
  await expect(row).toBeVisible();
  // kind is conveyed as text, not by an icon shape
  await expect(row.locator(".hkind")).toHaveText("edited");
  await expect(row.locator(".hkind .ico")).toHaveCount(0);
  // a row announces kind, path and author without the icon
  await expect(page.getByRole("button", { name: /edited\s+guide\.md.*alice@x\.io/s })).toHaveCount(1);
  // only genuine expanders claim to expand: the note and the diff disclosure
  await expect(page.locator(".history .hnote[aria-expanded]")).toHaveCount(1);
  await expect(page.locator(".history [aria-expanded]:not(.hnote):not(.hdiff-btn)")).toHaveCount(0);
  // ...and it still expands in place, without navigating
  await row.locator(".hnote").click({ position: { x: 6, y: 6 } }); // off the note's link
  await expect(row.locator(".hnote")).toHaveClass(/open/);
  await expect(page).toHaveURL(`/${pid}/history/guide.md`);
  // clicking the badge does exactly what clicking the row does: it opens
  // the version the row describes (BEA-7), not a dead zone
  await row.locator(".hkind").click();
  await page.waitForURL(new RegExp(`/${pid}/guide\\.md\\?v=[0-9a-f]{64}$`));
});

test("image file renders an <img>", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/assets/logo.png`);
  await expect(page.locator("#content img")).toBeVisible();
});

test("breadcrumb ancestor opens that folder", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/notes/deep/topic.md`);
  await expect(page.locator("#content h1")).toHaveText("Topic");
  await page.click('#crumb .crumb-seg[title="notes"]');
  await page.waitForURL(`/${pid}/notes`);
  await expect(page.locator(".dl-title")).toContainText("notes");
});

test("deep file link resolves after a hard reload", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/notes/readme.md`);
  await expect(page.locator("#content h1")).toHaveText("Notes");
  await page.reload();
  await expect(page.locator("#content h1")).toHaveText("Notes");
  // The tree unfolds the way to the deep-linked file
  await expect(page.locator('#tree .row[data-path="notes/readme.md"]')).toBeVisible();
});

test("back/forward walks file → folder → file", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/index.md`);
  await page.click('#tree .row[data-path="notes"]');
  await page.waitForURL(`/${pid}/notes`);
  await page.goBack();
  await expect(page.locator("#content h1")).toHaveText("Wiki");
  await page.goForward();
  await expect(page.locator(".dl-title")).toContainText("notes");
});

test("header search button opens the palette", async ({ page }) => {
  await login(page);
  await wikiId(page);
  await page.click("#search-btn");
  await expect(page.locator("#palette")).toBeVisible();
  await page.keyboard.press("Escape");
});

test("palette (⌘K) fuzzy-jumps to a file", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.keyboard.press("ControlOrMeta+k");
  await expect(page.locator("#palette")).toBeVisible();
  await page.fill("#palette input", "topic");
  await page.keyboard.press("Enter");
  await page.waitForURL(`/${pid}/notes/deep/topic.md`);
  await expect(page.locator("#content h1")).toHaveText("Topic");
});

// BEA-105: the switcher excludes the project you're in, so typing its own name
// used to match nothing while the palette copy promised project search. The
// root row carries the name now — one row, kind `project`, same destination.
test("palette finds the project you are inside by name", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}`);
  await page.waitForSelector("#sidebar");
  await page.keyboard.press("ControlOrMeta+k");
  await expect(page.locator("#palette")).toBeVisible();
  // with no query typed, at least one PROJECT row is on screen
  expect(await page.locator("#palette [cmdk-item] .pkind", { hasText: "project" }).count()).toBeGreaterThan(0);

  await page.fill("#palette input", "wiki");
  const rows = page.locator("#palette [cmdk-item]", { hasText: "wiki" });
  await expect(rows).toHaveCount(1); // exactly one, no duplicate root action
  await expect(rows.first().locator(".pkind")).toHaveText("project");
  await page.keyboard.press("Enter");
  await page.waitForURL(`/${pid}`);
});

// BEA-52: on a path that doesn't resolve the tree entries are gone and the
// switcher lists only other projects, so the palette used to offer no way
// back. cmdk owns the list's id (it overwrites ours), hence [cmdk-list].
test("palette on a dead route still offers the way back", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/does-not-exist.md`);
  await expect(page.locator(".notfound")).toBeVisible();
  await page.keyboard.press("ControlOrMeta+k");
  await expect(page.locator("#palette")).toBeVisible();
  for (const label of ["project root", "Dashboard", "Installation", "Settings"]) {
    await expect(page.locator("#palette [cmdk-list]")).toContainText(label);
  }
  // exactly one whole-project history entry, no duplicate
  expect(
    await page.locator("#palette [cmdk-item]", { hasText: "History: whole project" }).count(),
  ).toBe(1);
  await page.fill("#palette input", "Dashboard");
  await page.keyboard.press("Enter");
  await page.waitForURL(`/${pid}/dashboard`);
  await page.reload(); // the entries are real URLs, not panel state
  await expect(page.locator(".in-treemap")).toBeVisible();
});

// BEA-54: cmdk overwrites the `id` we pass its primitives, so every palette
// rule anchored on one was dead — the input lost its only author `color` and
// fell back to the UA's black. Anchors here are ours (#palette) or cmdk's own
// attributes, which is exactly what the fix relies on.
test("palette renders in the dark palette, not UA black (BEA-54)", async ({ page }) => {
  await login(page);
  await wikiId(page);
  await page.keyboard.press("ControlOrMeta+k");
  const input = page.locator("#palette input");
  await input.fill("topic");
  await expect(input).toHaveCSS("color", "rgb(238, 240, 243)"); // --text, was rgb(0, 0, 0)
  const sel = page.locator("#palette [cmdk-item][data-selected='true']");
  await expect(sel.locator(".plabel")).toHaveCSS("color", "rgb(255, 207, 133)"); // --accent-bright
  await expect(sel.locator(".pkind")).toHaveCSS("text-transform", "uppercase");
  await expect(sel).toHaveCSS("background-color", "rgba(245, 166, 35, 0.13)"); // --glow
  await page.keyboard.press("Escape");
});

test("share mints a public link that serves the file, revoke kills it", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/guide.md`);
  await page.click("#share-btn");
  // BEA-32: the modal hands over the URL and nothing destructive — Revoke is
  // the banner's, and two of them for one link is what this asserts away.
  await expect(page.locator(".modal .ai-del")).toHaveCount(0);
  const url = await page.locator(".modal-url").textContent();
  expect(url).toContain("/s/");
  const publicRes = await page.request.get(url!);
  expect(publicRes.status()).toBe(200);
  expect(await publicRes.text()).toContain("Second version");

  // Revoke where the control actually lives, from the file page.
  await page.click(".modal button:has-text('Done')");
  const banner = page.locator(".share-banner");
  await expect(banner).toBeVisible();
  await banner.locator(".ai-del").click();
  await page.click(".modal .danger-btn");
  await expectToast(page, "Share revoked");
  const gone = await page.request.get(url!);
  expect(gone.status()).toBe(404);
});

// BEA-147: the same finding the share dialog names, on the path every file
// takes — the viewer used to render the key as ordinary prose.
test("a file holding a key carries a badge in the file view", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/deploy.md`);

  const badge = page.locator(".sbadge");
  await expect(badge).toBeVisible();
  // Same rule, same line, same words as the share dialog's modal below.
  await expect(badge).toContainText("an AWS access key (line 3)");
  // Advisory: the file still renders in full, nothing is redacted.
  await expect(page.locator("#content h1")).toHaveText("Deploy");
  await expect(page.locator("#content")).toContainText("AWS_ACCESS_KEY_ID");
  // The badge itself must never echo the thing it found.
  await expect(badge).not.toContainText("AKIA");

  // A clean file gets no badge at all.
  await page.goto(`/${pid}/index.md`);
  await expect(page.locator("#content h1")).toHaveText("Wiki");
  await expect(page.locator(".sbadge")).toHaveCount(0);
});

// BEA-111: sharing a file that looks like it holds credentials asks first.
// Cancel mints nothing; Share anyway mints the link it would have.
test("share on a file holding a key asks before it mints, and Cancel mints nothing", async ({
  page,
}) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/deploy.md`);

  // Cancel: the dialog names the finding, and no link exists afterwards.
  await page.click("#share-btn");
  const dialog = page.locator(".modal", { hasText: "This file may contain credentials" });
  await expect(dialog).toBeVisible();
  await expect(dialog).toContainText("an AWS access key (line 3)");
  // The copy may only ever claim what was true at mint time.
  await expect(dialog).toContainText("at the moment you share it");
  await expect(dialog).toContainText("later changes are never checked");
  // …and it must never echo the thing it found.
  await expect(dialog).not.toContainText("AKIA");
  await dialog.locator("button:has-text('Cancel')").click();
  await expect(page.locator(".modal-url")).toHaveCount(0);
  const before = await (await page.request.get(`/api/p/${pid}/shares`)).json();
  expect(before.shares.filter((s: { path: string }) => s.path === "deploy.md")).toHaveLength(0);

  // Share anyway: the same click, carried through.
  await page.click("#share-btn");
  await page.locator(".modal button:has-text('Share anyway')").click();
  const url = (await page.locator(".modal-url").textContent())!;
  expect(url).toContain("/s/");
  const publicRes = await page.request.get(url);
  expect(publicRes.status()).toBe(200);
  await page.click(".modal button:has-text('Done')");

  // Already public: a second Share hands back the same link without asking.
  await page.reload();
  await page.click("#share-btn");
  await expect(page.locator(".modal", { hasText: "This file may contain credentials" })).toHaveCount(0);
  expect(await page.locator(".modal-url").textContent()).toBe(url);
  await page.click(".modal button:has-text('Done')");

  await page.request.delete(`/api/shares/${url.split("/s/")[1]}`);
});

// BEA-29: the CLI has had --expires all along; the dialog now offers it on
// the link you just minted, without changing that link's URL.
test("share dialog sets an expiry on the link it just minted", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/index.md`);
  await page.click("#share-btn");
  const url = (await page.locator(".modal-url").textContent())!;
  await expect(page.locator(".modal-expiry-note")).toHaveText("no expiry");

  await page.selectOption("#share-expiry", "168h");
  await expect(page.locator(".modal-expiry-note")).toContainText("expires");
  // Same link: the URL already on the clipboard keeps working.
  expect(await page.locator(".modal-url").textContent()).toBe(url);
  expect((await page.request.get(url)).status()).toBe(200);
  await page.click(".modal button:has-text('Done')");

  // …and Settings stops calling it permanent.
  await page.goto(`/${pid}/settings`);
  const row = page.locator(".admin-item", { hasText: "index.md" });
  await expect(row.locator(".ai-tag")).toContainText("expires");
  await expect(row.locator(".ai-tag")).not.toContainText("no expiry");

  await page.request.delete(`/api/shares/${url.split("/s/")[1]}`);
});

test("no browser upload: content arrives via sync; the tree picks it up", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/notes`);
  // The upload affordance is gone everywhere — content enters via local sync.
  await expect(page.locator("#upload-btn")).toHaveCount(0);
  await expect(page.locator('input[type="file"]')).toHaveCount(0);
  // A file lands through the device/store path (simulated via the API)…
  await page.request.put(
    `/api/p/${pid}/upload/content?path=${encodeURIComponent("notes/dropped.md")}`,
    { data: "# Dropped\n\nArrived through sync.\n" },
  );
  // …and the polling tree shows it; opening renders it.
  await page.goto(`/${pid}/notes/dropped.md`);
  await expect(page.locator("#content h1")).toHaveText("Dropped");
  await expect(page.locator('#tree .row[data-path="notes/dropped.md"]')).toBeVisible();
});

test("html file renders as a page in a sandboxed iframe", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  const html = "<h1 id='t'>Hello from HTML</h1><script>document.title='js-ran'</scr" + "ipt>";
  await page.request.put(
    `/api/p/${pid}/upload/content?path=${encodeURIComponent("pages/hello.html")}`,
    { data: html },
  );
  await page.goto(`/${pid}/pages/hello.html`);
  const frame = page.locator("#content iframe.htmlview");
  await expect(frame).toBeVisible();
  await expect(frame).toHaveAttribute("sandbox", "allow-scripts");
  await expect(page.frameLocator("#content iframe.htmlview").locator("#t")).toHaveText(
    "Hello from HTML",
  );
  // Server-side wall: inline HTML carries the sandbox CSP (same as /s/*).
  const res = await page.request.get(`/api/p/${pid}/file?path=${encodeURIComponent("pages/hello.html")}`);
  expect(res.headers()["content-security-policy"]).toBe("sandbox allow-scripts");
});

test("missing path gets the not-found view; Check again finds a late upload", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/later.md`);
  await expect(page.locator(".notfound h1")).toHaveText("Couldn't find that");
  await expect(page.locator(".notfound code")).toHaveText("later.md");
  await expect(page.locator(".notfound")).toContainText("still be uploading");
  // The file arrives (a teammate/agent finished syncing it)…
  await page.request.put(
    `/api/p/${pid}/upload/content?path=${encodeURIComponent("later.md")}`,
    { data: "# Finally here\n" },
  );
  await page.click(".notfound .pbtn"); // Check again
  await expect(page.locator("#content h1")).toHaveText("Finally here");
});

test("tree chevron folds and unfolds a folder", async ({ page }) => {
  await login(page);
  await wikiId(page);
  await expect(page.locator('#tree .row[data-path="notes"]')).toBeVisible();
  // Unfold via row click (opens listing + expands), then fold via chevron.
  await page.click('#tree .row[data-path="notes"]');
  await expect(page.locator('#tree .row[data-path="notes/readme.md"]')).toBeVisible();
  await page.click('#tree .row[data-path="notes"] .chev');
  await expect(page.locator('#tree .row[data-path="notes/readme.md"]')).not.toBeVisible();
  await page.click('#tree .row[data-path="notes"] .chev');
  await expect(page.locator('#tree .row[data-path="notes/readme.md"]')).toBeVisible();
});

// BEA-16: the undo for "I made this public" lives on the file, not three
// clicks away in the org panel.
test("public link: the file page says it is shared, and revokes without a reload", async ({
  page,
}) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/guide.md`);
  await expect(page.locator(".share-banner")).toHaveCount(0);

  await page.click("#share-btn");
  const url = (await page.locator(".modal-url").textContent())!;
  await page.click(".modal button:has-text('Done')");

  // The indicator is on the file itself, and it is still there after a reload
  // (the dialog used to be the only place the link — and its Revoke — existed).
  await page.reload();
  const banner = page.locator(".share-banner");
  await expect(banner).toBeVisible();
  await expect(banner).toContainText("Publicly shared");
  await expect(banner).toContainText("1 active link");
  await expect(banner).toContainText("no expiry");
  expect((await page.request.get(url)).status()).toBe(200);

  // Revoking from the file page kills the link and updates in place.
  await banner.locator(".ai-del").click();
  await page.click(".modal .danger-btn");
  await expectToast(page, "Share revoked");
  await expect(banner).toHaveCount(0);
  expect((await page.request.get(url)).status()).toBe(404);
});

test("project settings lists this project's public links and revokes them", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.request.post(`/api/p/${pid}/shares`, { data: { path: "notes/readme.md" } });

  await page.goto(`/${pid}/settings`);
  const row = page.locator(".admin-item", { hasText: "notes/readme.md" });
  await expect(row).toBeVisible();
  await expect(row.locator(".ai-tag")).toContainText("by e2e@example.com");
  await expect(row.locator(".ai-tag")).toContainText("no expiry");

  await row.locator(".ai-del").click();
  await page.click(".modal .danger-btn");
  await expectToast(page, "Share revoked");
  await expect(page.locator(".admin-item", { hasText: "notes/readme.md" })).toHaveCount(0);
  await expect(page.locator(".admin-empty", { hasText: "No public links." })).toBeVisible();
});

// "No public links." before the request lands is a confident no to "is anything
// of ours public right now?" — the one wrong answer this panel must never give.
test("public links show a loading row, never a premature 'no', while shares load", async ({
  page,
}) => {
  await login(page);
  const pid = await wikiId(page);
  const made = await (
    await page.request.post(`/api/p/${pid}/shares`, { data: { path: "notes/readme.md" } })
  ).json();

  await page.route("**/api/p/*/shares", async (route) => {
    await new Promise((r) => setTimeout(r, 2000));
    await route.continue();
  });

  await page.goto(`/${pid}/settings`);
  await expect(page.locator(".admin-empty", { hasText: "Loading…" })).toBeVisible();
  await expect(page.locator(".admin-empty", { hasText: "No public links." })).toHaveCount(0);

  await expect(page.locator(".admin-item", { hasText: "notes/readme.md" })).toBeVisible({
    timeout: 10_000,
  });
  await expect(page.locator(".admin-empty", { hasText: "Loading…" })).toHaveCount(0);

  await page.unroute("**/api/p/*/shares");
  await page.request.delete(`/api/shares/${made.token}`);
});

test("a read-only member sees the public-link banner but cannot revoke", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  const made = await (
    await page.request.post(`/api/p/${pid}/shares`, { data: { path: "index.md" } })
  ).json();

  // A real second identity in this page: drop the admin session first, or
  // the helper's first-time form login never sees /auth/login.
  await page.context().clearCookies();
  await login(page, READER);
  await page.goto(`/${pid}/index.md`);
  const banner = page.locator(".share-banner");
  await expect(banner).toBeVisible();
  await expect(banner).toContainText("Publicly shared");
  await expect(banner.locator("button:has-text('Copy link')")).toBeVisible();
  await expect(banner.locator(".ai-del")).toHaveCount(0);
  await expect(page.locator("#share-btn")).toHaveCount(0);

  await page.context().clearCookies();
  await login(page); // clean up as someone who may
  await page.request.delete(`/api/shares/${made.token}`);
});

test("public links: banner and settings table fit a 390px viewport", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  const made = await (
    await page.request.post(`/api/p/${pid}/shares`, { data: { path: "guide.md" } })
  ).json();
  await page.setViewportSize({ width: 390, height: 780 });

  const sideways = () =>
    page.evaluate(() => document.documentElement.scrollWidth > window.innerWidth + 1);

  await page.goto(`/${pid}/guide.md`);
  await expect(page.locator(".share-banner")).toBeVisible();
  expect(await sideways()).toBe(false);

  await page.goto(`/${pid}/settings`);
  await expect(page.locator(".admin-item", { hasText: "guide.md" })).toBeVisible();
  expect(await sideways()).toBe(false);
  // The table takes its own horizontal scroll rather than widening the page.
  // By class, not by position: People renders through the same AdminTable.
  const box = page.locator(".project-settings .shares-table");
  expect(await box.evaluate((el) => getComputedStyle(el).overflowX)).toBe("auto");

  await page.request.delete(`/api/shares/${made.token}`);
});

// BEA-7: a history row is an address for the version it describes.

test("history row opens THAT version, banner says so, View current returns", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/history/guide.md`);
  // Oldest row = the first version; click it rather than the latest.
  const added = page.locator(".hentry.add");
  await expect(added).toBeVisible();
  await added.click();
  await page.waitForURL(new RegExp(`/${pid}/guide\\.md\\?v=[0-9a-f]{64}$`));
  await expect(page.locator("#content")).toContainText("First version");
  await expect(page.locator("#content")).not.toContainText("Second version");
  // Rendered markdown, not raw source.
  await expect(page.locator("#content h1")).toHaveText("Guide");
  // The banner is what stops the page misleading.
  const banner = page.locator(".vbanner");
  await expect(banner).toBeVisible();
  await expect(banner).toContainText("This is not the current file");
  await expect(banner).toContainText("alice@x.io");
  await expect(banner.locator("a[download]")).toHaveAttribute("href", /blob\?sha=[0-9a-f]{64}.*download=1/);
  // Downloading while pinned gives that version, not the current bytes.
  await expect(page.locator("#download")).toHaveAttribute("href", /blob\?sha=[0-9a-f]{64}/);
  await banner.getByText("View current").click();
  await page.waitForURL(`/${pid}/guide.md`);
  await expect(page.locator("#content")).toContainText("Second version");
  await expect(page.locator(".vbanner")).toHaveCount(0);
});

test("a version URL survives a hard reload, and Back returns to history", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/history/guide.md`);
  await page.locator(".hentry.add").click();
  const url = page.url();
  await page.reload();
  await expect(page.locator("#content")).toContainText("First version");
  await expect(page.locator(".vbanner")).toBeVisible();
  await page.goto(url); // fresh navigation to the deep link
  await expect(page.locator("#content")).toContainText("First version");
  await page.goBack();
  await expect(page.locator(".history .hentry").first()).toBeVisible();
});

test("an unknown version says so instead of showing current content", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/guide.md?v=${"a".repeat(64)}`);
  await expect(page.locator("#content")).toContainText("That version isn't available");
  await expect(page.locator("#content")).not.toContainText("Second version");
  await expect(page.locator(".vbanner")).toBeVisible(); // still offers a way back
});

test("delete rows have no version to open, so they stay unclickable", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/history/scratch.md`);
  const del = page.locator(".hentry.delete");
  await expect(del).toBeVisible();
  await expect(del).not.toHaveClass(/clickable/);
  await del.click();
  await expect(page).toHaveURL(`/${pid}/history/scratch.md`);
});

// BEA-6: one agent run is one card, and any version can be put back.

test("history groups one agent run into a single card", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/history`);
  const run = page.locator(".hrun");
  await expect(run).toHaveCount(1);
  await expect(run.locator(".hrun-note")).toHaveText("claude-code session 8f21e4");
  // Both halves of the run, since the seed gives it session reads (BEA-98).
  await expect(run.locator(".hrun-meta")).toContainText("changed 2");
  await expect(run.locator(".hrun-meta")).toContainText("seed-agent");
  // Both of the run's changes live inside the card...
  await expect(run.locator(".hentry")).toHaveCount(2);
  await expect(run.locator('.hentry:has-text("runbook.md")')).toBeVisible();
  // ...and note-less changes are still bare rows, exactly as before.
  await expect(page.locator(".history > .hentry").first()).toBeVisible();
  await expect(page.locator(".history > .hentry .hrun-note")).toHaveCount(0);
  // The note is not repeated on every row inside the card.
  await expect(run.locator(".hnote")).toHaveCount(0);
  // The card collapses without navigating.
  await run.locator(".hrun-toggle").click();
  await expect(run.locator(".hentry")).toHaveCount(0);
  await expect(page).toHaveURL(`/${pid}/history`);
});

// BEA-35: the one thing history couldn't reverse was a file a run CREATED.
// Undo removes it (via a delete op the hub journals), and the DELETED row it
// leaves behind restores it — so the round trip is what the test asserts, and
// the seeded run is left exactly as it was found.
test("a file the run created can be undone, and comes back", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/history`);
  const created = page.locator('.hrun .hentry.add:has-text("runbook.md")');
  await expect(created).toBeVisible();
  // An add has no old bytes to put back — its undo is a removal.
  await expect(created.locator(".hrestore-btn")).toHaveCount(0);
  await expect(created.locator(".hremove-btn")).toBeVisible();
  // The file it EDITED gets no restore either, for the other reason: that
  // edit is still notes/readme.md's current content, so putting it back could
  // only write an empty change (BEA-57). The rule reaches inside run cards.
  // What it does not get is the removal — that is the run-created row's alone.
  const edited = page.locator('.hrun .hentry.edit:has-text("notes/readme.md")');
  await expect(edited).toBeVisible();
  await expect(edited.locator(".hrestore-btn")).toHaveCount(0);
  await expect(edited.locator(".hremove-btn")).toHaveCount(0);

  // It reaches every device, so it always asks first — and Cancel means no.
  await created.locator(".hremove-btn").click();
  const modal = page.locator(".modal");
  await expect(modal).toContainText("Remove runbook.md?");
  await expect(modal).toContainText("every synced device");
  await modal.getByRole("button", { name: "Cancel" }).click();
  await expect(page.locator(`.hentry.delete:has-text("runbook.md")`)).toHaveCount(0);

  await created.locator(".hremove-btn").click();
  await page.locator(".modal .danger-btn").click();
  await expectToast(page, /Removed runbook\.md/);
  const gone = page.locator('.history > .hentry.delete:has-text("runbook.md")').first();
  await expect(gone).toBeVisible();

  // ...and the delete row puts it back, bytes and all — after asking, and
  // saying the one true thing about this branch: History can't take it away
  // again (BEA-129).
  await gone.locator(".hrestore-btn").click();
  const rmodal = page.locator(".modal");
  await expect(rmodal).toContainText("Restore this version of runbook.md?");
  await expect(rmodal).toContainText("isn't available from History yet");
  // Restore adds content, so it is not dressed as a destructive action.
  await expect(rmodal.locator(".danger-btn")).toHaveCount(0);
  await rmodal.getByRole("button", { name: "Restore" }).click();
  await expectToast(page, /Restored runbook\.md/);
  await page.goto(`/${pid}/runbook.md`);
  await expect(page.locator("#content")).toContainText("Created during the agent run");
});

test("restoring an old version brings its content back", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  // Its own file, with its own two versions — restoring is a real write, so
  // it must not disturb what the rest of the suite reads.
  const path = "restore-me.md";
  const url = `/api/p/${pid}/upload/content?path=${path}`;
  await page.request.put(url, { data: "# Restore me\n\nThe good version.\n" });
  await page.request.put(url, { data: "# Restore me\n\nClobbered by an agent.\n" });

  await page.goto(`/${pid}/history/${path}`);
  const older = page.locator(".hentry.add"); // the first version
  await expect(older).toBeVisible();

  // It reaches every device, so it asks first — and Cancel writes nothing:
  // no request, no `restoring…`, no new row (BEA-129).
  await older.locator(".hrestore-btn").click();
  const modal = page.locator(".modal");
  await expect(modal).toContainText("Restore this version of restore-me.md?");
  // The file still exists, so this restore IS walk-backable — the copy says so.
  await expect(modal).toContainText("You can restore any other version afterwards.");
  await page.keyboard.press("Escape"); // Esc dismisses it too
  await expect(modal).toHaveCount(0);
  await older.locator(".hrestore-btn").click();
  await modal.getByRole("button", { name: "Cancel" }).click();
  await expect(modal).toHaveCount(0);
  await expect(page.locator(".history .hentry")).toHaveCount(2);
  await expect(page.locator(".hrestore-btn").first()).toHaveText(/restore$/);
  // Asking is still not navigating: the row stayed put through the dialog.
  await expect(page).toHaveURL(`/${pid}/history/${path}`);

  await older.locator(".hrestore-btn").click();
  await modal.getByRole("button", { name: "Restore" }).click();
  await expectToast(page, /Restored restore-me\.md/);
  // The restore is itself a change, and the file serves the old bytes again.
  await expect(page.locator(".history .hentry")).toHaveCount(3);
  await expect(page.locator(".history .hentry").first()).toContainText("restore restore-me.md@");
  await page.goto(`/${pid}/${path}`);
  await expect(page.locator("#content")).toContainText("The good version");
});

// BEA-57: the newest row IS the file's current content, so restoring it could
// only journal a +0 −0 change and sync it to every teammate. The button is
// gone there, and the server refuses the same call, so no client can write it.
test("the current version offers no restore", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  // Read-only against the seeded file, so the rest of the suite still finds
  // guide.md saying "Second version".
  await page.goto(`/${pid}/history/guide.md`);
  const rows = page.locator(".history .hentry");
  await expect(rows.first()).toBeVisible();
  await expect(rows.first().locator(".hrestore-btn")).toHaveCount(0);
  await expect(rows.nth(1).locator(".hrestore-btn")).toHaveCount(1);
  // The API is the real guarantee — the hidden button is just the UI agreeing.
  const feed = await (await page.request.get(`/api/p/${pid}/history?path=guide.md`)).json();
  const res = await page.request.post(`/api/p/${pid}/restore`, {
    data: { path: "guide.md", sha: feed.entries[0].blob },
  });
  expect(res.status()).toBe(409);
  expect(await res.text()).toContain("already the current content");
  await expect(rows).toHaveCount(2); // nothing was written

  // ...and after a real restore, the row it just created is the new current
  // version — its own file, since this one writes.
  const path = "current-version.md";
  const url = `/api/p/${pid}/upload/content?path=${path}`;
  await page.request.put(url, { data: "# Current\n\nThe good version.\n" });
  await page.request.put(url, { data: "# Current\n\nClobbered.\n" });
  await page.goto(`/${pid}/history/${path}`);
  const own = page.locator(".history .hentry");
  await expect(own.first().locator(".hrestore-btn")).toHaveCount(0);
  await own.last().locator(".hrestore-btn").click(); // the oldest row: the first version
  await page.locator(".modal").getByRole("button", { name: "Restore" }).click();
  await expectToast(page, /Restored current-version\.md/);
  await expect(own).toHaveCount(3);
  await expect(own.first().locator(".hrestore-btn")).toHaveCount(0);
  // The rule is content equality, not row index: the first version now holds
  // the same bytes as the head, so it stops offering a restore too.
  await expect(own.nth(1).locator(".hrestore-btn")).toHaveCount(1);
  await expect(own.nth(2).locator(".hrestore-btn")).toHaveCount(0);
});

test("a read-only member gets no restore or remove buttons", async ({ page }) => {
  await login(page, READER);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/history`);
  await expect(page.locator(".history .hentry").first()).toBeVisible();
  await expect(page.locator(".hrestore-btn")).toHaveCount(0);
  await expect(page.locator(".hremove-btn")).toHaveCount(0);
});

// BEA-26: the row was already an address for its version — but a bare
// role="button" div announces that to nobody, so a persona whose whole fear
// is "an agent quietly rewrote my doc" concludes recovery is impossible.
// The version now carries visible handles.

test("history rows carry visible Open/Download controls for that version", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/history/guide.md`);
  const added = page.locator(".hentry.add"); // the first version of guide.md
  await expect(added).toBeVisible();

  const open = added.getByRole("button", { name: /Open guide\.md as of/ });
  const dl = added.getByRole("link", { name: /Download guide\.md as of/ });
  await expect(open).toBeVisible();
  await expect(dl).toBeVisible();
  // Neither claims to expand anything (BEA-17's invariant).
  await expect(page.locator(".history [aria-expanded]:not(.hnote):not(.hdiff-btn)")).toHaveCount(0);

  // The download is that version's bytes, not the current file's.
  const href = await dl.getAttribute("href");
  expect(href).toMatch(/blob\?sha=[0-9a-f]{64}&name=guide\.md&download=1$/);
  const body = await (await page.request.get(href!)).text();
  expect(body).toContain("First version");
  expect(body).not.toContain("Second version");

  // Open lands on the file pinned to that version, and fires once.
  await open.click();
  await page.waitForURL(new RegExp(`/${pid}/guide\\.md\\?v=[0-9a-f]{64}$`));
  await expect(page.locator(".vbanner")).toBeVisible();
  await expect(page.locator("#content")).toContainText("First version");
});

test("version controls are keyboard-reachable and don't double-fire the row", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/history/guide.md`);
  const open = page.locator(".hentry.add").getByRole("button", { name: /Open guide\.md as of/ });
  await open.focus();
  await expect(open).toBeFocused();
  await open.press("Enter");
  await page.waitForURL(new RegExp(`/${pid}/guide\\.md\\?v=[0-9a-f]{64}$`));
  await expect(page.locator("#content")).toContainText("First version");
  // One entry in history, not two: the row's own handler never also ran.
  await page.goBack();
  await expect(page).toHaveURL(`/${pid}/history/guide.md`);
});

test("a delete row offers no version to open or download", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/history/scratch.md`);
  const del = page.locator(".hentry.delete");
  await expect(del).toBeVisible();
  await expect(del.locator(".hver-btn")).toHaveCount(0);
});

test("the folder change feed carries the version controls too", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/notes`);
  // This feed passes no `diff` prop at all — the controls must not be
  // gated behind it.
  const row = page.locator(".dl-history .hentry").first();
  await expect(row).toBeVisible();
  await expect(row.locator(".hdiff-btn")).toHaveCount(0);
  const dl = row.getByRole("link", { name: /^Download .* as of/ });
  await expect(dl).toBeVisible();
  await expect(dl).toHaveAttribute("href", /blob\?sha=[0-9a-f]{64}&name=[^&]+&download=1$/);
  await row.getByRole("button", { name: /^Open .* as of/ }).click();
  await page.waitForURL(new RegExp(`/${pid}/notes/.*\\?v=[0-9a-f]{64}$`));
  await expect(page.locator(".vbanner")).toBeVisible();
});

// BEA-44: the viewer used to decide on the extension, so every extensionless
// file an agent wrote — Dockerfile, LICENSE, .bdriveignore — hit a dead
// "No preview" card. It decides on the bytes now, and renders PDFs.

test("an extensionless UTF-8 file previews as text", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.request.put(`/api/p/${pid}/upload/content?path=sniff/Dockerfile`, {
    data: "FROM alpine\nRUN apk add --no-cache curl\n",
  });
  await page.goto(`/${pid}/sniff/Dockerfile`);
  await expect(page.locator("#content pre.plain")).toContainText("RUN apk add --no-cache curl");
});

test("an unlisted extension previews as text too", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.request.put(`/api/p/${pid}/upload/content?path=sniff/main.tf`, {
    data: 'resource "aws_s3_bucket" "b" {}\n',
  });
  await page.goto(`/${pid}/sniff/main.tf`);
  await expect(page.locator("#content pre.plain")).toContainText("aws_s3_bucket");
});

test("binary bytes get the no-preview card, never dumped into the page", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.request.put(`/api/p/${pid}/upload/content?path=sniff/model.bin`, {
    data: Buffer.from([0x89, 0x50, 0x00, 0x01, 0x02, 0xff, 0xfe]),
  });
  await page.goto(`/${pid}/sniff/model.bin`);
  const card = page.locator("#content .filecard");
  await expect(card).toContainText("No preview for this file type.");
  await expect(page.locator("#content pre.plain")).toHaveCount(0);
  await expect(card.getByRole("link", { name: "Download" })).toHaveAttribute(
    "href",
    /download\?path=sniff%2Fmodel\.bin$/,
  );
});

test("a text file past the 1 MB cap says so instead of loading it", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.request.put(`/api/p/${pid}/upload/content?path=sniff/huge.dat`, {
    data: "x".repeat((1 << 20) + 1024),
  });
  await page.goto(`/${pid}/sniff/huge.dat`);
  await expect(page.locator("#content .filecard")).toContainText(/Too large to preview \(1\.0 MB\)/);
});

test("a pdf renders in the browser's viewer, in the wide column", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  // Smallest thing Chromium's viewer accepts; the assertion is the frame,
  // not the glyphs.
  const pdf =
    "%PDF-1.4\n1 0 obj<</Type/Catalog/Pages 2 0 R>>endobj\n" +
    "2 0 obj<</Type/Pages/Kids[3 0 R]/Count 1>>endobj\n" +
    "3 0 obj<</Type/Page/Parent 2 0 R/MediaBox[0 0 200 200]>>endobj\n" +
    "trailer<</Root 1 0 R>>\n%%EOF\n";
  await page.request.put(`/api/p/${pid}/upload/content?path=sniff/report.pdf`, { data: pdf });
  await page.goto(`/${pid}/sniff/report.pdf`);
  const frame = page.locator("#content iframe.pdfview");
  await expect(frame).toBeVisible();
  await expect(frame).toHaveAttribute("src", /file\?path=sniff%2Freport\.pdf$/);
  // No sandbox attribute: the PDF viewer is not this page's JS realm, and
  // sandboxing without allow-same-origin breaks Firefox's pdf.js.
  await expect(frame).not.toHaveAttribute("sandbox", /./);
  await expect(page.locator(".page.wide")).toBeVisible();
});

test("an old version of an extensionless file previews the same way", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  const url = `/api/p/${pid}/upload/content?path=sniff/LICENSE`;
  await page.request.put(url, { data: "MIT License — the first draft.\n" });
  await page.request.put(url, { data: "Apache 2.0 — the second draft.\n" });
  await page.goto(`/${pid}/history/sniff/LICENSE`);
  const older = page.locator(".hentry.add");
  await expect(older).toBeVisible();
  await older.getByRole("button", { name: /^Open .* as of/ }).click();
  await page.waitForURL(new RegExp(`/${pid}/sniff/LICENSE\\?v=[0-9a-f]{64}$`));
  await expect(page.locator("#content pre.plain")).toContainText("the first draft");
  // A bad sha still explains itself rather than previewing nothing.
  await page.goto(`/${pid}/sniff/LICENSE?v=${"0".repeat(64)}`);
  await expect(page.locator("#content .empty")).toContainText("That version isn't available.");
});

// BEA-83. A deep link to a project id you can't see used to swap in another
// project and throw the path away, with nothing on screen to say so.
test("a bogus project deep link says so and keeps the URL", async ({ page }) => {
  await login(page);
  await page.goto("/no-such-project-xyz/some/file.md");
  await expect(page.locator("#content .empty")).toContainText("Project not found");
  expect(page.url()).toContain("/no-such-project-xyz/some/file.md");
  await expect(page.locator("#sidebar")).toBeVisible();
  await page.reload(); // no bounce, no loop
  await expect(page.locator("#content .empty")).toContainText("Project not found");
  expect(page.url()).toContain("/no-such-project-xyz/some/file.md");
  // The two other URL rewrites off current.id must not undo the fix either.
  await page.goto("/no-such-project-xyz/insights");
  expect(page.url()).toContain("/no-such-project-xyz/insights");
  await page.goto("/no-such-project-xyz/notes/");
  expect(page.url()).toContain("/no-such-project-xyz/notes/");
});

// BEA-81: an old URL for a file that has since been renamed or dragged into
// a folder still lands on the file, rewrites itself, and says what happened.
test("a moved file's old URL redirects and says so", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/old-guide.md`);
  await page.waitForURL(`/${pid}/archive/moved-guide.md`);
  await expect(page.locator("#content")).toContainText("This file has been moved");
  await expect(page.locator(".vbanner")).toContainText("Moved from old-guide.md");
  // replace, not push: Back must not bounce off the dead URL forever.
  await expect(page.locator(".notfound")).toHaveCount(0);
});

test("a path that never existed still gets the not-found card", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/nothing-here.md`);
  await expect(page.locator(".notfound")).toBeVisible();
  await expect(page.locator(".vbanner")).toHaveCount(0);
});

// BEA-74: .csv/.tsv render as a table, and anything the parser can't make a
// table of stays the plain-text view it is today.

const SALES_CSV = [
  `region,rep,quarter,"revenue (usd)",notes`,
  `EMEA,"Ortiz, Ana",Q1,128400,steady growth in the enterprise segment`,
  `APAC,"Chen, Wei",Q2,96250,"he said ""ship it"" on Friday"`,
  `LATAM,"Silva, Joao",Q3,74100,"two lines\nin one cell"`,
  `NA,"Baker, Sam",Q4,181900`,
  ``,
].join("\n");

test("csv renders as a table: quoting, embedded newlines, a ragged row", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.request.put(`/api/p/${pid}/upload/content?path=csv/sales.csv`, { data: SALES_CSV });

  await page.goto(`/${pid}/csv/sales.csv`);
  const table = page.locator("#content table.csvview");
  await expect(table).toBeVisible();
  await expect(page.locator("#content pre.plain")).toHaveCount(0);
  await expect(table.locator("thead th")).toHaveCount(5);
  await expect(table.locator("thead th").nth(3)).toHaveText("revenue (usd)");
  await expect(table.locator("tbody tr")).toHaveCount(4);
  // A quoted comma is one cell, not two.
  await expect(table.locator("tbody tr").first().locator("td").nth(1)).toHaveText("Ortiz, Ana");
  // "" is one literal quote.
  await expect(table.locator("tbody tr").nth(1).locator("td").nth(4)).toHaveText(
    'he said "ship it" on Friday',
  );
  // A newline inside quotes is one cell in one row, not a second row.
  // textContent, not toHaveText: the latter normalizes away the very
  // newline this case exists to prove survived.
  expect(
    await table
      .locator("tbody tr")
      .nth(2)
      .locator("td")
      .nth(4)
      .evaluate((el) => el.textContent),
  ).toBe("two lines\nin one cell");
  // The short last row keeps its columns and pads the missing one.
  const last = table.locator("tbody tr").last().locator("td");
  await expect(last).toHaveCount(5);
  await expect(last.nth(3)).toHaveText("181900");
  await expect(last.nth(4)).toHaveText("");
});

test("tsv gets the same table, by extension and not by sniffing", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.request.put(`/api/p/${pid}/upload/content?path=csv/hosts.tsv`, {
    data: "host\trole\nalpha\tweb\nbeta\tdb\n",
  });
  await page.goto(`/${pid}/csv/hosts.tsv`);
  await expect(page.locator("#content table.csvview thead th")).toHaveCount(2);
  await expect(page.locator("#content table.csvview tbody tr")).toHaveCount(2);
});

test("a csv the parser can't read falls back to today's plain text", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  // Unterminated quote: not something to guess at.
  await page.request.put(`/api/p/${pid}/upload/content?path=csv/broken.csv`, {
    data: 'a,b\n"never closed,2\n',
  });
  await page.goto(`/${pid}/csv/broken.csv`);
  await expect(page.locator("#content pre.plain")).toContainText("never closed");
  await expect(page.locator("#content table.csvview")).toHaveCount(0);

  // No delimiter at all is prose, not a one-column table.
  await page.request.put(`/api/p/${pid}/upload/content?path=csv/prose.csv`, {
    data: "just some prose\nover two lines\n",
  });
  await page.goto(`/${pid}/csv/prose.csv`);
  await expect(page.locator("#content pre.plain")).toContainText("just some prose");
  await expect(page.locator("#content table.csvview")).toHaveCount(0);
});

test("a past version of a csv is a table too", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  const url = `/api/p/${pid}/upload/content?path=csv/versioned.csv`;
  await page.request.put(url, { data: "a,b\n1,2\n" });
  await page.request.put(url, { data: "a,b\n3,4\n" });
  await page.goto(`/${pid}/history/csv/versioned.csv`);
  const older = page.locator(".hentry.add");
  await expect(older).toBeVisible();
  await older.getByRole("button", { name: /^Open .* as of/ }).click();
  await page.waitForURL(new RegExp(`/${pid}/csv/versioned\\.csv\\?v=[0-9a-f]{64}$`));
  await expect(page.locator("#content table.csvview tbody")).toContainText("1");
});

test("a csv past the row cap says how many rows it left out", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  const big = "n,sq\n" + Array.from({ length: 5200 }, (_, i) => `${i},${i * i}`).join("\n") + "\n";
  await page.request.put(`/api/p/${pid}/upload/content?path=csv/big.csv`, { data: big });
  await page.goto(`/${pid}/csv/big.csv`);
  await expect(page.locator("#content table.csvview tbody tr")).toHaveCount(4999); // 5,000 incl. header
  await expect(page.locator("#content .csvnote")).toContainText("showing 5,000 of 5,201 rows");
});

test("a wide csv scrolls inside its own box at 390px", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  const cols = Array.from({ length: 14 }, (_, i) => `column_heading_number_${i}`);
  const wide = [cols.join(","), cols.map((_, i) => `value-${i}-with-some-length`).join(",")].join(
    "\n",
  );
  await page.request.put(`/api/p/${pid}/upload/content?path=csv/wide.csv`, { data: wide + "\n" });
  await page.setViewportSize({ width: 390, height: 780 });
  await page.goto(`/${pid}/csv/wide.csv`);
  const box = page.locator("#content .csvbox");
  await expect(box).toBeVisible();
  expect(await box.evaluate((el) => getComputedStyle(el).overflowX)).toBe("auto");
  // The box takes the sideways scroll; the page body never does.
  expect(await box.evaluate((el) => el.scrollWidth > el.clientWidth)).toBe(true);
  expect(
    await page.evaluate(() => document.documentElement.scrollWidth > window.innerWidth + 1),
  ).toBe(false);
});

test("mermaid: a good fence renders, a broken one keeps its code block", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/diagram.md`);
  // The valid fence became a diagram...
  const svg = page.locator("#content .mermaid-diagram svg");
  await expect(svg).toHaveCount(1);
  await expect(svg).toContainText("Teammate");
  // ...and the broken one below it kept today's <pre><code> plus a note. One
  // bad fence must not take the good one on the same page down with it.
  await expect(page.locator("#content pre code.language-mermaid")).toHaveCount(1);
  await expect(page.locator("#content .mermaid-err")).toHaveText("Couldn't render this diagram.");
  // ...with the parser's own message under it, so the author can fix the fence.
  const detail = page.locator("#content .mermaid-err-detail");
  await expect(detail).toHaveCount(1);
  // Structure, not wording: mermaid is a ^ range and its expected-token list
  // is the library's, so pinning the exact string would make a minor bump red.
  await expect(detail).toContainText(/line \d+/i);
  // The message quotes the author's source, and this string is mounted through
  // dangerouslySetInnerHTML: the tag has to arrive as text and stay inert.
  await expect(detail).toContainText("<img onerror=x>");
  await expect(page.locator("#content img")).toHaveCount(0);
  // A long expected-token list scrolls inside its own box; the page does not.
  expect(await detail.evaluate((el) => getComputedStyle(el).overflowX)).not.toBe("visible");
  expect(await detail.evaluate((el) => el.scrollWidth > el.clientWidth)).toBe(true);
  expect(
    await page.evaluate(() => document.documentElement.scrollWidth > window.innerWidth + 1),
  ).toBe(false);
});

test("a file with no mermaid fence fetches no mermaid chunk", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  // mermaid.core is the library; the *Diagram-* chunks are its per-grammar
  // splits. lib/mermaid.ts itself is a static import of the app entry (a few
  // KB of gate, no mermaid code in it), which is exactly why the gate is a
  // plain string check and not something that has to load mermaid to answer.
  const fetched: string[] = [];
  page.on("request", (r) => /mermaid\.core|Diagram-/.test(r.url()) && fetched.push(r.url()));
  await page.goto(`/${pid}/guide.md`);
  await expect(page.locator("#content")).toContainText("Second version");
  await page.waitForTimeout(500);
  expect(fetched).toEqual([]);
});

test("a shared diagram renders on the public page, without one there is no script", async ({
  page,
}) => {
  await login(page);
  const pid = await wikiId(page);
  const mint = async (path: string) =>
    (await (await page.request.post(`/api/p/${pid}/shares`, { data: { path } })).json()).token;

  // Diagram-free share pages stay the zero-JavaScript document they were.
  const plain = await page.request.get(`/s/${await mint("index.md")}`);
  expect(await plain.text()).not.toContain("<script");

  await page.goto(`/s/${await mint("diagram.md")}`);
  const svg = page.locator(".mermaid-diagram svg");
  await expect(svg).toHaveCount(1);
  await expect(svg).toContainText("Teammate");
  await expect(page.locator(".mermaid-err")).toHaveText("Couldn't render this diagram.");
  // The diagnostics ride along on the share page too — share-mermaid.ts needs
  // no change of its own, which is exactly what asserting it here proves. The
  // share shell has its own inline CSS, so this also catches a rule that only
  // ever landed in the app's stylesheet.
  const detail = page.locator(".mermaid-err-detail");
  await expect(detail).toContainText(/line \d+/i);
  await expect(detail).toContainText("<img onerror=x>");
  await expect(page.locator("img")).toHaveCount(0);
  expect(await detail.evaluate((el) => getComputedStyle(el).whiteSpace)).toBe("pre");
});

/* BEA-61: a read count that doesn't say your own views are in it reads as
   other people's interest. Every surface printing one discloses it — the file
   header can't do it visibly (#meta is nowrap + ellipsis), so it does it by
   hover text and a screen-reader span. */
test("read counts disclose that your own views count", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);

  await page.goto(`/${pid}/index.md`);
  const heat = page.locator("#meta span[title]");
  await expect(heat).toContainText("/ 30d");
  await expect(heat).toHaveAttribute("title", /Includes your own views\./);
  await expect(heat.locator(".sr-only")).toContainText("10 minutes count once");

  // The folder page says it once, out loud, for the summary and every row.
  await page.goto(`/${pid}/notes`);
  await expect(page.locator(".dl-heatnote")).toHaveText(
    "Includes your own views. Repeat opens by the same reader inside 10 minutes count once.",
  );
});

/* BEA-153: Copy. Download's counterpart — the same bytes, to the clipboard.
   The whole point is that it is SOURCE, so every assertion here reads the
   clipboard and looks for markdown syntax the rendered DOM does not contain.
   Chromium treats http://localhost as a secure context, so navigator.clipboard
   exists; the permission grant is the only thing standing between these specs
   and copyText() returning false by design. */

const COPY_MD = ["# Copy me", "", "| col | val |", "| --- | --- |", "| a | 1 |", "", "```sh", "echo hi", "```", ""].join(
  "\n",
);

const clip = (page: import("@playwright/test").Page) =>
  page.evaluate(() => navigator.clipboard.readText());

test("Copy puts a markdown file's raw source on the clipboard", async ({ page, context }) => {
  await context.grantPermissions(["clipboard-read", "clipboard-write"]);
  await login(page);
  const pid = await wikiId(page);
  await page.request.put(`/api/p/${pid}/upload/content?path=copy/doc.md`, { data: COPY_MD });

  await page.goto(`/${pid}/copy/doc.md`);
  await expect(page.locator("#content h1")).toHaveText("Copy me");
  await page.click("#more-btn");
  await page.click("#more-menu .more-item:has-text('Copy')");
  await expectToast(page, "Copied copy/doc.md");
  // Source, not the rendered DOM: the heading marker, the table pipes and the
  // fence all survive. The page itself shows an <h1>, a <table> and a <pre> —
  // none of which contain these characters.
  expect(await clip(page)).toBe(COPY_MD);
});

test("Copy on a .csv copies the delimited source, not the rendered table", async ({
  page,
  context,
}) => {
  await context.grantPermissions(["clipboard-read", "clipboard-write"]);
  await login(page);
  const pid = await wikiId(page);
  await page.request.put(`/api/p/${pid}/upload/content?path=copy/rows.csv`, { data: SALES_CSV });

  await page.goto(`/${pid}/copy/rows.csv`);
  await expect(page.locator("#content table.csvview")).toBeVisible();
  await page.click("#more-btn");
  await page.click("#more-menu .more-item:has-text('Copy')");
  await expectToast(page, "Copied copy/rows.csv");
  expect(await clip(page)).toBe(SALES_CSV);
});

test("Copy runs from the palette too", async ({ page, context }) => {
  await context.grantPermissions(["clipboard-read", "clipboard-write"]);
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/guide.md`);
  await expect(page.locator("#content")).toContainText("Second version");
  await page.keyboard.press("ControlOrMeta+k");
  await expect(page.locator("#palette")).toBeVisible();
  await page.fill("#palette input", "Copy: guide.md");
  await page.locator("#palette [cmdk-item]", { hasText: "Copy: guide.md" }).first().click();
  await expectToast(page, "Copied guide.md");
  expect(await clip(page)).toContain("# Guide");
  expect(await clip(page)).toContain("Second version");
});

test("Copy on a pinned version yields THAT version's bytes", async ({ page, context }) => {
  await context.grantPermissions(["clipboard-read", "clipboard-write"]);
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/history/guide.md`);
  // The oldest row is the first version; the current file holds the second.
  await page.locator(".hentry.add").click();
  await page.waitForURL(new RegExp(`/${pid}/guide\\.md\\?v=[0-9a-f]{64}$`));
  await expect(page.locator("#content")).toContainText("First version");

  await page.click("#more-btn");
  await page.click("#more-menu .more-item:has-text('Copy')");
  await expectToast(page, "Copied guide.md");
  const text = await clip(page);
  expect(text).toContain("First version");
  // The current file says "Second version" — a ⋯ menu that mixed the two is
  // exactly what BEA-7 fixed for Download.
  expect(text).not.toContain("Second version");
});

test("Copy is absent for an image and for HTML", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.request.put(`/api/p/${pid}/upload/content?path=copy/page.html`, {
    data: "<h1>hello</h1>",
  });

  for (const path of ["assets/logo.png", "copy/page.html"]) {
    await page.goto(`/${pid}/${path}`);
    await page.click("#more-btn");
    await expect(page.locator("#more-menu .more-item:has-text('Download')")).toBeVisible();
    await expect(page.locator("#more-menu .more-item:has-text('Copy')")).toHaveCount(0);
    await page.keyboard.press("Escape");
  }
  // …and the palette does not offer it either, on the page that has no menu item.
  await page.keyboard.press("ControlOrMeta+k");
  await expect(page.locator("#palette")).toBeVisible();
  await page.fill("#palette input", "Copy: copy/page.html");
  await expect(page.locator("#palette [cmdk-item]", { hasText: "Copy: copy/page.html" })).toHaveCount(0);
});

test("a clipboard that isn't there gets a failure toast, not a success one", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  // What an http:// self-host looks like from the page's side: no
  // navigator.clipboard, so copyText returns false by design. Without the
  // branch on that return value this is a "Copied" toast over an empty
  // clipboard — the failure mode the toast exists to prevent.
  await page.addInitScript(() => {
    Object.defineProperty(navigator, "clipboard", { get: () => undefined });
  });
  await page.goto(`/${pid}/index.md`);
  await page.click("#more-btn");
  await page.click("#more-menu .more-item:has-text('Copy')");
  await expectToast(page, /Copy failed .* secure \(https\) origin/);
  await expect(page.locator("#toast.show, [data-sonner-toast]").filter({ hasText: "Copied" })).toHaveCount(0);
});

test("a read-only member can copy — it is a read, like Download", async ({ page, context }) => {
  await context.grantPermissions(["clipboard-read", "clipboard-write"]);
  await login(page, READER);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/index.md`);
  await page.click("#more-btn");
  await page.click("#more-menu .more-item:has-text('Copy')");
  await expectToast(page, "Copied index.md");
  expect(await clip(page)).toContain("# Wiki");
});

/* BEA-155: the scroll restorer. Reading a file is never interrupted by a
   background refresh — the read-count poll used to call onRendered through
   MarkdownView's meta effect, and the restorer read that as "content landed"
   and re-applied scrollTo(0) mid-read. The retire-on-user-scroll rule that
   fixes it is unit-tested in src/lib/scroll.test.ts (its trigger is a 60s
   poll, longer than this suite's timeout); these two cover the paths a
   refactor of the restorer breaks. Scroll #content — the document itself
   never scrolls (helpers.ts). */
const LONG_DOC =
  "# Long read\n\n" + Array.from({ length: 200 }, (_, i) => `Paragraph ${i} of the long read.`).join("\n\n");

test("a fresh navigation lands at the top of the new file", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.request.put(`/api/p/${pid}/upload/content?path=scroll/long.md`, { data: LONG_DOC });

  await page.goto(`/${pid}/scroll/long.md`);
  await expect(page.locator("#content h1")).toHaveText("Long read");
  const content = page.locator("#content");
  await content.evaluate((el) => el.scrollTo({ top: 1200, behavior: "instant" }));
  expect(await content.evaluate((el) => el.scrollTop)).toBeGreaterThan(1000);

  // #content persists across routes, so its carried-over offset must be reset.
  await page.click('#tree .row[data-path="index.md"]');
  await page.waitForURL(`/${pid}/index.md`);
  await expect(page.locator("#content h1")).toHaveText("Wiki");
  await expect
    .poll(() => content.evaluate((el) => el.scrollTop), { timeout: 5000 })
    .toBeLessThanOrEqual(2);
});

test("back returns to the remembered offset, not to the top", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.request.put(`/api/p/${pid}/upload/content?path=scroll/long.md`, { data: LONG_DOC });

  await page.goto(`/${pid}/scroll/long.md`);
  await expect(page.locator("#content h1")).toHaveText("Long read");
  const content = page.locator("#content");
  await content.evaluate((el) => el.scrollTo({ top: 1200, behavior: "instant" }));

  await page.click('#tree .row[data-path="index.md"]');
  await page.waitForURL(`/${pid}/index.md`);
  await page.goBack();
  await page.waitForURL(`/${pid}/scroll/long.md`);
  await expect(page.locator("#content h1")).toHaveText("Long read");
  await expect
    .poll(() => content.evaluate((el) => el.scrollTop), { timeout: 5000 })
    .toBeGreaterThan(1000);
});

/* BEA-154: a doc's YAML frontmatter used to be a table pinned to the top of
   the reading column, pushing the document below the fold. It is a panel
   beside the prose now — a rail on a wide window, a closed disclosure on a
   phone — and the reading column starts with the document. */
test("frontmatter is a side panel, not a slab on top of the document", async ({ page }) => {
  // Wide enough for the rail: the breakpoint is arithmetic (style.css), and
  // the default 1280 viewport is deliberately below it.
  await page.setViewportSize({ width: 1440, height: 900 });
  await login(page);
  const pid = await wikiId(page);
  // Seeded at runtime so no other spec's file counts move.
  const put = async (path: string, body: string) => {
    const r = await page.request.put(
      `/api/p/${pid}/upload/content?path=${encodeURIComponent(path)}`,
      { data: body },
    );
    expect(r.ok(), `seeding ${path}: ${r.status()}`).toBeTruthy();
  };
  await put(
    "meta/props.md",
    "---\ntitle: Q3 findings\nstatus: draft\ntags: [churn, revenue]\nmeta:\n  reviewed: true\n---\n\n# Q3 findings\n\nBody text.\n",
  );

  await page.goto(`/${pid}/meta/props.md`);
  const panel = page.locator("#content .fmpanel");
  await expect(panel).toBeVisible();
  // The document leads: the h1 is the first thing in the prose column, and
  // no frontmatter table survives inside the rendered markdown.
  await expect(page.locator("#content h1")).toHaveText("Q3 findings");
  await expect(page.locator("#content table.frontmatter")).toHaveCount(0);
  expect(
    await page.evaluate(() => {
      const h1 = document.querySelector("#content h1") as HTMLElement;
      return h1.getBoundingClientRect().top;
    }),
  ).toBeLessThan(
    await panel.evaluate((el) => el.getBoundingClientRect().bottom),
  );
  // Same keys, author order, nested value still compact YAML in <code>.
  await expect(panel.locator("dt")).toHaveText(["title", "status", "tags", "meta"]);
  await expect(panel.locator("dd").nth(2)).toHaveText("churn, revenue");
  await expect(panel.locator("dd code")).toHaveText("reviewed: true");
  // A rail, not a squeezed column: the prose keeps its 768px measure and the
  // panel sits to its right.
  const geom = await page.evaluate(() => {
    const prose = document.querySelector("#content .markdown > div:not(.fmpanel)") as HTMLElement;
    const p = document.querySelector(".fmpanel") as HTMLElement;
    return { prose: prose.getBoundingClientRect(), panel: p.getBoundingClientRect() };
  });
  expect(Math.round(geom.prose.width), "prose measure unchanged").toBe(768);
  expect(geom.panel.left, "panel is to the right of the prose").toBeGreaterThanOrEqual(
    geom.prose.right,
  );

  // Collapsing is remembered — across a different file, and across a reload.
  await panel.locator("summary").click();
  await expect(panel).not.toHaveAttribute("open", /.*/);
  await page.goto(`/${pid}/index.md`);
  await expect(page.locator("#content .fmpanel")).toHaveCount(0); // no frontmatter, no panel
  await page.goto(`/${pid}/meta/props.md`);
  await expect(page.locator("#content .fmpanel")).not.toHaveAttribute("open", /.*/);
  await page.reload();
  await expect(page.locator("#content .fmpanel")).not.toHaveAttribute("open", /.*/);
});

test("frontmatter panel on a phone: closed disclosure above the body", async ({ browser }) => {
  const ctx = await browser.newContext({ viewport: { width: 390, height: 844 } });
  const page = await ctx.newPage();
  await login(page);
  const pid = await wikiId(page);
  await page.request.put(`/api/p/${pid}/upload/content?path=meta/phone.md`, {
    data: "---\ntitle: Q3\nowner: snow\n---\n\n# Q3\n\nBody.\n",
  });
  await page.goto(`/${pid}/meta/phone.md`);
  const panel = page.locator("#content .fmpanel");
  await expect(panel).toBeVisible();
  // No stored choice yet: a phone has no room for a rail, so it opens closed
  // and sits above the body rather than beside it.
  await expect(panel).not.toHaveAttribute("open", /.*/);
  const m = await page.evaluate(() => {
    const p = document.querySelector(".fmpanel") as HTMLElement;
    const h1 = document.querySelector("#content h1") as HTMLElement;
    return {
      above: p.getBoundingClientRect().bottom <= h1.getBoundingClientRect().top,
      overflow: document.documentElement.scrollWidth > document.documentElement.clientWidth,
    };
  });
  expect(m.above, "390px: panel sits above the body").toBe(true);
  expect(m.overflow, "390px: horizontal page scroll").toBe(false);
  await ctx.close();
});
