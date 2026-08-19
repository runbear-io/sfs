import { useEffect } from "react";
import { useForm } from "react-hook-form";
import { zodResolver } from "@hookform/resolvers/zod";
import { z } from "zod";
import { useQueryClient } from "@tanstack/react-query";
import { api } from "../api/http";
import { modalConfirm, modalPrompt } from "../modal";
import { toast } from "../toast";
import { useHubRefresh, usePermissions, useShares } from "../hooks/useHub";
import { PROJECT_ICONS, ProjectIcon } from "./shell";
import { OPENS_NOTE, SharesTable } from "./SharesTable";
import { projColor } from "./ProjectNav";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Separator } from "@/components/ui/separator";
import { Textarea } from "@/components/ui/textarea";
import { atLeast } from "../api/types";
import type { Org, PermLevel, Project, ProjectPerms } from "../api/types";

// Settings for the open project (sidebar menu): General edits the name,
// description and icon; Public links answers "is anything of ours public right
// now?" and sits high for that reason; People says who can do what; About
// holds the identity facts; the danger zone deletes. Install/connect lives on
// the Installation page.

const MAX_DESC = 280;

// Mirrors the server's rules (projects.go) so a typo never round-trips.
const schema = z.object({
  name: z
    .string()
    .trim()
    .min(1, "Give the project a name.")
    .max(120, "Keep the name under 120 characters."),
  description: z.string().max(MAX_DESC, `Keep the description under ${MAX_DESC} characters.`),
  icon: z.string(),
});
type Values = z.infer<typeof schema>;

export function ProjectSettings({
  project,
  org,
  onDeleted,
}: {
  project: Project;
  org: Org | null;
  onDeleted: () => Promise<void>;
}) {
  const refresh = useHubRefresh();
  // Project admins, and only as UX: handleProjectUpdate enforces it too.
  // (Workspace owners resolve to admin server-side, so they still pass.)
  const mayEdit = atLeast(project.perm, "admin");

  const form = useForm<Values>({
    resolver: zodResolver(schema),
    defaultValues: {
      name: project.name,
      description: project.description ?? "",
      icon: project.icon ?? "",
    },
  });
  // Switching projects (or a refresh bringing new values) re-seeds the form,
  // so the fields never show another project's metadata.
  useEffect(() => {
    form.reset({
      name: project.name,
      description: project.description ?? "",
      icon: project.icon ?? "",
    });
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [project.id, project.name, project.description, project.icon]);

  const icon = form.watch("icon");
  const description = form.watch("description");

  const save = form.handleSubmit(async (values) => {
    // Only the dirty keys travel: PATCH is a partial update, so an untouched
    // field is never sent — and never overwritten by a stale value.
    const dirty = form.formState.dirtyFields;
    const body: Partial<Values> = {};
    if (dirty.name) body.name = values.name.trim();
    if (dirty.description) body.description = values.description;
    if (dirty.icon) body.icon = values.icon;
    if (Object.keys(body).length === 0) return;
    try {
      await api("PATCH", "/api/projects/" + project.id, body);
      toast("Saved.");
      form.reset({ ...values, name: values.name.trim() }); // clean, keeps what was typed
      await refresh(); // nav mark + dashboard header update without a reload
    } catch (e) {
      toast((e as Error).message, true); // form left alone, so nothing is lost
    }
  });

  return (
    <div className="project-settings">
      <h2>
        {project.name}
        {!atLeast(project.perm, "write") && <span className="ps-chip">Read-only</span>}
      </h2>

      <Card>
        <CardHeader>
          <CardTitle>General</CardTitle>
          <CardDescription>Name, description and icon for this project.</CardDescription>
        </CardHeader>
        <Separator />
        <CardContent>
          <form className="ps-form" onSubmit={save}>
            <div className="ps-field">
              <Label htmlFor="ps-icon-btn">Icon</Label>
              <div className="ps-icon-row">
                <span
                  className="proj-mark"
                  aria-hidden="true"
                  style={{ background: projColor(project.name) }}
                >
                  <ProjectIcon name={icon} />
                </span>
                <DropdownMenu>
                  <DropdownMenuTrigger asChild>
                    <Button id="ps-icon-btn" type="button" variant="subtle" disabled={!mayEdit}>
                      Change
                    </Button>
                  </DropdownMenuTrigger>
                  <DropdownMenuContent align="start" className="ps-icon-grid">
                    {/* Real menu items, so the grid closes on pick and works
                        from the keyboard like every other menu in the app. */}
                    <DropdownMenuItem
                      className={"ps-icon-cell" + (icon === "" ? " active" : "")}
                      title="Default"
                      aria-label="Default icon"
                      onSelect={() => form.setValue("icon", "", { shouldDirty: true })}
                    >
                      <ProjectIcon />
                    </DropdownMenuItem>
                    {Object.keys(PROJECT_ICONS).map((name) => (
                      <DropdownMenuItem
                        key={name}
                        className={"ps-icon-cell" + (icon === name ? " active" : "")}
                        title={name}
                        aria-label={name}
                        onSelect={() => form.setValue("icon", name, { shouldDirty: true })}
                      >
                        <ProjectIcon name={name} />
                      </DropdownMenuItem>
                    ))}
                  </DropdownMenuContent>
                </DropdownMenu>
              </div>
            </div>

            <div className="ps-field">
              <Label htmlFor="ps-name">Name</Label>
              <Input
                id="ps-name"
                disabled={!mayEdit}
                aria-invalid={!!form.formState.errors.name}
                aria-describedby={form.formState.errors.name ? "ps-name-err" : undefined}
                {...form.register("name")}
              />
              {form.formState.errors.name && (
                <span id="ps-name-err" role="alert" className="field-err">
                  {form.formState.errors.name.message}
                </span>
              )}
            </div>

            <div className="ps-field">
              <Label htmlFor="ps-desc">
                Description <span className="ps-opt">(optional)</span>
              </Label>
              <Textarea
                id="ps-desc"
                rows={2}
                disabled={!mayEdit}
                placeholder="What this project is for."
                aria-invalid={!!form.formState.errors.description}
                aria-describedby={form.formState.errors.description ? "ps-desc-err" : undefined}
                {...form.register("description")}
              />
              <div className="ps-meta">
                {form.formState.errors.description ? (
                  <span id="ps-desc-err" role="alert" className="field-err">
                    {form.formState.errors.description.message}
                  </span>
                ) : (
                  <span />
                )}
                <span className="ps-count">
                  {description.length} / {MAX_DESC}
                </span>
              </div>
            </div>

            {mayEdit && (
              <>
                <Separator />
                <div className="ps-actions">
                  <Button
                    id="ps-save"
                    type="submit"
                    variant="primary"
                    disabled={!form.formState.isDirty || form.formState.isSubmitting}
                  >
                    Save changes
                  </Button>
                </div>
              </>
            )}
          </form>
        </CardContent>
      </Card>

      <PublicLinks project={project} />

      <People project={project} org={org} />

      <Card>
        <CardHeader>
          <CardTitle>About</CardTitle>
        </CardHeader>
        <Separator />
        <CardContent>
          <dl className="ps-facts">
            <dt>Project id</dt>
            <dd>
              <code>{project.id}</code>
            </dd>
            {org && (
              <>
                <dt>Workspace</dt>
                <dd>{org.name}</dd>
              </>
            )}
            {project.created && (
              <>
                <dt>Created</dt>
                <dd>{new Date(project.created).toLocaleDateString()}</dd>
              </>
            )}
          </dl>
          {/* The anti-lock-in answer, at the moment someone asks it. Copy only:
              exporting is a CLI job (`bdrive export [folder]` — run in the synced
              folder, no project id), so there is no route and no button here. */}
          <p className="ps-note ps-export">
            <strong>Take your files elsewhere.</strong> Run <code>bdrive export</code> in the synced
            folder to write the whole project — every device's journal and every content blob, so
            full history and authorship — into a single archive. <code>bdrive import</code> restores
            it into any other BearDrive hub, self-hosted or cloud. Export warns first if this device
            still has changes it hasn't pushed.{" "}
            <a
              href="https://docs.beardrive.ai/reference/migration/"
              target="_blank"
              rel="noreferrer"
            >
              How migration works →
            </a>
          </p>
        </CardContent>
      </Card>

      {/* Admin-only, and only as UX: handleProjectDelete enforces it too. */}
      {mayEdit && (
        <Card className="ps-danger">
          <CardHeader>
            <CardTitle>Danger zone</CardTitle>
          </CardHeader>
          <Separator />
          <CardContent>
            <p>
              Deleting removes the project from this hub. Its files stay in storage. This can't be
              undone.
            </p>
            <Button
              variant="danger"
              onClick={async () => {
                const typed = await modalPrompt(
                  `Delete “${project.name}”?`,
                  "This can't be undone. Type the project name to confirm:",
                  "",
                  "Delete project",
                  { match: project.name, danger: true },
                );
                if (typed === null) return;
                try {
                  await api("DELETE", "/api/projects/" + project.id);
                  toast(`Deleted “${project.name}”.`);
                  await onDeleted();
                } catch (e) {
                  toast((e as Error).message, true);
                }
              }}
            >
              Delete project
            </Button>
          </CardContent>
        </Card>
      )}
    </div>
  );
}

// Public links: this project's live share URLs, where its own settings are.
// The org panel keeps the cross-project audit; nobody should have to go
// there to find one project's links.
function PublicLinks({ project }: { project: Project }) {
  const qc = useQueryClient();
  const { data: shares, error, isLoading } = useShares(project.id);
  if (error) return null; // sharing is off on this server, or single-volume mode
  return (
    <Card>
      <CardHeader>
        <CardTitle>Public links</CardTitle>
        <CardDescription>
          Files in this project that anyone with the URL can read — no account needed.
          {/* Only when the hub actually measures opens: on a hub with read
              telemetry off there is no number, so promising one would lie. */}
          {(shares || []).some((s) => s.opens !== undefined) && <> {OPENS_NOTE}</>}
        </CardDescription>
      </CardHeader>
      <Separator />
      <CardContent>
        <SharesTable
          shares={shares || []}
          loading={isLoading}
          canRevoke={atLeast(project.perm, "write")}
          onChanged={() => qc.invalidateQueries({ queryKey: ["shares", project.id] })}
          empty="No public links."
        />
      </CardContent>
    </Card>
  );
}

const LEVELS: Array<{ value: PermLevel; label: string }> = [
  { value: "admin", label: "Admin" },
  { value: "write", label: "Write" },
  { value: "read", label: "Read" },
  { value: "none", label: "No access" },
];
const LABEL: Record<string, string> = Object.fromEntries(LEVELS.map((l) => [l.value, l.label]));

// People: the level everyone in the workspace gets, plus per-person
// exceptions. Visible to every member with access; editable only for admins,
// who see the same table with live controls.
function People({ project, org }: { project: Project; org: Org | null }) {
  const qc = useQueryClient();
  const { data, error } = usePermissions(project.id);
  const isAdmin = atLeast(project.perm, "admin");
  const reload = () => {
    qc.invalidateQueries({ queryKey: ["permissions", project.id] });
    qc.invalidateQueries({ queryKey: ["projects"] });
  };
  const run = async (fn: () => Promise<unknown>, ok: string) => {
    try {
      await fn();
      toast(ok);
    } catch (e) {
      toast((e as Error).message, true);
    }
    reload();
  };

  if (error) return null; // permissions are unavailable in single-volume mode
  if (!data) return null;
  const perms: ProjectPerms = data;
  const base = `/api/p/${project.id}/permissions`;
  // Workspace owners are always project admins, whatever the grant list says.
  const owners = new Set(
    (org?.members || []).filter((m) => m.role === "owner").map((m) => m.email.toLowerCase()),
  );
  const rows = [
    ...perms.grants.filter((g) => !owners.has(g.email.toLowerCase())),
    ...[...owners].sort().map((email) => ({ email, level: "admin" as PermLevel, owner: true })),
  ];

  const addPerson = async () => {
    const email = await modalPrompt(
      "Add an exception",
      "Email of a workspace member. They get Read access; change it in the table.",
      "",
      "Add",
    );
    if (email === null || !email.trim()) return;
    await run(() => api("PUT", `${base}/${encodeURIComponent(email.trim())}`, { level: "read" }), "Added.");
  };

  return (
    <Card className="ps-people">
      <CardHeader>
        <CardTitle>People</CardTitle>
        <CardDescription>Who can see and change this project.</CardDescription>
      </CardHeader>
      <Separator />
      <CardContent>
        <p className="ps-row">
          <span>Everyone in {org?.name || "this workspace"} can</span>
          <select
            aria-label="Default access for workspace members"
            disabled={!isAdmin}
            value={perms.default}
            onChange={async (e) => {
              const level = e.target.value as PermLevel;
              if (
                level === "none" &&
                !(await modalConfirm(
                  "Make this project invite-only?",
                  "Only people listed below (and workspace owners) will see this project.",
                  "Make invite-only",
                ))
              ) {
                reload();
                return;
              }
              await run(() => api("PUT", base, { default: level }), "Default access updated.");
            }}
          >
            {LEVELS.filter((l) => l.value !== "admin").map((l) => (
              <option key={l.value} value={l.value}>
                {l.label}
              </option>
            ))}
          </select>
        </p>
        {perms.default === "none" && (
          <p className="ps-note">
            This project is invite-only: only the people below and workspace owners can see it.
          </p>
        )}

        <div className="ps-people-head">
          <h4>Exceptions</h4>
          {isAdmin && (
            <Button type="button" variant="subtle" onClick={addPerson}>
              + Add
            </Button>
          )}
        </div>
        {rows.length === 0 ? (
          <p className="ps-note">No exceptions — everyone gets the access above.</p>
        ) : (
          <div className="admin-list">
            {rows.map((g) => {
              const isOwner = "owner" in g;
              return (
                <div className="admin-item" key={g.email}>
                  <span className="ai-main" title={g.email}>
                    {g.email}
                    {perms.creator && g.email.toLowerCase() === perms.creator.toLowerCase() && (
                      <span className="ai-tag"> (creator)</span>
                    )}
                  </span>
                  {isOwner ? (
                    <span className="ai-tag">Workspace owner — always admin</span>
                  ) : (
                    <span className="role-cell">
                      <select
                        aria-label={`Access for ${g.email}`}
                        disabled={!isAdmin}
                        value={g.level}
                        onChange={(e) =>
                          run(
                            () =>
                              api("PUT", `${base}/${encodeURIComponent(g.email)}`, {
                                level: e.target.value,
                              }),
                            `${g.email} is now ${LABEL[e.target.value] || e.target.value}.`,
                          )
                        }
                      >
                        {LEVELS.map((l) => (
                          <option key={l.value} value={l.value}>
                            {l.label}
                          </option>
                        ))}
                      </select>
                      {isAdmin && (
                        <button
                          className="ai-del"
                          aria-label={`Remove exception for ${g.email}`}
                          onClick={() =>
                            run(
                              () => api("DELETE", `${base}/${encodeURIComponent(g.email)}`),
                              "Reverted to the default access.",
                            )
                          }
                        >
                          Remove
                        </button>
                      )}
                    </span>
                  )}
                </div>
              );
            })}
          </div>
        )}
      </CardContent>
    </Card>
  );
}
