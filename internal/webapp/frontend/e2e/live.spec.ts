import { test, expect } from "@playwright/test";
import { login, wikiId, ADMIN, MEMBER } from "./helpers";

// Live change notification (SSE). The Go tests cover both ends of the wire —
// the hub publishes, the sync client wakes up — so what is left, and what only
// a browser can check, is that a frame reaches TanStack Query and invalidates
// the right thing.
//
// Every assertion is time-bounded well under the 15s tree poll: a test that
// waited 20s would pass on the poll alone and prove nothing.
//
// These specs OWN the paths they touch. The harness is one hub with mutable
// state shared by every spec (workers: 1), so writing to a seeded file would
// both break the next repeat of this file and quietly change what browse.spec
// asserts about index.md.

type SPAWindow = Window & { __spa?: number };

// A path nothing else in the suite reads, and a fresh one per test run where
// the test is about something APPEARING.
const OWNED = "live-spec-open.md";
const fresh = () => `live-spec-${Date.now()}-${Math.random().toString(36).slice(2, 8)}.md`;

test("an open file updates when a peer writes it, with no reload", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);

  // Establish the "before" ourselves rather than relying on the seed, so the
  // test is identical on its first run and its fiftieth.
  await page.request.put(`/api/p/${pid}/upload/content?path=${OWNED}`, {
    data: "# Before\n\nThe reader opens this.\n",
  });
  await page.goto(`/${pid}/${OWNED}`);
  await expect(page.locator("#content h1")).toHaveText("Before");

  // Mark the document. If the mark survives, the page never reloaded — the
  // update arrived on the stream rather than through a navigation.
  await page.evaluate(() => ((window as SPAWindow).__spa = 1));

  await page.request.put(`/api/p/${pid}/upload/content?path=${OWNED}`, {
    data: "# After\n\nA teammate changed this while you were reading.\n",
  });

  // The assertion that would have failed before events existed: file CONTENT
  // has no refetch interval, so an open body previously stayed on screen until
  // the reader navigated away — indefinitely, not for 15 seconds.
  await expect(page.locator("#content h1")).toHaveText("After", { timeout: 10_000 });
  expect(await page.evaluate(() => (window as SPAWindow).__spa)).toBe(1);
});

test("a peer's new file appears in the tree without waiting for the poll", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  const path = `notes/${fresh()}`; // must not exist yet, or "appears" proves nothing

  await page.goto(`/${pid}/notes`);
  await expect(page.locator(`#tree .row[data-path="${path}"]`)).toHaveCount(0);
  await page.evaluate(() => ((window as SPAWindow).__spa = 1));

  await page.request.put(`/api/p/${pid}/upload/content?path=${encodeURIComponent(path)}`, {
    data: "# Live\n",
  });

  await expect(page.locator(`#tree .row[data-path="${path}"]`)).toBeVisible({ timeout: 10_000 });
  expect(await page.evaluate(() => (window as SPAWindow).__spa)).toBe(1);
});

// The stream names paths, so it is a read of the project and must be walled
// like one. A hub that streamed to non-members would leak filenames from every
// project on it — the one thing /heat is careful never to do.
test("the event stream is refused to someone outside the project", async ({ page, playwright }) => {
  await login(page);
  const pid = await wikiId(page);
  // A context of its own: no cookies, so this is a stranger, not the reader
  // signed in above.
  const anon = await playwright.request.newContext({ baseURL: "http://localhost:8993" });
  const resp = await anon.get(`/api/p/${pid}/events`);
  expect(resp.status()).toBeGreaterThanOrEqual(400);
  await anon.dispose();
});

// Presence: two real accounts, two browser contexts, one project. The roster
// travels on the same stream as changes, so this also proves the two frame
// types coexist on one connection.
test("a teammate viewing the project shows up in the presence bar", async ({ browser }) => {
  const one = await browser.newContext();
  const two = await browser.newContext();
  const a = await one.newPage();
  const b = await two.newPage();
  try {
    await login(a, ADMIN);
    await login(b, MEMBER);
    const pid = await wikiId(a);
    await a.goto(`/${pid}/${OWNED}`);
    await b.goto(`/${pid}/${OWNED}`);

    // Each sees somebody. Assert on the bar rendering at all, not on a name
    // the seed is free to change.
    await expect(a.locator("#presence span").first()).toBeVisible({ timeout: 15_000 });
    await expect(b.locator("#presence span").first()).toBeVisible({ timeout: 15_000 });
  } finally {
    await one.close();
    await two.close();
  }
});
