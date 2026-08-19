import { test, expect } from "@playwright/test";
import { login, wikiId, ADMIN, MEMBER, expectToast } from "./helpers";

// Phase 4: org admin (rename, members, projects, invites, share audit) and
// hub settings (policy toggles, pending queue). Panels are not routes —
// navigation closes them. Mutating specs revert their changes: the suite
// shares one hub per run.

// The org panel opens from the account menu (sidebar footer).
async function openOrgSettings(page: import("@playwright/test").Page) {
  await page.click("#account-btn");
  await page.click("#menu-org-settings");
}

test("org admin: members with roles, self marked, rename round-trip", async ({ page }) => {
  await login(page);
  await openOrgSettings(page);
  await expect(page.locator("#org-title")).toHaveText("default");
  // The crumb names the surface; repeating the org name under the <h1> that
  // already says it told the reader nothing.
  await expect(page.locator("#crumb")).toHaveText("Organization");
  await expect(page.locator(".admin-item", { hasText: ADMIN })).toContainText("(you)");
  const memberRow = page.locator(".admin-item", { hasText: MEMBER });
  await expect(memberRow.locator("select")).toHaveValue("member");

  // Rename and revert
  await page.fill("#org-rename", "renamed-org");
  await page.click("#org-rename-btn");
  await expectToast(page, "Renamed");
  await page.click("#account-btn");
  await expect(page.locator("#menu-org-settings")).toContainText("renamed-org");
  await page.keyboard.press("Escape");
  await page.fill("#org-rename", "default");
  await page.click("#org-rename-btn");
  await page.click("#account-btn");
  await expect(page.locator("#menu-org-settings")).toContainText("default");
  await page.keyboard.press("Escape");
});

// The org page owns a URL like every other page: the account menu is a plain
// link to it, so a deep link and a reload both land on the same view.
test("org admin: is a real route, not panel state", async ({ page }) => {
  await login(page);
  await openOrgSettings(page);
  await expect(page).toHaveURL(/\/orgs\/[^/]+$/);
  const url = page.url();

  await page.reload();
  await expect(page.locator("#org-title")).toHaveText("default");

  await page.goto("/");
  await page.goto(url);
  await expect(page.locator("#org-title")).toHaveText("default");
});

test("org admin: member role change round-trip", async ({ page }) => {
  await login(page);
  await openOrgSettings(page);
  const sel = page.locator(".admin-item", { hasText: MEMBER }).locator("select");
  await sel.selectOption("owner");
  await expectToast(page, "Role updated");
  await expect(sel).toHaveValue("owner");
  await sel.selectOption("member");
  await expect(sel).toHaveValue("member");
});

test("org admin: invite create shows in list, revoke removes it", async ({ page }) => {
  await login(page);
  await openOrgSettings(page);
  await page.click(".admin-h .pbtn"); // New invite
  await expectToast(page, "Invite");
  const row = page.locator(".admin-item", { hasText: "/join/" }).first();
  await expect(row).toBeVisible();
  await expect(row.locator(".ai-tag")).toContainText("unused");
  await row.locator(".ai-del").click();
  await page.click(".modal .danger-btn"); // confirm revoke
  await expectToast(page, "Revoked");
  await expect(page.locator(".admin-item", { hasText: "/join/" })).toHaveCount(0);
});

test("org admin: public share audit lists and revokes", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.request.post(`/api/p/${pid}/shares`, { data: { path: "index.md" } });
  await openOrgSettings(page);
  const row = page.locator(".admin-item", { hasText: "index.md" });
  await expect(row).toBeVisible();
  await expect(row.locator(".ai-tag")).toContainText("wiki");
  // BEA-130: the row hands the link back. Assert the toast, not the
  // clipboard — the suite grants no clipboard-read permission, and the
  // confirmation is what the user actually sees.
  await row.locator("button[aria-label='Copy the public link to index.md']").click();
  await expectToast(page, "Copied.");
  // …and Revoke is not displaced by it.
  await row.locator(".ai-del").click();
  await page.click(".modal .danger-btn");
  await expectToast(page, "Share revoked");
  await expect(page.locator(".admin-item", { hasText: "index.md" })).toHaveCount(0);
});

test("org admin: the project list is read-only; rename lives on project settings", async ({
  page,
}) => {
  await login(page);
  const made = await (await page.request.post("/api/projects", { data: { name: "doomed" } })).json();
  await page.reload(); // pick up the new project
  await openOrgSettings(page);
  const row = page.locator(".admin-item", { hasText: "doomed" });
  await expect(row).toBeVisible();
  // Neither affordance lives here any more: renaming and deleting a project
  // both happen on the project's own Settings page.
  await expect(row.locator(".ai-btn", { hasText: "Rename" })).toHaveCount(0);
  await expect(row.locator(".ai-del")).toHaveCount(0);

  // …and renaming there works, showing up in the nav.
  await page.goto(`/${made.project.id}/settings`);
  await page.fill("#ps-name", "doomed-2");
  await page.click("#ps-save");
  await expectToast(page, "Saved");
  await expect(page.locator("#projects .proj-trigger")).toContainText("doomed-2");

  await page.request.delete("/api/projects/" + made.project.id); // clean up
});

test("member sees the org panel read-only", async ({ page }) => {
  await login(page, MEMBER);
  await openOrgSettings(page);
  // The role reads as a chip beside the name, not as part of it, and the
  // short page explains itself rather than looking truncated.
  // The chip is a sibling of the <h1>, not a child: inside it, the
  // accessible name of the heading came out as "defaultMember".
  await expect(page.locator(".role-chip")).toHaveText("Member");
  await expect(page.locator(".admin-sub")).toContainText("Only owners");
  await expect(page.locator("#org-rename")).toHaveCount(0);
  await expect(page.locator(".admin-item select")).toHaveCount(0);
  await expect(page.locator(".admin-item .ai-tag").first()).toBeVisible(); // role tags
});

test("hub settings: policy view, save round-trip, pending queue empty", async ({ page }) => {
  await login(page);
  await page.click("#account-btn");
  await page.click("#menu-hub-admin");
  await expect(page.locator("#crumb")).toHaveText("Signup & access");
  await expect(page.locator(".admin h1")).toHaveText("Signup & access");
  // Server has no SMTP: verification toggle disabled
  const ver = page.locator(".admin-item.toggle").first().locator("input");
  await expect(ver).toBeDisabled();
  await expect(page.locator(".admin-item", { hasText: "Self-signup" })).toContainText("invite-only");
  await expect(page.locator(".admin-item", { hasText: "Hub admins" })).toContainText(ADMIN);
  // Toggle approval on, save, revert
  const app = page.locator(".admin-item.toggle").nth(1).locator("input");
  await app.check();
  await page.click('.admin button:has-text("Save policy")');
  await expectToast(page, "policy saved");
  await app.uncheck();
  await page.click('.admin button:has-text("Save policy")');
  await expectToast(page, "policy saved");
  await expect(page.locator(".admin-empty", { hasText: "No one is waiting" })).toBeVisible();
});

test("navigating away closes an open admin panel", async ({ page }) => {
  await login(page);
  await page.click("#account-btn");
  await page.click("#menu-hub-admin");
  await expect(page.locator(".admin h1")).toBeVisible();
  await page.click('#tree .row[data-path="index.md"]');
  await expect(page.locator("#content h1")).toHaveText("Wiki");
  await expect(page.locator(".admin")).toHaveCount(0);
});

test("members table sorts by email", async ({ page }) => {
  await login(page);
  await openOrgSettings(page);
  const emails = page.locator(".admin-table .admin-item .ai-main");
  await expect(emails.first()).toBeVisible();
  const before = await emails.allTextContents();
  // Sorting is a button inside the header cell so it is reachable by
  // keyboard; the header also announces the direction via aria-sort.
  await page.click('.admin-table th:has-text("Member") .th-sort');
  await expect(page.locator('.admin-table th:has-text("Member")')).toHaveAttribute("aria-sort", /ascending|descending/);
  const after = await emails.allTextContents();
  expect([...before].reverse()).toEqual(after);
});
