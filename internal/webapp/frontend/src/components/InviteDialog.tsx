import { useRef, useState } from "react";
import { postJSON } from "../api/http";
import { Button } from "@/components/ui/button";
import { copyText } from "../util";
import { toast } from "../toast";
import { expiryLabel } from "./SharesTable";
import { Dialog, DialogContent, DialogTitle } from "@/components/ui/dialog";

/* Expiry is offered BEFORE minting, the opposite of ShareDialog. That comment
   (ShareDialog.tsx:9-14) is about ShareDB.Create reusing an existing permanent
   link, so minting with a TTL would hand out a second one; CreateInvite always
   mints a fresh token, so there is no second link to dodge. No "Never" preset:
   DefaultInviteTTL is 7 days (orgs.go:70) and an invite has no permanent form. */
const EXPIRY_PRESETS = [
  { value: "24h", label: "In 24 hours" },
  { value: "168h", label: "In 7 days" },
  { value: "720h", label: "In 30 days" },
];

/* Minting an org invite is at least as consequential as minting a public share
   link — on the default invite-only posture the link also lets the recipient
   CREATE an account (orgs.go:594-602, BuiltinAuth.InviteValid) — so it gets the
   ShareDialog treatment, moved one step earlier: nothing is created until the
   user confirms, and what was created is then on the page rather than only on
   the clipboard. Copy failure only changes a button label; the link itself
   never depends on the clipboard, because this dialog is the ONLY place the
   project-scoped form of it exists (see ProjectSettings). */
export function InviteDialog({
  orgId,
  orgName,
  projectId,
  projectName,
  onClose,
}: {
  orgId: string;
  orgName: string;
  projectId: string;
  projectName: string;
  onClose: () => void;
}) {
  const [preset, setPreset] = useState("168h");
  const [url, setUrl] = useState<string | null>(null);
  const [expires, setExpires] = useState<string | undefined>();
  const [copied, setCopied] = useState(false);
  const [busy, setBusy] = useState(false);
  // Post-mint, Copy link keeps the focus for the same reason ShareDialog gives:
  // the dialog exists to hand over a URL.
  const copyRef = useRef<HTMLButtonElement>(null);
  /* A latch, not just `disabled={busy}`: two clicks dispatched inside ONE task
     both run before React repaints the button, and a second mint leaves a
     stray live invite on the org. State is a repaint behind; a ref is not. */
  const minting = useRef(false);

  async function mint() {
    if (minting.current) return;
    minting.current = true;
    setBusy(true);
    try {
      const out = await postJSON<{ url: string; expires?: string }>(
        `/api/orgs/${orgId}/invites`,
        { expires_in: preset },
      );
      const link = out.url + "?p=" + projectId;
      setUrl(link);
      setExpires(out.expires);
      // Best-effort: the result picks a button label, never whether the link
      // survives. Priya's browser had no clipboard and hit a dead end.
      setCopied(await copyText(link));
    } catch (e) {
      toast((e as Error).message, true);
    } finally {
      minting.current = false;
      setBusy(false);
    }
  }

  return (
    <Dialog open onOpenChange={(open) => !open && onClose()}>
      <DialogContent
        className="modal"
        showCloseButton={false}
        onOpenAutoFocus={(e) => {
          if (!url) return;
          e.preventDefault();
          copyRef.current?.focus();
        }}
      >
        <DialogTitle asChild>
          <h3>{url ? "Invite link" : "Invite a teammate"}</h3>
        </DialogTitle>
        {url === null ? (
          <>
            <p>
              <b>Anyone with this link joins {orgName} as a member</b> — and on a hub with signup
              closed, it also lets them create an account. They land on{" "}
              <b>{projectName}</b>'s setup page.
            </p>
            <div className="modal-expiry">
              <label htmlFor="invite-expiry">Expires</label>
              <select
                id="invite-expiry"
                value={preset}
                disabled={busy}
                onChange={(e) => setPreset(e.target.value)}
              >
                {EXPIRY_PRESETS.map((o) => (
                  <option key={o.value} value={o.value}>
                    {o.label}
                  </option>
                ))}
              </select>
            </div>
            <div className="modal-actions">
              <Button variant="subtle" onClick={onClose}>
                Cancel
              </Button>
              <Button id="invite-create" variant="primary" disabled={busy} onClick={mint}>
                {busy ? "Creating…" : "Create invite link"}
              </Button>
            </div>
          </>
        ) : (
          <>
            <p>
              Send this to your teammate. It stays valid until it expires or you revoke it in
              Organization settings.
            </p>
            <div className="modal-url">{url}</div>
            <div className="modal-expiry">
              <span className="modal-expiry-note">{expiryLabel(expires)}</span>
            </div>
            <div className="modal-actions">
              <Button
                ref={copyRef}
                variant="primary"
                onClick={() =>
                  copyText(url).then((ok) => {
                    setCopied(ok);
                    toast(ok ? "Copied." : "Select and copy the link above.");
                  })
                }
              >
                {copied ? "Copied ✓" : "Copy link"}
              </Button>
              <Button variant="subtle" onClick={() => window.open(url, "_blank")}>
                Open
              </Button>
              <Button variant="subtle" onClick={onClose}>
                Done
              </Button>
            </div>
          </>
        )}
      </DialogContent>
    </Dialog>
  );
}
