import { test, expect } from "@playwright/test";
import { login, wikiId } from "./helpers";

// A missing quadrant label is invisible to every check except this one.
test("reads × freshness names all four quadrants", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/dashboard`);
  await expect(page.locator(".in-chart .in-quad")).toHaveText([
    "hot + stale",
    "hot + fresh",
    "cold + stale",
    "cold + fresh",
  ]);
});

/* Three defects on one chart (BEA-60): the busiest dot sat half outside the
   frame, no dot said which file it was, and the size caption was drawn on top
   of the "hot + stale" label. The frame numbers come off the viewBox rather
   than being repeated here. */
test("every dot sits inside the frame, and the hot+stale ones say which file they are", async ({
  page,
}) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/dashboard`);

  const svg = page.locator("svg.in-chart:not(.in-treemap):not(.in-matrix)");
  const [, , W, H] = (await svg.getAttribute("viewBox"))!.split(" ").map(Number);
  const M = { l: 44, r: 16, t: 20, b: 34 }; // Scatter's margins

  const dots = await svg.locator(".in-pt").evaluateAll((els) =>
    els.map((e) => ({
      cx: +e.getAttribute("cx")!,
      cy: +e.getAttribute("cy")!,
      r: +e.getAttribute("r")!,
    })),
  );
  expect(dots.length).toBeGreaterThan(0);
  // The repro: the hottest file landed at cy === M.t with r up to 7.
  expect(Math.min(...dots.map((d) => d.cy - d.r))).toBeGreaterThanOrEqual(M.t);
  expect(Math.max(...dots.map((d) => d.cx + d.r))).toBeLessThanOrEqual(W - M.r);
  expect(Math.max(...dots.map((d) => d.cy + d.r))).toBeLessThanOrEqual(H - M.b);
  expect(Math.min(...dots.map((d) => d.cx - d.r))).toBeGreaterThanOrEqual(M.l);

  // The seeded archive/ files are hot and months old — the danger quadrant.
  const labels = svg.locator(".in-pt-label");
  const names = await labels.allTextContents();
  expect(names.length).toBeGreaterThan(0);
  expect(names.length).toBeLessThanOrEqual(6);
  expect(names).toContain("retired-spec.md"); // basename, not the full path
  for (const n of names) {
    await expect(page.locator(".in-hp-row", { hasText: n })).not.toHaveCount(0);
  }
  // Inside the frame, and no two labels on the same baseline.
  const boxes = await labels.evaluateAll((els) =>
    els.map((e) => e.getBoundingClientRect()),
  );
  const plot = (await svg.boundingBox())!;
  const s = plot.width / W; // viewBox units → screen px
  for (const b of boxes) {
    expect(b.left).toBeGreaterThanOrEqual(plot.x + M.l * s - 1);
    expect(b.right).toBeLessThanOrEqual(plot.x + (W - M.r) * s + 1);
    expect(b.top).toBeGreaterThanOrEqual(plot.y + M.t * s - 1);
    expect(b.bottom).toBeLessThanOrEqual(plot.y + (H - M.b) * s + 1);
  }
  for (let i = 0; i < boxes.length; i++)
    for (let j = i + 1; j < boxes.length; j++)
      expect(
        boxes[i].right < boxes[j].left ||
          boxes[j].right < boxes[i].left ||
          boxes[i].bottom < boxes[j].top ||
          boxes[j].bottom < boxes[i].top,
      ).toBe(true);

  // A dot keeps its tooltip and its click even where a label now sits.
  await expect(svg.locator(".in-pt").first().locator("title")).toHaveCount(1);
  await expect(svg.locator(".in-pt.danger").first()).toBeVisible();

  // The caption moved out of the plot, where it overprinted "hot + stale".
  await expect(svg).not.toContainText("dot size");
  await expect(page.locator(".in-cap")).toHaveText("dot size = agent share of reads");
});

/* A heat row outlives its file. Before this, the file panels joined heat onto
   the tree and dropped whatever didn't match — so the page could say "no reads"
   while the agent-coverage panel beside it rendered those same reads. */
test("reads for a deleted file are ranked and labelled, not dropped", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/dashboard`);

  const row = page.locator(".in-hp-row", { hasText: "scratch.md" });
  await expect(row).toHaveCount(1); // seeded with reads, deleted by the seed
  await expect(row.locator(".in-hp-gone")).toHaveText("· no longer in the project");
  await expect(page.locator(".insights")).not.toContainText("No reads in the window yet");
  await expect(page.locator(".in-orphan-note")).toHaveText([
    "1 file with reads is no longer in the project — see Hot path.",
    "1 file with reads is no longer in the project — see Hot path.",
  ]);

  // The file view would 404 on it; history still has the content.
  await row.click();
  await expect(page).toHaveURL(new RegExp(`/${pid}/history/scratch\\.md$`));
});

// The footnote counts what Hot path will actually list, per lens — scratch.md
// has human reads only, so the agent lens has no orphan to report.
test("the orphan footnote follows the lens", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/dashboard`);
  await expect(page.locator(".in-orphan-note").first()).toBeVisible();
  await page.getByRole("button", { name: "Agent reads" }).click();
  await expect(page.locator(".in-orphan-note")).toHaveCount(0);
  await expect(page.locator(".in-hp-row", { hasText: "scratch.md" })).toHaveCount(0);
  await page.getByRole("button", { name: "Human reads" }).click();
  await expect(page.locator(".in-hp-row", { hasText: "scratch.md" })).toHaveCount(1);
});

// BEA-62: the page named three read types and filtered two. Share reads are
// the ones an owner most wants to isolate — traffic from links they minted.
test("the shared lens isolates share-link reads and paints them share-colored", async ({
  page,
}) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/dashboard`);
  await expect(page.locator(".in-lens-btn")).toHaveText([
    "All reads",
    "Human reads",
    "Agent reads",
    "Shared reads",
  ]);

  await page.getByRole("button", { name: "Shared reads" }).click();
  // notes/deep/topic.md carries the seed's only share reads. Other specs mint
  // and open their own links against this shared hub, so assert the filter's
  // invariant — share reads only — not a row count that moves with test order.
  const topic = page.locator(".in-hp-row", { hasText: "notes/deep/topic.md" });
  await expect(topic).toHaveCount(1);
  await expect(topic.locator(".in-hp-count")).toHaveText("3");

  // The bar is the whole point: under a share lens the reads must not be
  // painted as somebody else's traffic.
  await expect(topic.locator(".in-hp-share")).not.toHaveCSS("width", "0px");
  for (const cls of [".in-hp-agent", ".in-hp-human"]) {
    await expect(topic.locator(cls)).toHaveCSS("width", "0px");
  }
  await expect(page.locator(".in-sw.share")).toBeVisible();

  // Files read only by people or agents drop out entirely — the lens filters,
  // it does not merely re-sort. Neither path is shared by any spec.
  for (const p of ["archive/retired-spec.md", "scratch.md"]) {
    await expect(page.locator(".in-hp-row", { hasText: p })).toHaveCount(0);
  }

  // "All reads" still means human + agent + share.
  await page.getByRole("button", { name: "All reads" }).click();
  await expect(page.locator(".in-hp-row", { hasText: "archive/retired-spec.md" })).toHaveCount(1);
  await expect(topic).toHaveCount(1);
});

// The empty scope must stay empty: falling back to every file would be worse
// than showing nothing, because the number would silently mean something else.
test("a project with no share reads says so under the shared lens", async ({ page }) => {
  await login(page);
  const made = await (await page.request.post("/api/projects", { data: { name: "noshare" } })).json();
  try {
    await page.request.put(
      `/api/p/${made.project.id}/upload/content?path=a.md`,
      { data: "# A\n", headers: { "content-type": "text/markdown" } },
    );
    await page.goto(`/${made.project.id}/dashboard`);
    await page.getByRole("button", { name: "Shared reads" }).click();
    await expect(page.locator(".dl-empty")).toContainText("No reads in the window yet.");
    await expect(page.locator(".in-hp-row", { hasText: "a.md" })).toHaveCount(0);
  } finally {
    await page.request.delete("/api/projects/" + made.project.id);
  }
});

// A brand-new project used to draw ~840px of empty frames with the quadrant
// labels floating over nothing — the first screen every project shows.
// Created at runtime and deleted: a permanent fixture sorting before "wiki"
// would move where the app lands and break home.spec.ts.
test("a project with no files says so instead of drawing empty charts", async ({ page }) => {
  await login(page);
  const made = await (await page.request.post("/api/projects", { data: { name: "blank" } })).json();
  try {
    await page.goto(`/${made.project.id}/dashboard`);
    await expect(page.locator(".in-blank")).toContainText("no files");
    await expect(page.locator(".in-chart")).toHaveCount(0);
    await expect(page.locator("body")).not.toContainText("hot + stale");
    await expect(page.locator(".in-blank a")).toHaveAttribute(
      "href",
      `/${made.project.id}/install`,
    );
  } finally {
    await page.request.delete("/api/projects/" + made.project.id);
  }
});

/* BEA-68: the treemap used to paint the full green→red ramp even when every
   file was the same age, then the legend under it admitted the colour meant
   nothing. Three checks: the seeded project spans months and keeps its colours;
   an all-new project goes grey; and the flat test is per-scope, not per-project. */

const fills = (page: import("@playwright/test").Page) =>
  page.locator(".in-tm-cell").evaluateAll((els) => els.map((e) => e.getAttribute("fill")!));

test("a range worth colouring keeps its colours", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/dashboard`);
  await expect(page.locator(".in-tm-range")).toContainText("observed:");
  await expect(page.locator(".in-sw-age")).not.toHaveClass(/in-sw-flat/);
  // 2h-old files next to 210d-old ones: not one flat fill.
  expect(new Set(await fills(page)).size).toBeGreaterThan(1);
});

test("a project too young to rank goes grey instead of all-clear green", async ({ page }) => {
  await login(page);
  const made = await (await page.request.post("/api/projects", { data: { name: "brandnew" } })).json();
  const pid = made.project.id;
  try {
    for (const p of ["a.md", "b.md"]) {
      await page.request.put(`/api/p/${pid}/upload/content?path=${p}`, { data: `# ${p}\n` });
    }
    await page.goto(`/${pid}/dashboard`);
    await expect(page.locator(".in-treemap")).toBeVisible();
    const f = await fills(page);
    expect(f.length).toBe(2);
    expect(new Set(f).size).toBe(1); // one fill, and it is not on the ramp
    expect(f[0]).toBe("rgb(150,156,164)");
    await expect(page.locator(".in-tm-range")).toContainText("colour off");
    await expect(page.locator(".in-sw-age")).toHaveClass(/in-sw-flat/);
  } finally {
    await page.request.delete("/api/projects/" + pid);
  }
});

test("the flat test follows the scope, not the project", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  // notes/ holds only 90min- and 24h-old files, inside a project spanning 210d.
  await page.goto(`/${pid}/dashboard/notes`);
  await expect(page.locator(".in-tm-range")).toContainText("colour off");
  expect(new Set(await fills(page)).size).toBe(1);
  await page.goto(`/${pid}/dashboard`);
  await expect(page.locator(".in-tm-range")).toContainText("observed:");
});

// The other zero state, which was never broken: files that nobody has read
// still get a map, a scatter and a self-explaining hot path.
test("files with no reads still chart", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/dashboard`);
  await expect(page.locator(".in-treemap")).toBeVisible();
  await expect(page.locator(".in-blank")).toHaveCount(0);
});

// BEA-61: the Dashboard's advice is built on these counts, so its caption is
// the one place the disclosure has to survive a scope change too.
test("the reads × freshness caption says own views count, scoped or not", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  for (const route of [`/${pid}/dashboard`, `/${pid}/dashboard/notes`]) {
    await page.goto(route);
    await expect(page.locator(".insights > .dl-sub")).toContainText(
      "Includes your own views. Repeat opens by the same reader inside 10 minutes count once.",
    );
  }
});
