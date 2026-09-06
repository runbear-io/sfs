import { useState } from "react";
import type { Org } from "../api/types";
import { linkProps } from "../nav";
import { Icon } from "./shell";
import { projColor } from "./ProjectNav";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";

const REPO_URL = "https://github.com/runbear-io/beardrive";

// The GitHub mark, inline: lucide dropped its brand icons in v1, and one
// glyph is not worth a second icon dependency. Filled (not stroked) like
// every other rendering of this logo, so it sits outside the `.ico` sizing
// rules that the lucide `<Icon>` components share.
function GithubMark() {
  return (
    <svg viewBox="0 0 16 16" className="gh-mark" fill="currentColor" aria-hidden="true">
      <path d="M8 0C3.58 0 0 3.58 0 8c0 3.54 2.29 6.53 5.47 7.59.4.07.55-.17.55-.38 0-.19-.01-.82-.01-1.49-2.01.37-2.53-.49-2.69-.94-.09-.23-.48-.94-.82-1.13-.28-.15-.68-.52-.01-.53.63-.01 1.08.58 1.23.82.72 1.21 1.87.87 2.33.66.07-.52.28-.87.51-1.07-1.78-.2-3.64-.89-3.64-3.95 0-.87.31-1.59.82-2.15-.08-.2-.36-1.02.08-2.12 0 0 .67-.21 2.2.82.64-.18 1.32-.27 2-.27s1.36.09 2 .27c1.53-1.04 2.2-.82 2.2-.82.44 1.1.16 1.92.08 2.12.51.56.82 1.27.82 2.15 0 3.07-1.87 3.75-3.65 3.95.29.25.54.73.54 1.48 0 1.07-.01 1.93-.01 2.2 0 .21.15.46.55.38A8.013 8.013 0 0 0 16 8c0-4.42-3.58-8-8-8z" />
    </svg>
  );
}

// The sidebar footer is the account row: avatar, name, email. Clicking it
// opens a menu with the workspace (org) and account actions — settings,
// hub administration for admins, and sign-out. Radix owns open/dismiss
// behavior (Escape, outside click, focus).
//
// The org entry is a plain link to org.manage_url: this hub's own org page
// when it owns its orgs, the identity provider's page when it does not. The
// server decides; nothing here branches on the answer.
/* The star ask is a link that sits there, never a banner that interrupts: it
   is dim, one click, and identical on every render, so it reads as social
   proof rather than as a request. Anything dismissible would need dismissal
   state and would still have interrupted once.

   One component because both footers below render it identically — signed in
   and signed out were byte-identical copies. */
function StarOnGitHub() {
  return (
    <a className="gh-star" href={REPO_URL} target="_blank" rel="noreferrer">
      <GithubMark />
      <span>Star on GitHub</span>
      <span className="ext" aria-hidden="true">↗</span>
      <span className="sr-only"> (opens in a new tab)</span>
    </a>
  );
}

export function AccountBar({
  me,
  org,
  admin,
  orgActive,
  billing,
  signOut,
}: {
  me: { email: string; name: string };
  org: Org | null;
  admin?: { pending: number; onClick: () => void }; // hub admins only
  orgActive?: boolean; // the org page is the open surface
  billing?: { plan: string; url: string }; // managed deployments only
  // Desktop app: sign-out is a sidecar call, not the hub's /auth/logout page.
  signOut?: () => void;
}) {
  const display = me.name || me.email;
  // The menu's open state is ours because the org entry is a link: linkProps
  // calls preventDefault to route internally, and Radix composes its own
  // select handler with checkForDefaultPrevented, so that handler never runs
  // and the menu stays open on top of the page it just opened. Closing it
  // here works for both destinations without giving up a real <a>
  // (middle-click, copy link address).
  const [menuOpen, setMenuOpen] = useState(false);
  const orgLink = org ? linkProps(org.manage_url) : null;
  const billingLink = billing ? linkProps(billing.url) : null;
  return (
    <footer id="accountbar">
      <StarOnGitHub />
      <DropdownMenu modal={false} open={menuOpen} onOpenChange={setMenuOpen}>
        <DropdownMenuTrigger asChild>
          <button id="account-btn" className={orgActive ? "active" : undefined} aria-label="Account menu">
            <span className="avatar" style={{ background: projColor(me.email) }} aria-hidden="true">
              {(display.trim()[0] || "?").toUpperCase()}
            </span>
            <span className="acct">
              <b>{display}</b>
              {me.name && <small>{me.email}</small>}
            </span>
            <Icon name="chev" />
          </button>
        </DropdownMenuTrigger>
        <DropdownMenuContent id="account-menu" side="top" align="start" sideOffset={6} className="acct-menu">
          {org && (
            <>
              <DropdownMenuLabel className="menu-sec">Organization</DropdownMenuLabel>
              <DropdownMenuItem asChild>
                <a
                  id="menu-org-settings"
                  aria-current={orgActive ? "page" : undefined}
                  {...orgLink}
                  onClick={(e) => {
                    orgLink?.onClick?.(e);
                    setMenuOpen(false);
                  }}
                >
                  <Icon name="gear" />
                  <span>
                    <b>{org.name}</b> Settings
                  </span>
                  {!org.manage_url.startsWith("/") && (
                    <>
                      <span className="ext" aria-hidden="true">↗</span>
                      <span className="sr-only"> (opens in a new tab)</span>
                    </>
                  )}
                </a>
              </DropdownMenuItem>
              {billing && (
                <DropdownMenuItem asChild>
                  {/* An in-app view route (/billing); the chip shows the
                      org's current plan. */}
                  <a
                    id="menu-billing"
                    {...billingLink}
                    onClick={(e) => {
                      billingLink?.onClick?.(e);
                      setMenuOpen(false);
                    }}
                  >
                    <Icon name="card" />
                    <span>Billing</span>
                    <span className="ps-chip plan-chip">{billing.plan}</span>
                  </a>
                </DropdownMenuItem>
              )}
            </>
          )}
          {admin && (
            <>
              <DropdownMenuLabel className="menu-sec">Hub</DropdownMenuLabel>
              <DropdownMenuItem id="menu-hub-admin" onSelect={admin.onClick}>
                <Icon name="shield" />
                <span>Signup &amp; access{admin.pending ? ` · ${admin.pending}` : ""}</span>
              </DropdownMenuItem>
            </>
          )}
          <DropdownMenuLabel className="menu-sec">Account</DropdownMenuLabel>
          {signOut ? (
            <DropdownMenuItem id="signout" onSelect={signOut}>
              <Icon name="power" />
              <span>Sign out</span>
            </DropdownMenuItem>
          ) : (
            <DropdownMenuItem asChild>
              <a id="signout" href="/auth/logout">
                <Icon name="power" />
                <span>Log out</span>
              </a>
            </DropdownMenuItem>
          )}
        </DropdownMenuContent>
      </DropdownMenu>
    </footer>
  );
}

// SignedOutBar is the desktop app's sidebar footer while no device sign-in
// exists: same GitHub row, and the account slot becomes the sign-in action
// (which opens the hub's login page in the system browser via the sidecar).
export function SignedOutBar({ onSignIn }: { onSignIn: () => void }) {
  return (
    <footer id="accountbar">
      <StarOnGitHub />
      <button id="account-btn" onClick={onSignIn} aria-label="Sign in">
        <span className="avatar" style={{ background: "var(--hover)" }} aria-hidden="true">?</span>
        <span className="acct">
          <b>Sign in…</b>
          <small>connect to your hub</small>
        </span>
      </button>
    </footer>
  );
}
