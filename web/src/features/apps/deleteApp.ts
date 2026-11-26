// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { useQuery, useTransport } from "@connectrpc/connect-query";
import {
  type CatalogInstall,
  CatalogService,
  type Change,
  ChangeService,
  ChangeState,
  type Domain,
  DomainService,
  PlanAction,
  type Resource,
  type ResourceRef,
  ResourceService,
  type ServiceInfo,
} from "@dockwell/api";
import { plural } from "@lingui/core/macro";
import { useLingui } from "@lingui/react/macro";
import { useNavigate } from "@tanstack/react-router";

import { useMay } from "../auth/may";
import { type Opening, type PlanChoice, useChangeSheet } from "../changes/ChangeSheet";
import {
  deletingProjectEdit,
  listResources,
  planDeletingProject,
  resourcesIn,
  useResourcesIn,
  useResourceWords,
} from "../projects/contents";
import { environmentOf, nameOf } from "./app";
import { useAppDomains } from "./domains";
import { DEFAULT_PROJECT, usePlaces } from "./useServices";

function projectLabel(project: Resource, installs: readonly CatalogInstall[] | undefined) {
  const name = project.resource?.name || project.id;
  return installs?.some((install) => install.projectId === project.id) ? name : project.displayName || name;
}

const appOrRoute = (kind: string | undefined) => kind === "service" || kind === "route";
const refKey = ({ kind, scope, name }: Pick<ResourceRef, "kind" | "scope" | "name">) =>
  JSON.stringify([kind, scope, name]);

export function useDeleteApps() {
  const { i18n, t } = useLingui();
  const transport = useTransport();
  const sheet = useChangeSheet();
  const mayProject = useMay()(ChangeService.method.proposeChange, { install: true });
  const words = useResourceWords();
  return (apps: readonly ServiceInfo[], onApplied: () => void) => {
    if (apps.length === 0) return;
    const n = apps.length;
    const summary = plural(n, { one: "Delete # app", other: "Delete # apps" });
    const client = createClient(ChangeService, transport);
    const onClose = (change: Change | undefined) => {
      if (change?.state === ChangeState.APPLIED) onApplied();
    };
    sheet.open({
      kind: "plan",
      retryLabel: t`Review again`,
      onClose,
      make: async () => {
        const [{ domains }, projects, environments, { installs }] = await Promise.all([
          createClient(DomainService, transport).listDomains({}),
          listResources(transport, { kind: "project" }),
          listResources(transport, { kind: "environment" }),
          createClient(CatalogService, transport).listCatalogInstalls({}),
        ]);
        const edits = apps.flatMap((app) => {
          if (!app.resource) return [];
          const { scope, name } = app.resource;
          return [
            ...domains
              .filter((domain) => domain.environmentId === scope)
              .flatMap((domain) => domain.routes)
              .filter((route) => route.service === name)
              .map((route) => ({ resource: { kind: "route", scope, name: route.name }, delete: true })),
            { resource: app.resource, baseRevision: app.revision, delete: true },
          ];
        });
        const deleting = new Set(edits.map((edit) => refKey(edit.resource)));
        const touched = new Set(apps.map(environmentOf));
        const possible = projects.filter(
          (project) => projects.length > 1 && project.resource?.name !== DEFAULT_PROJECT,
        );
        const empty = (
          await Promise.all(
            possible.map(async (project) => {
              const scopes = environments.filter((env) => env.resource?.scope === project.id).map((env) => env.id);
              if (!scopes.some((scope) => touched.has(scope))) return undefined;
              const contents = await resourcesIn(transport, [project.id, ...scopes]);
              if (
                contents.some(
                  ({ resource }) => resource && appOrRoute(resource.kind) && !deleting.has(refKey(resource)),
                )
              )
                return undefined;
              const left = contents.filter(
                ({ resource }) => resource && resource.kind !== "environment" && !deleting.has(refKey(resource)),
              );
              return { project, scopes, left };
            }),
          )
        ).filter((item) => item !== undefined);
        if (empty.length === 0) return client.proposeChange({ summary, edits });

        const listFormat = new Intl.ListFormat(i18n.locale, { type: "conjunction" });
        const entries = empty.map(({ project, left }) => ({
          name: projectLabel(project, installs),
          leftWords: left.length > 0 ? words(left) : "",
        }));
        const labels = listFormat.format(entries.map((entry) => entry.name));
        const hasExtras = entries.some((entry) => entry.leftWords !== "");
        const described = listFormat.format(
          entries.map(({ name, leftWords }) => (leftWords ? t`${name} (${leftWords})` : t`${name} (nothing else)`)),
        );
        const projectCount = empty.length;
        const appPart = plural(n, { one: "# app", other: "# apps" });
        const projectPart = plural(projectCount, { one: "# project", other: "# projects" });
        const summaryWithProjects = t`Delete ${appPart} and ${projectPart}`;
        const label = plural(projectCount, {
          one: "Also delete # project",
          other: "Also delete # projects",
        });
        const describeChoice = (on: boolean) => {
          if (entries.length === 1 && entries[0]) {
            const { name, leftWords } = entries[0];
            if (!leftWords) return t`${name} · Nothing else is in it`;
            return on ? t`${name} · Takes ${leftWords} with it` : t`${name} · Leaves ${leftWords} in it`;
          }
          if (!hasExtras) {
            return on ? t`${labels} · Nothing else is in them` : t`Leaves ${labels} empty`;
          }
          return on ? t`Takes ${described}` : t`Leaves ${described}`;
        };
        const nonAdminNote = (() => {
          if (entries.length === 1 && entries[0]) {
            const { name, leftWords } = entries[0];
            return leftWords ? t`Leaves ${name} with ${leftWords}.` : t`Leaves ${name} empty.`;
          }
          return hasExtras ? t`Leaves ${described}.` : t`Leaves ${labels} empty.`;
        })();
        const covered = new Set(empty.flatMap(({ project, scopes }) => [project.id, ...scopes]));
        const projectEdits = empty.map(({ project }) => ({
          ...deletingProjectEdit(project),
          baseRevision: project.revision,
        }));
        const make = async (on: boolean) => {
          const result = await client.proposeChange({
            summary: on ? summaryWithProjects : summary,
            edits: on ? [...edits, ...projectEdits] : edits,
          });
          // The server expands projects within the proposal. Catch an app or route added since the list read.
          const change = result.change;
          const steps = change?.plan?.resources ?? [];
          const projectScopes = new Set(covered);
          for (const { id, resource } of steps) {
            if (resource?.kind === "environment" && covered.has(resource.scope)) projectScopes.add(id);
          }
          if (
            on &&
            change &&
            steps.some(
              ({ action, resource }) =>
                action === PlanAction.DELETE &&
                resource &&
                projectScopes.has(resource.scope) &&
                appOrRoute(resource.kind) &&
                !deleting.has(refKey(resource)),
            )
          ) {
            await client.discardChange({ id: change.id });
            throw new Error(t`A project gained an app or domain. Nothing was deleted. Review again.`);
          }
          return result;
        };
        const choice = (on: boolean): PlanChoice => ({
          label,
          on,
          ...(mayProject.allowed ? { description: describeChoice(on) } : { off: mayProject.reason }),
          flip: () => ({ kind: "plan", make: () => make(!on), choice: choice(!on), onClose }),
        });
        const on = mayProject.allowed;
        return { ...(await make(on)), choice: choice(on), ...(on ? {} : { notes: [nonAdminNote] }) };
      },
    });
  };
}

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
  const last = project !== undefined && inside !== undefined && !left.some((r) => appOrRoute(r.resource?.kind));
  // A project itself is in no project, so deleting one is an admin's (ADR 0049).
  const mayProject = may(ChangeService.method.proposeChange, { install: true });
  const refused = mayProject.allowed ? undefined : mayProject.reason || undefined;
  // As Projects names it: an install by the name it was given, never its app's (#796).
  const label = project ? projectLabel(project, installs.data?.installs) : "";
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
    pending:
      places.isPending ||
      projects.isPending ||
      (project !== undefined && (installs.isPending || (environments.length > 0 && contents.isPending))),
    error: projects.error ?? (project !== undefined ? installs.error : null) ?? contents.error,
    retry: () => {
      if (projects.error) void projects.refetch();
      if (installs.error) void installs.refetch();
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
