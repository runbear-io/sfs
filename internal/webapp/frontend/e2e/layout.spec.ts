import { test, expect, type Page } from "@playwright/test";
import { login, wikiId } from "./helpers";

/* The column system (shell.tsx <Page>, style.css .page): every route renders
   exactly one .page, and pages of the same width share the same column edges.
   These used to range from 560px to unbounded — half of them uncentered — so
   no two routes lined up. Widths live in CSS tokens; this asserts the routes
   actually resolve to them. */

const WIDTHS = { read: 768, app: 768 }; // both = Tailwind md; wide is viewport-capped at 1280

async function column(page: Page) {
  return page.evaluate(() => {
    const pages = document.querySelectorAll("#content > .page");
    if (pages.length !== 1) throw new Error(`expected 1 .page, got ${pages.length}`);
    const el = pages[0] as HTMLElement;
    const r = el.getBoundingClientRect();
    const kind = el.classList.contains("read") ? "read" : el.classList.contains("wide") ? "wide" : "app";
    return { kind, left: Math.round(r.left), width: Math.round(r.width) };
  });
}

test("every view shares one column system", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page); // the seeded project — other specs create their own
  const seen: Record<string, { left: number; width: number }> = {};

  const visit = async (path: string, want: "read" | "app" | "wide") => {
    await page.goto(`http://localhost:8993/${pid}${path}`);
    await page.waitForSelector("#content > .page");
    const col = await column(page);
    expect(col.kind, `${path || "/"} column kind`).toBe(want);
    if (want !== "wide") {
      expect(col.width, `${path || "/"} column width`).toBe(WIDTHS[want]);
    }
    // Same width class ⇒ identical edges, on every route.
    if (seen[want]) {
      expect(col.left, `${path || "/"} left edge matches other ${want} pages`).toBe(seen[want].left);
      expect(col.width, `${path || "/"} width matches other ${want} pages`).toBe(seen[want].width);
    } else {
      seen[want] = col;
    }
  };

  await visit("", "app"); // project home
  await visit("/install", "app"); // the same guide, so the same column
  await visit("/settings", "app");
  await visit("/dashboard", "app"); // charts cap their own measure; the column is normal
  await visit("/history", "app"); // structured view, not a file render
  await visit("/index.md", "read"); // rendered markdown — the only read surface
  await visit("/notes", "app"); // folder listing is a structured view too
});

test("the install route and the project home render the guide identically", async ({ page }) => {
  // They are two sidebar items apart and show the same component; /install
  // used to wrap it in the .onboard card — 320px narrower, 90px lower.
  await login(page);
  const pid = await wikiId(page);
  const box = async (path: string) => {
    await page.goto(`http://localhost:8993/${pid}${path}`);
    await page.waitForSelector(".guide");
    return page.evaluate(() => {
      const r = (document.querySelector(".guide") as HTMLElement).getBoundingClientRect();
      return { left: Math.round(r.left), width: Math.round(r.width), top: Math.round(r.top) };
    });
  };
  expect(await box("/install")).toEqual(await box(""));
});

test("charts never scale past the size they were drawn at", async ({ page }) => {
  // .in-chart SVGs are viewBox="0 0 720 …" at width:100%, so an unbounded
  // column magnifies them — labels ended up larger than the page title.
  await page.setViewportSize({ width: 1600, height: 900 });
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`http://localhost:8993/${pid}/dashboard`);
  await page.waitForSelector(".in-chart");
  const worst = await page.evaluate(() => {
    let max = 0;
    for (const el of document.querySelectorAll(".in-chart")) {
      const vb = (el.getAttribute("viewBox") || "0 0 720 0").split(/\s+/);
      max = Math.max(max, el.getBoundingClientRect().width / Number(vb[2]));
    }
    return max;
  });
  expect(worst, "chart scale factor").toBeLessThanOrEqual(1.06);
});

test("the gutter belongs to the scroll container, not the column", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  // A markdown page used to carry the max-width on #content itself, so its
  // gutter came out of the reading measure and .md ran ~80px narrower than
  // every other page. The gutter must live on #content alone.
  for (const path of ["/index.md", "/notes", "/history"]) {
    await page.goto(`http://localhost:8993/${pid}${path}`);
    await page.waitForSelector("#content > .page");
    const r = await page.evaluate(() => {
      const c = document.querySelector("#content") as HTMLElement;
      const p = document.querySelector("#content > .page") as HTMLElement;
      return {
        contentMax: getComputedStyle(c).maxWidth,
        pad: getComputedStyle(c).paddingLeft,
        childMax: getComputedStyle(p.firstElementChild as HTMLElement).maxWidth,
      };
    });
    expect(r.contentMax, `${path}: #content must not constrain width`).toBe("none");
    expect(r.pad, `${path}: gutter`).toBe("40px");
    // Views may not re-declare a column of their own inside .page.
    expect(r.childMax, `${path}: view sets its own max-width`).toBe("none");
  }
});

/* BEA-79: Public links was invisible at 1440x900 — below the fold of #content,
   which is the app's only scroll container, and with overlay scrollbars nothing
   at rest said there was more. Two halves: the container now takes a real
   scrollbar (so it consumes width, so it is visible), and the security section
   sits second instead of fourth. */

test("settings shows Public links high, and #content shows it scrolls", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.setViewportSize({ width: 1440, height: 900 });
  await page.goto(`http://localhost:8993/${pid}/settings`);
  await expect(page.locator(".project-settings [data-slot=card-title]")).toHaveText([
    "General",
    "Public links",
    "People",
    "About",
    "Danger zone",
  ]);

  // A classic scrollbar takes layout width; an overlay one doesn't. This is
  // the affordance — no hover, no scroll event.
  const bar = await page.evaluate(() => {
    const c = document.querySelector("#content") as HTMLElement;
    return { gap: c.offsetWidth - c.clientWidth, overflows: c.scrollHeight > c.clientHeight };
  });
  expect(bar.overflows, "settings must overflow at 1440x900").toBe(true);
  expect(bar.gap, "#content reserves a visible scrollbar").toBeGreaterThan(0);
});

/* The who/when/how-hot line is what the product is differentiated by, and a
   phone is exactly when you're catching up — but ≤900px used to `display:
   none` it on the file view and ellipsise it to `claude-…` / `Alice <ali…` in
   History. Desktop values are read first and compared, rather than hard-coded:
   the seeded hub's read counts drift, so a literal would flake. */
test("provenance survives to a phone on the file view and in History", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  const runHead = page.locator(".hrun-head").first();

  const read = async () => {
    await page.goto(`/${pid}/index.md`);
    // Not waitForSelector: #meta is always in the DOM, empty until the file
    // loads and (before this fix) display:none below 900px.
    await expect(page.locator("#meta")).not.toBeEmpty();
    const meta = await page.locator("#meta").textContent();
    await page.goto(`/${pid}/history`);
    await page.waitForSelector(".hrun-head");
    return {
      meta,
      note: await runHead.locator(".hrun-note").textContent(),
      runMeta: await runHead.locator(".hrun-meta").textContent(),
      time: await runHead.locator(".hrun-time").textContent(),
    };
  };

  await page.setViewportSize({ width: 1200, height: 900 });
  const desktop = await read();
  expect(desktop.meta, "desktop provenance line").toBeTruthy();

  await page.setViewportSize({ width: 390, height: 844 });
  await page.goto(`/${pid}/index.md`);
  await expect(page.locator("#meta")).not.toBeEmpty();
  await expect(page.locator("#meta"), "390px: provenance line visible").toBeVisible();

  const m = await page.evaluate(() => {
    const bar = document.querySelector("#topbar") as HTMLElement;
    const meta = document.querySelector("#meta") as HTMLElement;
    const btns = [...bar.querySelectorAll<HTMLElement>(".btn, .icon-btn")].filter(
      (b) => b.getBoundingClientRect().width > 0,
    );
    const last = btns[btns.length - 1].getBoundingClientRect();
    const content = document.querySelector("#content") as HTMLElement;
    return {
      // Actions stay flush right on row 1, above the wrapped meta row.
      gapFromRight: Math.round(bar.getBoundingClientRect().right - last.right),
      actionsAboveMeta: last.bottom <= meta.getBoundingClientRect().top + 1,
      tap: Math.min(...btns.map((b) => b.getBoundingClientRect().height)),
      // Nothing clipped, and the grown topbar pushes content down rather
      // than overlapping it.
      clipped: meta.scrollWidth > meta.clientWidth + 1 || meta.scrollHeight > meta.clientHeight + 1,
      contentBelow: content.getBoundingClientRect().top >= bar.getBoundingClientRect().bottom - 1,
      overflow: document.documentElement.scrollWidth > document.documentElement.clientWidth,
    };
  });
  expect(m.gapFromRight, "390px: actions flush right").toBeLessThanOrEqual(10);
  expect(m.actionsAboveMeta, "390px: actions stayed on the first row").toBe(true);
  expect(m.tap, "390px: action tap target").toBeGreaterThanOrEqual(44);
  expect(m.clipped, "390px: provenance line clipped").toBe(false);
  expect(m.contentBelow, "390px: topbar overlaps the page").toBe(true);
  expect(m.overflow, "390px: horizontal page scroll").toBe(false);

  const mobile = await read();
  expect(mobile, "390px: same provenance as desktop").toEqual(desktop);

  const clip = await page.evaluate(() => {
    const head = document.querySelector(".hrun-head") as HTMLElement;
    const bad = (sel: string) => {
      const el = head.querySelector(sel) as HTMLElement;
      return el.scrollWidth > el.clientWidth + 1;
    };
    return { note: bad(".hrun-note"), meta: bad(".hrun-meta") };
  });
  expect(clip.note, "390px: run note clipped").toBe(false);
  expect(clip.meta, "390px: run meta clipped").toBe(false);

  // Routes with no provenance must not gain a blank strip: the topbar is
  // exactly its desktop height there.
  await page.goto(`/${pid}/notes`);
  await page.waitForSelector(".dl-row");
  const folder = await page.evaluate(() => ({
    metaShown: getComputedStyle(document.querySelector("#meta") as HTMLElement).display !== "none",
    barHeight: Math.round((document.querySelector("#topbar") as HTMLElement).getBoundingClientRect().height),
  }));
  expect(folder.metaShown, "390px: empty meta on a folder route").toBe(false);
  expect(folder.barHeight, "390px: folder topbar height").toBe(52);
});

/* Mobile folder rows used to drop .dl-meta entirely below 430px, leaving an
   unlabelled coloured dot as the only signal — and the dot's meaning lived in
   a title= that touch never shows and screen readers never read. The meta now
   wraps to a second line and the dot carries its own accessible name. */
test("folder rows keep their metadata, and the heat dot a name, on a phone", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  const file = page.locator('.dl-row[title="notes/readme.md"]');
  const dir = page.locator('.dl-row[title="notes/deep"]');

  await page.setViewportSize({ width: 431, height: 800 });
  await page.goto(`/${pid}/notes`);
  await page.waitForSelector(".dl-row");
  const desktopMeta = await file.locator(".dl-meta").textContent();

  for (const width of [360, 390, 430]) {
    await page.setViewportSize({ width, height: 800 });
    await page.goto(`/${pid}/notes`);
    await page.waitForSelector(".dl-row");

    // Same information as desktop: read count, size and date all present.
    await expect(file.locator(".dl-meta"), `${width}px: file meta`).toBeVisible();
    expect(await file.locator(".dl-meta").textContent(), `${width}px: file meta`).toBe(desktopMeta);
    await expect(file.locator(".dl-meta")).toContainText(/read/);
    await expect(dir.locator(".dl-meta"), `${width}px: folder meta`).toContainText("1 item");

    const m = await page.evaluate(() => {
      const rows = [...document.querySelectorAll<HTMLElement>(".dl-row")];
      const name = rows.map((r) => r.querySelector(".dl-name") as HTMLElement);
      return {
        truncated: name.some((n) => n.scrollWidth > n.clientWidth + 1),
        minHeight: Math.min(...rows.map((r) => r.getBoundingClientRect().height)),
        overflow: document.documentElement.scrollWidth > document.documentElement.clientWidth,
      };
    });
    expect(m.truncated, `${width}px: filename truncated`).toBe(false);
    expect(m.minHeight, `${width}px: row tap target`).toBeGreaterThanOrEqual(44);
    expect(m.overflow, `${width}px: horizontal page scroll`).toBe(false);

    // Accessibility tree, not pixels: every dot announces itself. (Read
    // counts accumulate across the shared hub, so match the shape not a
    // number — what matters is that no dot is nameless.)
    const dots = await page.locator(".heatdot").count();
    await expect(page.getByRole("img", { name: /read/ }), `${width}px: named dots`).toHaveCount(dots);
    // BEA-61: the dot's name also has to say what the count includes. Anchored
    // on the disclosure, not just the shape — the old unanchored regex would
    // have passed just as happily with the sentence dropped again.
    await expect(file.locator(".heatdot")).toHaveAttribute(
      "aria-label",
      /\d+ reads? .*in 30 days\. Includes your own views\..*10 minutes count once\./,
    );
  }
});
