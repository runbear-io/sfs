import { test, expect, type Page } from "@playwright/test";
import { login, wikiId, ADMIN, MEMBER } from "./helpers";
import { spawn, type ChildProcess } from "node:child_process";
import { mkdtempSync, mkdirSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

/* Round 13 — the frontend surfaces nobody has ever opened in a browser.
 *
 * Every assertion in this file first proves the page RENDERED. Round 11's
 * first cut passed against an empty pane; the rule that came out of it is that
 * a negative assertion ("the admin roster is not on this page") is worthless
 * without a positive one on the same page ("this page is the admin panel").
 * Each test states its control explicitly.
 *
 * Slug for helpers: sec13fe.
 */

// ---------------------------------------------------------------- helpers

// Paths the hub still ACCEPTS after round 12 closed C0/C1/bidi/zero-width at
// ingest (journal.SafeText). These are the ones left, and the palette, the
// tree and the dashboard have never seen any of them.
const sec13fePayloads: Array<{ name: string; seg: string; why: string }> = [
  {
    name: "U+0130 dotted capital I",
    seg: "İdeas.md",
    // "İ".toLowerCase() is TWO code units, so every index the palette's fuzzy
    // matcher computes against the lowercased string is off by one from the
    // string Highlight then slices.
    why: "toLowerCase() changes the string's length; the palette highlights by index",
  },
  {
    name: "combining marks (zalgo)",
    seg: "z" + "́".repeat(60) + "algo.md",
    why: "60 stacked marks on one grapheme; a row that paints outside its box",
  },
  {
    name: "RTL script, no override",
    seg: "ملف-report.md",
    why: "strong-RTL letters reorder the run with no control character at all",
  },
  {
    name: "Cyrillic homoglyph",
    seg: "guіde.md",
    why: "renders as guide.md next to the real guide.md",
  },
  {
    name: "NBSP",
    seg: "index .md",
    why: "renders as a space; index .md and index.md are one row apart",
  },
  {
    name: "long segment",
    seg: "L".repeat(200) + ".md",
    why: "200 chars in one label",
  },
  {
    name: "U+2028 line separator",
    seg: "line sep.md",
    why: "SafeText refuses C1 and the bidi block but not U+2028, which CSS Text treats as a forced break",
  },
];

// Seeds a file through the sync-side write door (the same one browse.spec.ts
// uses). Returns the paths the server actually accepted — a refusal here is a
// clean result, not a broken fixture, so it is reported rather than thrown.
async function sec13feSeed(page: Page, pid: string, path: string, body: string) {
  const res = await page.request.put(
    `/api/p/${pid}/upload/content?path=${encodeURIComponent(path)}`,
    { data: body },
  );
  return res.status();
}

// Waits until the SPA shell is on screen. Proof-of-render for every test here.
async function sec13feShell(page: Page) {
  await expect(page.locator("#sidebar")).toBeVisible();
}

// ================================================================== 1. HubSettings
//
// The CISO has flagged this twice: nobody has seen what it renders before the
// API answers, or on a 403. It is the hub's admin panel — signup policy,
// allowed domains, admin roster, pending approvals — and it is URL-less panel
// state opened from the account menu, which only appears for an admin.

test("HubSettings: the admin panel renders its roster for an admin (control)", async ({ page }) => {
  await login(page, ADMIN);
  await sec13feShell(page);
  await page.click("#account-btn");
  await page.click("#menu-hub-admin");
  // CONTROL — this is the panel, and it is populated. Every negative
  // assertion in the two tests below is measured against this.
  await expect(page.locator(".admin h1")).toHaveText("Signup & access");
  await expect(page.locator(".admin-item", { hasText: "Hub admins" })).toContainText(ADMIN);
  await expect(page.locator(".admin-item.toggle")).toHaveCount(2);
});

test("HubSettings: a member who forges the client-side admin flag gets an empty pane, not a roster", async ({
  page,
}) => {
  // The attacker model is a member driving their OWN browser: config.auth.admin
  // is a client-side boolean, so the menu entry is one devtools edit away. The
  // question is what the COMPONENT does once it is mounted without the rights.
  await page.route("**/api/config", async (route) => {
    const res = await route.fetch();
    const cfg = await res.json();
    cfg.auth.admin = true;
    await route.fulfill({ response: res, json: cfg });
  });
  const seen: number[] = [];
  page.on("response", (r) => {
    if (r.url().includes("/api/admin/")) seen.push(r.status());
  });

  await login(page, MEMBER);
  await sec13feShell(page);
  await page.click("#account-btn");
  // CONTROL: the forged flag really did open the door in the client.
  await expect(page.locator("#menu-hub-admin")).toBeVisible();
  await page.click("#menu-hub-admin");
  // CONTROL: we are on the panel — the crumb is the panel's, so an empty
  // `.admin` below means "rendered nothing", not "never navigated".
  await expect(page.locator("#crumb")).toHaveText("Signup & access");
  // The app is still alive (no white screen, no boundary).
  await sec13feShell(page);

  // The server refused, and every route it refused really was asked.
  await expect
    .poll(() => seen.length, { message: "no /api/admin/* request was made at all" })
    .toBeGreaterThan(0);
  expect(seen.every((s) => s === 403)).toBeTruthy();

  // Nothing admin-only reached the DOM: no roster, no queue, no toggles, and
  // no policy control the member could post.
  await expect(page.locator(".admin h1")).toHaveCount(0);
  await expect(page.locator(".admin-item.toggle")).toHaveCount(0);
  await expect(page.locator("body")).not.toContainText("Hub admins");
  await expect(page.locator("body")).not.toContainText("Allowed email domains");
  await expect(page.locator("body")).not.toContainText("Pending signups");
  await expect(page.locator('button:has-text("Save policy")')).toHaveCount(0);
});

test("HubSettings: a 403 that arrives mid-flight leaves the app usable", async ({ page }) => {
  // The panel opens as an admin (the API answers), and the policy route then
  // starts refusing — a permission revoked while the tab is open. Nothing has
  // ever driven this transition.
  await login(page, ADMIN);
  await sec13feShell(page);
  await page.click("#account-btn");
  await page.click("#menu-hub-admin");
  await expect(page.locator(".admin h1")).toHaveText("Signup & access"); // CONTROL

  await page.route("**/api/admin/**", (route) =>
    route.fulfill({ status: 403, body: "hub admins only" }),
  );
  // Re-open the panel so the queries refetch behind the 403.
  await page.goto("/");
  await sec13feShell(page);
  await page.click("#account-btn");
  await page.click("#menu-hub-admin");
  await expect(page.locator("#crumb")).toHaveText("Signup & access");

  // The floor: the shell survives, no ErrorBoundary, and no stale admin data
  // is left painted from the successful load.
  await sec13feShell(page);
  await expect(page.locator("body")).not.toContainText("This page didn’t load");
  await expect(page.locator(".admin-item", { hasText: "Hub admins" })).toHaveCount(0);
  // And navigation still works afterwards.
  const pid = await wikiId(page);
  await page.goto(`/${pid}/index.md`);
  await expect(page.locator("#content h1")).toHaveText("Wiki");
});

// ================================================================== 2. Palette
//
// ⌘K indexes and renders paths a member chose. Round 12 closed the C0/C1/bidi/
// zero-width classes at ingest; these are the shapes still legal.

test("Palette: every still-legal hostile filename renders as itself", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);

  const accepted: Array<{ name: string; path: string }> = [];
  const refused: Array<{ name: string; status: number }> = [];
  for (const p of sec13fePayloads) {
    const path = `sec13/${p.seg}`;
    const status = await sec13feSeed(page, pid, path, `# ${p.name}\n`);
    if (status >= 200 && status < 300) accepted.push({ name: p.name, path });
    else refused.push({ name: p.name, status });
  }
  // Inventory in the log, so a clean result is still evidence.
  console.log("sec13fe palette payloads accepted:", JSON.stringify(accepted.map((a) => a.name)));
  console.log("sec13fe palette payloads refused at ingest:", JSON.stringify(refused));
  expect(accepted.length, "the hub refused every payload — nothing was measured").toBeGreaterThan(0);

  await page.goto(`/${pid}/sec13`);
  // CONTROL: the folder listing rendered, so the tree really holds these.
  await expect(page.locator("#content")).toBeVisible();

  for (const a of accepted) {
    const base = a.path.split("/").pop()!;
    // The sidebar's search button is the documented opener (browse.spec.ts);
    // it needs no focus assumptions.
    await page.click("#search-btn");
    await expect(page.locator("#palette")).toBeVisible();
    // Query with a plain ASCII run every payload contains, so the match does
    // not depend on the hostile part being typeable.
    const ascii = base.replace(/[^\x20-\x7e]/g, "").replace(/\.md$/, "").slice(0, 5) || "sec13";
    await page.fill("#palette input", ascii);
    const rows = page.locator("#palette [cmdk-item]");
    // CONTROL: the palette found SOMETHING for this query.
    await expect(rows.first()).toBeVisible();

    // The label the palette paints must be the path it will open. Highlight()
    // rebuilds the string from indices computed against a lowercased copy; a
    // label that is not the filename is a name the reader cannot trust.
    const labels = await page.locator("#palette [cmdk-item] .plabel").allTextContents();
    const match = labels.find((l) => l.includes(base) || base.includes(l.trim()));
    expect(
      labels.some((l) => l === a.path || l === base),
      `palette rendered no faithful label for ${JSON.stringify(a.path)} (${a.name}).\n` +
        `labels on screen: ${JSON.stringify(labels)}\nnearest: ${JSON.stringify(match)}`,
    ).toBeTruthy();
    await page.keyboard.press("Escape");
    await expect(page.locator("#palette")).toHaveCount(0);
  }
});

test("Palette: selecting a hostile-named row opens that exact file", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  const path = "sec13/İdeas.md";
  const status = await sec13feSeed(page, pid, path, "# Ideas\n\nreal content\n");
  test.skip(status < 200 || status >= 300, `hub refused the payload (${status})`);

  await page.goto(`/${pid}/sec13`);
  await expect(page.locator("#content")).toBeVisible(); // CONTROL
  await page.click("#search-btn");
  await expect(page.locator("#palette")).toBeVisible();
  await page.fill("#palette input", "deas");
  const row = page.locator("#palette [cmdk-item]").first();
  await expect(row).toBeVisible(); // CONTROL
  const shown = (await row.locator(".plabel").textContent()) ?? "";
  await row.click();
  await expect(page.locator("#content")).toBeVisible();
  const landed = decodeURIComponent(new URL(page.url()).pathname).replace(`/${pid}/`, "");
  expect(
    shown === landed || shown === landed.split("/").pop(),
    `the palette painted ${JSON.stringify(shown)} and opened ${JSON.stringify(landed)}`,
  ).toBeTruthy();
});

// ================================================================== 3. Insights
//
// The named lead: `folders[f] = n` on a bare {} keyed by SERVER-SUPPLIED folder
// names, with `(d.folders||{})[c]` read unguarded. A folder literally named
// __proto__ is a plain assignment onto Object.prototype's setter, which drops
// it silently; a folder named constructor shadows a function with a number.
// Round 12 could not turn it into a visible break by reading. This drives it.

test("Insights: folders named __proto__ and constructor do not disturb the dashboard", async ({
  page,
}) => {
  await login(page);
  const pid = await wikiId(page);

  const seeded: string[] = [];
  for (const dir of ["__proto__", "constructor", "prototype"]) {
    const p = `${dir}/note.md`;
    const st = await sec13feSeed(page, pid, p, `# ${dir}\n\ncontent\n`);
    console.log(`sec13fe seed ${p} -> ${st}`);
    if (st >= 200 && st < 300) seeded.push(p);
  }
  expect(seeded.length, "hub refused every reserved-word folder name").toBeGreaterThan(0);

  // Report agent reads so /heat?by=device has folder keys to bucket. The device
  // id is unclaimed on this hub, so ownsDevice lets it count as an agent.
  const rep = await page.request.post(`/api/p/${pid}/reads`, {
    headers: { "X-Bdrive-Device": "sec13fe-agent" },
    data: { reads: seeded.flatMap((p) => [{ path: p }, { path: p }, { path: p }]) },
  });
  console.log("sec13fe read report:", rep.status(), await rep.text());

  // Whole-project dashboard: CoverageMatrix reads d.folders through a Map, so
  // this is the guarded path.
  await page.goto(`/${pid}/dashboard`);
  await expect(page.locator(".in-treemap")).toBeVisible(); // CONTROL
  await expect(page.locator(".in-chart .in-quad").first()).toBeVisible();
  await expect(page.locator("body")).not.toContainText("This page didn’t load");

  // The server's own answer, which is what the scoped Dashboard must not
  // contradict. Every reserved name is an ordinary own property here.
  const heat = await (await page.request.get(`/api/p/${pid}/heat?by=device&days=30`)).json();
  console.log("sec13fe /heat?by=device:", JSON.stringify(heat));
  const covered = (dir: string) =>
    (heat.devices ?? []).some(
      (d: { folders?: Record<string, number> }) =>
        d.folders && Object.prototype.hasOwnProperty.call(d.folders, dir),
    );

  // Scoped dashboard: this is the branch that rebuilds `folders` with a plain
  // assignment onto a bare {}. Drive every reserved name; the ones that behave
  // are the control for the one that does not.
  // A Map, deliberately. A plain {} keyed by these names is the very bug under
  // test — `drew["__proto__"] = false` writes nothing and `drew["__proto__"]`
  // then reads Object.prototype, which is truthy, so the assertion below would
  // silently pass. That is almost certainly why this lead survived round 12.
  const drew = new Map<string, boolean>();
  for (const p of seeded) {
    const dir = p.split("/")[0];
    await page.goto(`/${pid}/dashboard/${encodeURIComponent(dir)}`);
    // CONTROL: the scoped dashboard rendered at all.
    await expect(page.locator(".insights")).toBeVisible();
    await expect(page.locator("body")).not.toContainText("This page didn’t load");
    const title = await page.locator(".in-title").textContent();
    expect(title, `scoped dashboard for ${dir} lost its scope label`).toContain(dir);
    drew.set(
      dir,
      await page
        .locator(".in-matrix")
        .isVisible()
        .catch(() => false),
    );
  }
  console.log(
    "sec13fe scoped coverage matrix drawn:",
    JSON.stringify([...drew]),
    "server says covered:",
    JSON.stringify(seeded.map((p) => [p.split("/")[0], covered(p.split("/")[0])])),
  );

  // CONTROL: at least one reserved name DOES draw the matrix, so "not drawn"
  // below means this folder specifically, not "scoped dashboards never draw one".
  const control = [...drew].find(([d, v]) => v && covered(d));
  expect(
    control,
    `no scoped dashboard drew a coverage matrix at all (${JSON.stringify([...drew])}) — ` +
      `the fixture never exercised CoverageMatrix, so nothing was measured`,
  ).toBeTruthy();

  for (const p of seeded) {
    const dir = p.split("/")[0];
    if (!covered(dir)) continue;
    expect(
      drew.get(dir),
      `the server reports device coverage of ${JSON.stringify(dir)}, and scoping to ` +
        `${JSON.stringify(control![0])} draws the coverage matrix, but scoping to ` +
        `${JSON.stringify(dir)} draws none. Insights.tsx rebuilds the folder map with ` +
        `\`folders[f] = n\` on a bare {}: a key that lands on an Object.prototype setter ` +
        `is swallowed, the device's folder set comes out empty, and the .filter() below ` +
        `drops the whole device — so a member who creates one folder can make an agent's ` +
        `read coverage disappear from the Dashboard.`,
    ).toBeTruthy();
  }
});

// ================================================================== 4. ErrorBoundary
//
// New code on the render path of every page (round 12). Nothing has rendered
// it. Two questions: what does it put in the DOM, and can the app get stuck?

// I could not reach the boundary from outside the app. Every attempt below is
// recorded rather than dropped, because "the floor was never stood on" is the
// thing the next round needs to know:
//
//   - a /api/config the app cannot read ({"mode":"hub"}, no auth block) throws
//     inside useConfig's queryFn, which React Query catches. The app then sits
//     on "Loading…" forever with no error shown and no boundary. Not a render
//     throw, and /api/config is server-owned, so it is not attacker-reachable.
//   - the round-12 decodePath URIError (%80) is fixed at the source.
//   - none of the still-legal hostile filenames below throws anywhere.
//
// What CAN be asserted without inventing a throw is the boundary's contract in
// the negative: no surface this round drives may reach it. Every hostile-input
// test in this file therefore asserts the boundary's own copy is absent, which
// is a real regression net for the next component that renders peer text.
test("ErrorBoundary: no surface driven this round reaches the app's floor", async ({ page }) => {
  const errors: string[] = [];
  page.on("pageerror", (e) => errors.push(String(e)));
  await login(page, ADMIN);
  const pid = await wikiId(page);
  for (const p of sec13fePayloads) {
    await sec13feSeed(page, pid, `sec13/${p.seg}`, `# ${p.name}\n`);
  }
  for (const url of [
    `/${pid}`,
    `/${pid}/sec13`,
    `/${pid}/dashboard`,
    `/${pid}/history`,
    `/${pid}/settings`,
  ]) {
    await page.goto(url);
    // CONTROL: the page rendered — an empty pane would pass the next line.
    await sec13feShell(page);
    await expect(page.locator("body")).not.toContainText("This page didn’t load");
  }
  expect(errors, `uncaught errors across the driven surfaces:\n${errors.join("\n")}`).toEqual([]);
});

// ================================================================== 5. VolumeApp
//
// Single-volume mode has never been loaded in a browser. It is the auth-free
// plain-folder viewer — a different trust model, the same components.

test.describe("VolumeApp (single-volume mode)", () => {
  let proc: ChildProcess | undefined;
  // Outside 8993-8996: playwright.config.ts reserves that block for the
  // session-long harnesses, which are already listening when this spawns. A
  // port inside it binds nowhere and the readiness poll below then answers
  // 200 from the harness that owns it — every assertion measures the wrong
  // server (#vault-name reads "BearDrive", /api/tree says "this server hosts
  // projects") instead of failing.
  const port = 8997;
  const base = `http://127.0.0.1:${port}`;
  let dir = "";
  const accepted: string[] = [];

  test.beforeAll(async () => {
    dir = mkdtempSync(join(tmpdir(), "sec13fe-vol-"));
    const vol = join(dir, "vol");
    mkdirSync(join(vol, "sec13"), { recursive: true });
    writeFileSync(join(vol, "index.md"), "# Volume Home\n\nGo to [notes](sec13/plain.md).\n");
    writeFileSync(join(vol, "sec13", "plain.md"), "# Plain\n");
    for (const p of sec13fePayloads) {
      try {
        writeFileSync(join(vol, "sec13", p.seg), `# ${p.name}\n`);
        accepted.push(p.seg);
      } catch {
        /* a name the filesystem itself refuses is not a finding */
      }
    }
    proc = spawn(
      "go",
      ["run", "./cmd/bdrive", "serve", "--dir", vol, "--addr", `127.0.0.1:${port}`],
      { cwd: join(process.cwd(), "..", "..", ".."), env: { ...process.env, BDRIVE_HOME: join(dir, "home") }, stdio: "inherit" },
    );
    const until = Date.now() + 120_000;
    for (;;) {
      try {
        const r = await fetch(base + "/api/config");
        if (r.ok) break;
      } catch {
        /* not up yet */
      }
      if (Date.now() > until) throw new Error("volume-mode server never came up");
      await new Promise((r) => setTimeout(r, 500));
    }
  });

  test.afterAll(() => {
    proc?.kill("SIGKILL");
  });

  test("renders the auth-free viewer with no hub assumptions", async ({ page }) => {
    const errors: string[] = [];
    page.on("pageerror", (e) => errors.push(String(e)));
    await page.goto(base + "/");
    // CONTROL: the SPA mounted in volume mode.
    await expect(page.locator("#sidebar")).toBeVisible();
    await expect(page.locator("#vault-name")).toHaveText("vol");
    await expect(page.locator("body")).not.toContainText("This page didn’t load");

    // No auth surface — the mode is auth-free by design, so an account menu
    // here would be a control that cannot work.
    await expect(page.locator("#account-btn")).toHaveCount(0);

    // A file opens, and its URL carries no project id.
    await page.goto(base + "/index.md");
    await expect(page.locator("#content h1")).toHaveText("Volume Home");
    expect(new URL(page.url()).pathname).toBe("/index.md");

    // The hub-only APIs must not be called at all in this mode: a component
    // that assumes a project id would 404 into a confusing state.
    const hubCalls: string[] = [];
    page.on("request", (r) => {
      const u = new URL(r.url()).pathname;
      if (u.startsWith("/api/p/") || u === "/api/projects" || u.startsWith("/api/admin/"))
        hubCalls.push(u);
    });
    await page.goto(base + "/sec13");
    await expect(page.locator("#content")).toBeVisible();
    await page.waitForTimeout(1500);
    expect(hubCalls, `volume mode called hub-only routes: ${hubCalls.join(", ")}`).toEqual([]);
    expect(errors, `uncaught errors in volume mode: ${errors.join("\n")}`).toEqual([]);
  });

  test("hostile filenames render in the tree, the listing and the palette", async ({ page }) => {
    const errors: string[] = [];
    page.on("pageerror", (e) => errors.push(String(e)));
    await page.goto(base + "/sec13");
    await expect(page.locator("#sidebar")).toBeVisible(); // CONTROL
    await expect(page.locator("#content")).toBeVisible();
    console.log("sec13fe volume payloads on disk:", JSON.stringify(accepted));

    // The listing names every file the folder holds — a name that vanishes
    // from the listing is a file the reader cannot reach or audit.
    // The server's own answer is the reference: a name the API lists and the
    // page does not is a file the reader cannot reach or audit.
    const tree = await (await page.request.get(base + "/api/tree")).json();
    const inApi = (tree.children ?? [])
      .find((c: { path: string }) => c.path === "sec13")
      ?.children?.map((c: { name: string }) => c.name) as string[] | undefined;
    console.log("sec13fe volume /api/tree sec13:", JSON.stringify(inApi));
    console.log(
      "sec13fe volume payloads written to disk:",
      JSON.stringify(accepted.map((s) => [...s].map((c) => c.codePointAt(0)!.toString(16)).join(" "))),
    );
    expect(inApi, "the volume server listed no sec13 folder").toBeTruthy();

    const listed = await page.locator("#content").innerText();
    for (const name of inApi!) {
      expect(
        listed.includes(name),
        `the API lists ${JSON.stringify(name)} but the folder listing does not show it:\n${listed}`,
      ).toBeTruthy();
    }

    // And the palette indexes them without the app throwing.
    await page.keyboard.press("ControlOrMeta+k");
    await expect(page.locator("#palette")).toBeVisible();
    await page.fill("#palette input", "sec13");
    await expect(page.locator("#palette [cmdk-item]").first()).toBeVisible();
    await page.keyboard.press("Escape");
    expect(errors, `uncaught errors: ${errors.join("\n")}`).toEqual([]);
  });
});
