import { test, expect } from "@playwright/test";
import { login, wikiId } from "./helpers";

// Live change notification (SSE). The Go tests cover both ends of the wire —
// the hub publishes, the sync client wakes up — so what is left, and what only
// a browser can check, is that a frame actually reaches TanStack Query and
// invalidates the right thing.
//
// Every assertion here is deliberately time-bounded well under the 15s tree
// poll. A test that waited 20s would pass on the poll alone and prove nothing.

type SPAWindow = Window & { __spa?: number };

test("an open file updates when a peer writes it, with no reload", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/index.md`);
  await expect(page.locator("#content h1")).toHaveText("Wiki");

  // Mark the document. If this survives, the page never reloaded — the update
  // came through the stream rather than a navigation.
  await page.evaluate(() => ((window as SPAWindow).__spa = 1));

  // A peer writes the file the reader is looking at.
  await page.request.put(`/api/p/${pid}/upload/content?path=index.md`, {
    data: "# Wiki rewritten\n\nA teammate changed this while you were reading.\n",
  });

  // This is the assertion that would have failed before events existed: file
  // CONTENT has no refetch interval, so an open body previously stayed on
  // screen until the reader navigated away — forever, not for 15 seconds.
  await expect(page.locator("#content h1")).toHaveText("Wiki rewritten", { timeout: 10_000 });
  expect(await page.evaluate(() => (window as SPAWindow).__spa)).toBe(1);
});

test("a peer's new file appears in the tree without waiting for the poll", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/notes`);
  await page.evaluate(() => ((window as SPAWindow).__spa = 1));

  await page.request.put(
    `/api/p/${pid}/upload/content?path=${encodeURIComponent("notes/live-arrival.md")}`,
    { data: "# Live\n" },
  );

  await expect(page.locator('#tree .row[data-path="notes/live-arrival.md"]')).toBeVisible({
    timeout: 10_000,
  });
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
