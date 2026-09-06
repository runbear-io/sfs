import { useMemo, useRef, useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { api } from "../api/http";
import { Button } from "@/components/ui/button";
import { copyText } from "../util";
import { toast } from "../toast";
import { track } from "../analytics";
import { expiryLabel } from "./SharesTable";
import { Icon } from "./shell";
import { projColor } from "./ProjectNav";
import { secretsMessage, type SecretFinding } from "../lib/secrets";
import { ruleFor } from "../lib/folders";
import { useFolders, useOrgs, usePermissions } from "../hooks/useHub";
import { atLeast, type PermLevel, type Project, type ShareInfo } from "../api/types";
import { Dialog, DialogContent, DialogTitle } from "@/components/ui/dialog";

/* One Share dialog for a file and for a folder, on the shape Google Drive
   established: who inside the workspace can open this, then whether anyone
   outside can.

   BOTH sections are always rendered, and only which one is LIVE depends on
   what you opened it on. That is what makes it one dialog rather than two
   wearing the same title: a folder's answer to "is there a public link?" is
   "links are per file", which is an answer, not a reason to hide the section.

   Two things are deliberately NOT copied from Drive:

   - No Commenter. There are no comments.
   - No Transfer ownership. Nothing here is owned per item; org owners are
     implicitly admin on every project (perms.go), so there is nothing to hand
     over.

   And one thing Drive has that this cannot: a public link on a FOLDER. /s/
   tokens address one file's content. Offering a folder link would be a
   promise the hub has no way to keep. */

const EXPIRY_PRESETS = [
  { value: "", label: "Never" },
  { value: "24h", label: "In 24 hours" },
  { value: "168h", label: "In 7 days" },
  { value: "720h", label: "In 30 days" },
];

/* The levels a folder rule may carry, and the one that is easy to get wrong:
   "" is the inherit state — the rule names some people and says nothing about
   everyone else. A select with no option for it displays the first option
   instead and saves that on the next edit. */
const FOLDER_LEVELS: Array<{ value: PermLevel | ""; label: string }> = [
  { value: "", label: "Same as the project" },
  { value: "write", label: "Write" },
  { value: "read", label: "Read" },
  { value: "none", label: "No access" },
];
const GRANT_LEVELS = FOLDER_LEVELS.filter((l) => l.value !== "");

export function ShareDialog({
  project,
  path,
  isDir,
  shares,
  onChanged,
  onOpenFolder,
  onClose,
}: {
  project: Project;
  path: string;
  isDir: boolean;
  shares: ShareInfo[];
  onChanged: () => void;
  onOpenFolder: (path: string) => void;
  onClose: () => void;
}) {
  const qc = useQueryClient();
  const { data: folders } = useFolders(project.id);
  const { data: perms } = usePermissions(project.id);
  const { data: orgs } = useOrgs(!!project.org);
  const orgName = orgs?.find((o) => o.id === project.org)?.name || "this workspace";
  const isAdmin = atLeast(project.perm, "admin");
  const canWrite = atLeast(project.perm, "write");

  // A folder's rule is keyed by its slash-terminated prefix; a file is matched
  // against the rules as-is.
  const key = isDir ? path + "/" : path;
  const rules = useMemo(() => folders?.folders || [], [folders]);
  const own = isDir ? rules.find((r) => r.prefix === key) : undefined;
  const governing = ruleFor(rules, key);
  const link = shares.find((s) => s.path === path);

  const [busy, setBusy] = useState(false);
  const [addEmail, setAddEmail] = useState("");
  const [addLevel, setAddLevel] = useState<PermLevel>("read");
  const [pending, setPending] = useState<SecretFinding[] | null>(null);
  const [preset, setPreset] = useState("");
  const copyRef = useRef<HTMLButtonElement>(null);

  const reload = () => {
    qc.invalidateQueries({ queryKey: ["folders", project.id] });
    onChanged();
  };
  const run = async (fn: () => Promise<unknown>, ok: string) => {
    setBusy(true);
    try {
      await fn();
      toast(ok);
    } catch (e) {
      toast((e as Error).message, true);
    } finally {
      setBusy(false);
      reload();
    }
  };

  /* Every folder write sends the WHOLE rule: PUT /folders replaces
     {prefix, default, perms}, so a partial post would drop the exceptions. */
  const putRule = (next: { level?: PermLevel | ""; perms?: Record<string, string> }) => {
    const perms: Record<string, string> =
      next.perms ?? Object.fromEntries((own?.grants || []).map((g) => [g.email, g.level]));
    const level = next.level ?? own?.default ?? "";
    // A rule that says nothing and names nobody is not a rule. Removing it
    // keeps the project's folder list honest about what is actually restricted.
    if (level === "" && Object.keys(perms).length === 0) {
      return api("DELETE", `/api/p/${project.id}/folders?prefix=${encodeURIComponent(key)}`);
    }
    return api("PUT", `/api/p/${project.id}/folders`, { prefix: key, default: level, perms });
  };

  // ---- public link (file only) ----

  async function mint(confirm: boolean) {
    setBusy(true);
    try {
      const r = await fetch(`/api/p/${project.id}/shares`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(confirm ? { path, confirm: true } : { path }),
      });
      // 409: the hub found credential-shaped strings and minted nothing. A raw
      // fetch, so this never passes through errorFor() — read the body and ask
      // INLINE. It cannot be a modalConfirm: modal.tsx holds one modal at a
      // time, and this dialog is already it.
      if (r.status === 409) {
        const { findings } = (await r.json()) as { findings?: SecretFinding[] };
        setPending(findings || []);
        return;
      }
      if (!r.ok) throw new Error(await r.text());
      const s = (await r.json()) as ShareInfo;
      setPending(null);
      // Fired here rather than in api/http.ts: this is the app's one write that
      // goes out as a raw fetch, so the table never sees it.
      track("share_created");
      // Copy on mint. Sharing used to be one click that put the URL on the
      // clipboard; routing it through a dialog must not cost the user that.
      const ok = await copyText(s.url);
      toast(ok ? "Link created and copied." : "Link created.");
      onChanged();
    } catch (e) {
      toast("Share failed: " + (e as Error).message, true);
    } finally {
      setBusy(false);
    }
  }

  /* Revoking is the audience select going back to Restricted, not a Revoke
     button: this modal hands out access, and a destructive control here would
     be the second one for the same link (the banner already has it). */
  async function setPublic(next: string) {
    if (next === "public") return mint(false);
    setPending(null);
    if (!link) return;
    await run(
      () => api("DELETE", `/api/shares/${link.token}`),
      "Link revoked — the URL no longer works.",
    );
  }

  async function setExpiry(next: string) {
    if (!link) return;
    const prev = preset;
    setPreset(next);
    try {
      await api("PATCH", `/api/shares/${link.token}`, { expires_in: next });
      onChanged();
    } catch (e) {
      toast((e as Error).message, true);
      setPreset(prev); // never leave the control claiming a lifetime the hub refused
    }
  }

  // ---- consequence text: what this change actually does ----

  // Rules nested under this folder keep winning — longest prefix wins outright
  // and rules never merge (folders.go). Someone setting a broad rule and
  // expecting it to reach inside is wrong, so say so before they find out.
  const nested = rules.filter((r) => r.prefix !== key && r.prefix.startsWith(key));

  const title = `Share "${path.split("/").pop() || project.name}"`;

  return (
    <Dialog open onOpenChange={(open) => !open && onClose()}>
      <DialogContent
        className="modal modal-wide"
        showCloseButton={false}
        onOpenAutoFocus={(e) => {
          e.preventDefault();
          copyRef.current?.focus();
        }}
      >
        <DialogTitle asChild>
          <h3>{title}</h3>
        </DialogTitle>

        {/* ---- who can open this ---- */}
        <h4 className="sd-head">Who can open this</h4>

        {isDir ? (
          <>
            <p className="ps-row">
              <span>Everyone in {orgName} can</span>
              <select
                aria-label="Access for everyone in the workspace"
                disabled={!isAdmin || busy}
                value={own?.default ?? ""}
                onChange={(e) =>
                  run(
                    () => putRule({ level: e.target.value as PermLevel | "" }),
                    "Folder access updated.",
                  )
                }
              >
                {FOLDER_LEVELS.map((l) => (
                  <option key={l.value} value={l.value}>
                    {l.label}
                  </option>
                ))}
              </select>
              {!own?.default && perms && (
                <span className="ai-tag">the project default is {perms.default}</span>
              )}
            </p>

            {own?.default === "none" && (
              <p className="ps-note">
                A folder set to <b>no access</b> is not synced to anyone outside the list below,
                and disappears from their file tree, history and search. Older share links into it
                stop working. Its <i>name</i> stays visible to project members — their devices have
                to know not to write there. If the name has to be secret too, use a separate
                project.
              </p>
            )}
            {nested.length > 0 && (
              <p className="ps-note">
                {nested.length === 1
                  ? `${nested[0].prefix} has its own rule and keeps it — this does not reach inside it.`
                  : `${nested.length} folders inside have their own rules and keep them — this does not reach inside them.`}
              </p>
            )}

            {(own?.grants.length || isAdmin) && (
              <div className="admin-list sd-people">
                {(own?.grants || []).map((g) => (
                  <div className="admin-item" key={g.email}>
                    <span className="avatar sd-avatar" style={{ background: projColor(g.email) }} aria-hidden="true">
                      {(g.email.trim()[0] || "?").toUpperCase()}
                    </span>
                    <span className="ai-main" title={g.email}>
                      {g.email}
                    </span>
                    <span className="role-cell">
                      <select
                        aria-label={`Access to this folder for ${g.email}`}
                        disabled={!isAdmin || busy}
                        value={g.level}
                        onChange={(e) => {
                          const perms: Record<string, string> = Object.fromEntries(
                            (own?.grants || []).map((x) => [x.email, x.level]),
                          );
                          perms[g.email] = e.target.value;
                          run(() => putRule({ perms }), `${g.email} updated.`);
                        }}
                      >
                        {GRANT_LEVELS.map((l) => (
                          <option key={l.value} value={l.value}>
                            {l.label}
                          </option>
                        ))}
                      </select>
                    </span>
                  </div>
                ))}
                {isAdmin && (
                  <div className="admin-item sd-add">
                    <input
                      className="sd-add-input"
                      placeholder="Email of a workspace member"
                      aria-label="Add someone to this folder"
                      value={addEmail}
                      disabled={busy}
                      onChange={(e) => setAddEmail(e.target.value)}
                    />
                    <span className="role-cell">
                      <select
                        aria-label="Access for the person being added"
                        value={addLevel}
                        disabled={busy}
                        onChange={(e) => setAddLevel(e.target.value as PermLevel)}
                      >
                        {GRANT_LEVELS.map((l) => (
                          <option key={l.value} value={l.value}>
                            {l.label}
                          </option>
                        ))}
                      </select>
                      <Button
                        variant="subtle"
                        disabled={!addEmail.trim() || busy}
                        onClick={() => {
                          const perms: Record<string, string> = Object.fromEntries(
                            (own?.grants || []).map((x) => [x.email, x.level]),
                          );
                          perms[addEmail.trim().toLowerCase()] = addLevel;
                          const who = addEmail.trim();
                          setAddEmail("");
                          run(() => putRule({ perms }), `${who} added.`);
                        }}
                      >
                        Add
                      </Button>
                    </span>
                  </div>
                )}
              </div>
            )}
          </>
        ) : (
          /* A FILE has no access of its own: there are no per-file grants, so
             everything here is inherited. Showing an editable control would be
             a lie about blast radius — the rule it wrote governs every sibling
             file too, from a dialog titled with one filename. So: name the rule
             that decides, and offer to open it. */
          <>
            <p className="ps-row">
              {governing ? (
                <span>
                  Access comes from the rule on <b>{governing.prefix}</b> — you can{" "}
                  <b>{governing.me}</b> here.
                </span>
              ) : (
                <span>
                  Everyone in {orgName} with access to this project can open it
                  {perms && <> — the project default is <b>{perms.default}</b></>}.
                </span>
              )}
            </p>
            {governing && (
              <p className="ps-row">
                <Button
                  variant="subtle"
                  onClick={() => onOpenFolder(governing.prefix.replace(/\/$/, ""))}
                >
                  Open sharing for {governing.prefix}
                </Button>
              </p>
            )}
          </>
        )}

        {/* ---- public link ---- */}
        <h4 className="sd-head">Public link</h4>

        {isDir ? (
          <p className="ps-note">
            Public links are per file — open a file inside this folder to share it with someone
            who has no account.
          </p>
        ) : (
          <>
            <p className="ps-row">
              <span>
                <Icon name={link ? "globe" : "lock"} />
              </span>
              <select
                aria-label="Public link"
                id="share-public"
                disabled={!canWrite || busy}
                value={link ? "public" : "restricted"}
                onChange={(e) => setPublic(e.target.value)}
              >
                <option value="restricted">Restricted — only people with access</option>
                <option value="public">Anyone with the link can view</option>
              </select>
            </p>

            {pending && (
              <>
                <p className="sd-warn">
                  <b>This file may contain credentials</b>
                </p>
                <p className="ps-note">{secretsMessage(pending)}</p>
                <p className="ps-row">
                  <Button variant="primary" disabled={busy} onClick={() => mint(true)}>
                    Share anyway
                  </Button>
                  <Button variant="subtle" disabled={busy} onClick={() => setPending(null)}>
                    Cancel
                  </Button>
                </p>
              </>
            )}

            {link && (
              <>
                <div className="modal-url">{link.url}</div>
                <div className="modal-expiry">
                  <label htmlFor="share-expiry">Expires</label>
                  <select
                    id="share-expiry"
                    value={preset}
                    disabled={!canWrite || busy}
                    onChange={(e) => setExpiry(e.target.value)}
                  >
                    {EXPIRY_PRESETS.map((o) => (
                      <option key={o.value} value={o.value}>
                        {o.label}
                      </option>
                    ))}
                  </select>
                  <span className="modal-expiry-note">{expiryLabel(link.expires)}</span>
                </div>
              </>
            )}
          </>
        )}

        <div className="modal-actions">
          <Button
            ref={copyRef}
            variant="primary"
            onClick={() =>
              copyText(link ? link.url : window.location.href).then((ok) =>
                toast(ok ? "Copied." : "Select and copy the link above."),
              )
            }
          >
            Copy link
          </Button>
          <Button variant="subtle" onClick={onClose}>
            Done
          </Button>
        </div>
      </DialogContent>
    </Dialog>
  );
}
