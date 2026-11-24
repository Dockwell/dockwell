// SPDX-License-Identifier: AGPL-3.0-only

import { useQuery, useTransport } from "@connectrpc/connect-query";
import {
  CatalogService,
  type Change,
  ChangeService,
  ChangeState,
  type Domain,
  type Resource,
  ResourceService,
  type ServiceInfo,
} from "@dockwell/api";
import { useLingui } from "@lingui/react/macro";
import { useNavigate } from "@tanstack/react-router";

import { useMay } from "../auth/may";
import { type Opening, type PlanChoice, useChangeSheet } from "../changes/ChangeSheet";
import { planDeletingProject, useResourcesIn, useResourceWords } from "../projects/contents";
import { environmentOf, nameOf } from "./app";
import { useAppDomains } from "./domains";
import { usePlaces } from "./useServices";

/** An app's project, as deleting the app sees it. */
export interface AppProject {
  /** The project. */
  project: Resource;
  /** As the panel shows it: its display name, or its name. */
  label: string;
  /** The app is the last thing in it: no other service, and no route but the app's own. */
  last: boolean;
  /** What else is in it, in words, such as "2 secrets and 1 volume", or "" for nothing. */
  left: string;
  /** All it holds, the app included, in words, such as "2 apps, 1 domain and 2 secrets". */
  holds: string;
  /** Why it can't go, for someone whose role doesn't cover deleting a project (ADR 0049, 0059), or undefined. */
  refused: string | undefined;
  /** Opens the plan to delete the project with everything in it, this app included. */
  open: () => void;
}

/**
 * Deleting an app is a plan too: this only opens it. A route can't outlive what it serves, so the
 * plan takes the app's domains with it, and lists them. When the app is the last thing in its
 * project, the plan takes the project too, with what the app left in it, behind a switch that's on
 * (ADR 0074): the default project, where the next app goes, is never offered. Once it applies, the
 * app's pages have nothing to show, so the list comes next.
 */
export function useDeleteApp(service: ServiceInfo, options?: DeleteAppOptions) {
  return useDeleteAppPlan(service, useAppDomains(service).domains, options);
}

interface DeleteAppOptions {
  /** Where to go once it's deleted: the list it was on. */
  after?: "/apps" | "/databases";
  /** What goes with it, as a database's users go with its service. */
  alongside?: readonly Resource[];
}

export function useDeleteAppPlan(
  service: ServiceInfo,
  domains: readonly Domain[],
  options: DeleteAppOptions = {},
): {
  routes: number;
  open: () => void;
  project: AppProject | undefined;
  pending: boolean;
  error: Error | null;
  retry: () => void;
} {
  const after = options.after ?? "/apps";
  const alongside = options.alongside ?? [];
  const { t } = useLingui();
  const sheet = useChangeSheet();
  const navigate = useNavigate();
  const transport = useTransport();
  const may = useMay();
  const places = usePlaces();
  const words = useResourceWords();
  const name = nameOf(service);
  const resource = service.resource;
  const place = places.place(environmentOf(service));
  const projects = useQuery(ResourceService.method.listResources, { kind: "project" });
  const installs = useQuery(CatalogService.method.listCatalogInstalls, {});
  const project = place && !place.isDefault ? projects.data?.resources.find((p) => p.id === place.scope) : undefined;
  const environments = project ? places.environments.filter((e) => e.scope === project.id).map((e) => e.id) : [];
  const contents = useResourcesIn(environments, project !== undefined);
  const inside = contents.data;
  const routes = domains.flatMap((domain) => domain.routes.filter((route) => route.service === name));
  const mine = new Set(routes.map((route) => route.name));
  const isMine = (r: Resource) =>
    (r.resource?.kind === "service" && r.resource.name === name) ||
    (r.resource?.kind === "route" && mine.has(r.resource.name)) ||
    alongside.some((a) => a.id === r.id);
  const left = (inside ?? []).filter((r) => !isMine(r));
  const last =
    project !== undefined &&
    inside !== undefined &&
    !left.some((r) => ["service", "route"].includes(r.resource?.kind ?? ""));
  // A project itself is in no project, so deleting one is an admin's (ADR 0049).
  const mayProject = may(ChangeService.method.proposeChange, { install: true });
  const refused = mayProject.allowed ? undefined : mayProject.reason || undefined;
  // As Projects names it: an install by the name it was given, never its app's (#796).
  const label = project
    ? (installs.data?.installs.some((i) => i.projectId === project.id)
        ? project.resource?.name
        : project.displayName) ||
      project.resource?.name ||
      project.id
    : "";
  const leftWords = left.length > 0 ? words(left) : "";

  const onClose = (change: Change | undefined) => {
    if (change && change.state !== ChangeState.PLANNED && change.state !== ChangeState.FAILED) {
      void navigate({ to: after });
    }
  };
  const appOnly = (choice?: PlanChoice): Opening => ({
    kind: "propose",
    summary: t`Delete ${name}`,
    edits: [
      ...routes.map((route) => ({
        resource: { kind: "route", scope: resource?.scope ?? "", name: route.name },
        delete: true,
      })),
      ...alongside.flatMap((a) => (a.resource ? [{ resource: a.resource, delete: true }] : [])),
      { resource: { kind: "service", scope: resource?.scope ?? "", name }, delete: true },
    ],
    onClose,
    ...(choice ? { choice } : {}),
  });
  const withProject = (of: Resource, summary: string, choice?: PlanChoice): Opening => ({
    kind: "plan",
    make: () => planDeletingProject(transport, of, summary),
    onClose,
    ...(choice ? { choice } : {}),
  });
  const choice = (on: boolean): PlanChoice => ({
    label: t`Also delete the project ${label}`,
    on,
    ...(refused
      ? { off: refused }
      : {
          description: leftWords
            ? on
              ? t`Takes ${leftWords} with it`
              : t`Leaves ${leftWords} in it`
            : t`Nothing else is in it`,
        }),
    flip: () =>
      on || !project
        ? appOnly(choice(false))
        : withProject(project, t`Delete ${name} and the project ${label}`, choice(true)),
  });

  const open = () => {
    if (!project || !last) {
      sheet.open(appOnly());
      return;
    }
    sheet.open(
      refused ? appOnly(choice(false)) : withProject(project, t`Delete ${name} and the project ${label}`, choice(true)),
    );
  };
  return {
    routes: routes.length,
    open,
    pending: places.isPending || (project !== undefined && environments.length > 0 && contents.isPending),
    error: projects.error ?? contents.error,
    retry: () => {
      if (projects.error) void projects.refetch();
      if (contents.error) void contents.refetch();
    },
    project: project
      ? {
          project,
          label,
          last,
          left: leftWords,
          holds: words(inside ?? []),
          refused,
          open: () => sheet.open(withProject(project, t`Delete the project ${label}`)),
        }
      : undefined,
  };
}
