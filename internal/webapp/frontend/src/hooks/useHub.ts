import { useQuery, useQueryClient } from "@tanstack/react-query";
import { getJSON } from "../api/http";
import type { OrgList, PendingList, ProjectList, ProjectPerms, ShareInfo } from "../api/types";

// Hub-wide server state: the project list (polled — new projects appear
// without a reload, matching the classic app's 30s refresh) and the orgs
// the signed-in account belongs to.

const projectsQuery = {
  queryKey: ["projects"],
  queryFn: () => getJSON<ProjectList>("/api/projects"),
};

export function useProjects(enabled: boolean) {
  return useQuery({
    ...projectsQuery,
    enabled,
    refetchInterval: 30_000,
    select: (d) => d.projects || [],
  });
}

// Fetches the project list NOW, even where useProjects is disabled — which is
// the join screen (useProjects(!joinToken)), the one place that has to answer
// "is this ?p= a project I can see" before navigating. useHubRefresh cannot:
// invalidateQueries never fetches a disabled query, so it would resolve with
// the list still empty and every project-scoped invite would fall back to "/".
// It seeds the same ["projects"] entry, so the observer that mounts a tick
// later already has the data.
export function useFetchProjects() {
  const qc = useQueryClient();
  return () => qc.fetchQuery(projectsQuery);
}

export function useOrgs(enabled: boolean) {
  return useQuery({
    queryKey: ["orgs"],
    queryFn: () => getJSON<OrgList>("/api/orgs"),
    enabled,
    select: (d) => d.orgs || [],
  });
}

// One project's permission settings (default level + explicit grants). Any
// member with read may fetch it; only an admin may change it.
export function usePermissions(projectId: string | undefined) {
  return useQuery({
    queryKey: ["permissions", projectId],
    queryFn: () => getJSON<ProjectPerms>(`/api/p/${projectId}/permissions`),
    enabled: !!projectId,
  });
}

// One project's live public links. Any member with read may fetch them —
// knowing the folder you rely on is exposed is not an admin privilege. One
// cache entry feeds both the file-page banner and the Settings list, so a
// revoke in either place updates the other.
export function useShares(projectId: string | undefined, enabled = true) {
  return useQuery({
    queryKey: ["shares", projectId],
    queryFn: () => getJSON<{ shares: ShareInfo[] }>(`/api/p/${projectId}/shares`),
    enabled: !!projectId && enabled,
    select: (d) => d.shares || [],
  });
}

// Pending signups; only fetched for hub admins (the admin bar shows the
// count).
export function usePending(enabled: boolean) {
  return useQuery({
    queryKey: ["admin", "pending"],
    queryFn: () => getJSON<PendingList>("/api/admin/pending"),
    enabled,
    select: (d) => d.pending || [],
  });
}

// Resolves once the refetches land — await it before navigating to a
// just-created project, or the router's unknown-id fallback will bounce
// off the stale list.
export function useHubRefresh() {
  const qc = useQueryClient();
  return () =>
    Promise.all([
      qc.invalidateQueries({ queryKey: ["projects"] }),
      qc.invalidateQueries({ queryKey: ["orgs"] }),
    ]).then(() => {});
}
