// SPDX-License-Identifier: AGPL-3.0-only

import type { MessageInitShape } from "@bufbuild/protobuf";
import { createClient, type Transport } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import { ChangeService, type EditSchema, type Resource, ResourceService } from "@dockwell/api";
import { plural } from "@lingui/core/macro";
import { useLingui } from "@lingui/react/macro";
import { useQuery } from "@tanstack/react-query";
import { useCallback } from "react";

/** The most ListResources returns at once. */
const PAGE_SIZE = 1000;

/**
 * Everything that lives in these environments, read a page at a time: services, routes, volumes,
 * networks, variables, secrets, tasks, backup policies and database users (ADR 0014). Names and
 * where each is, never a spec.
 */
export async function resourcesIn(
  transport: Transport,
  environments: readonly string[],
  signal?: AbortSignal,
): Promise<Resource[]> {
  const client = createClient(ResourceService, transport);
  const lists = await Promise.all(
    environments.map(async (scope) => {
      const out: Resource[] = [];
      let pageToken = "";
      do {
        const page = await client.listResources({ scope, pageSize: PAGE_SIZE, pageToken }, { signal });
        out.push(...page.resources);
        pageToken = page.nextPageToken;
      } while (pageToken !== "");
      return out;
    }),
  );
  return lists.flat();
}

/** A project's environments, by ID, from the environments read. */
export function environmentsOf(project: string, environments: readonly Resource[]): string[] {
  return environments.filter((e) => e.resource?.scope === project).map((e) => e.id);
}

/**
 * The edits that delete a project with everything in it, in one change: each resource its
 * environments hold, then the project, which takes the environments with it (ADR 0036). The
 * engine orders them by what uses what, so a route goes before its service and a service before
 * what it mounts, and refuses, by name, anything made since these were read (ADR 0074).
 */
export function deletingProject(project: Resource, inside: readonly Resource[]): MessageInitShape<typeof EditSchema>[] {
  return [
    ...inside.flatMap((r) => (r.resource ? [{ resource: r.resource, delete: true }] : [])),
    { resource: { kind: "project", name: project.resource?.name ?? project.id }, delete: true },
  ];
}

/** Plans deleting a project with everything in it, read just now, so the plan has what's there. */
export async function planDeletingProject(
  transport: Transport,
  project: Resource,
  environments: readonly string[],
  summary: string,
) {
  const inside = await resourcesIn(transport, environments);
  return createClient(ChangeService, transport).proposeChange({ summary, edits: deletingProject(project, inside) });
}

/**
 * What a project's environments hold, as one list, for as long as something on screen asks. It
 * reads again when a change applies, as every resource list does.
 */
export function useResourcesIn(environments: readonly string[], enabled = true) {
  const transport = useTransport();
  const key = [...environments].sort();
  return useQuery({
    // Under ResourceService's own key, so a change applied in the plan sheet reads it again.
    queryKey: ["connect-query", { serviceName: ResourceService.typeName, methodName: "ResourcesIn", input: key }],
    queryFn: ({ signal }) => resourcesIn(transport, key, signal),
    enabled: enabled && key.length > 0,
  });
}

/** How many of each kind a list holds, in the order a person reads them. */
export function countKinds(resources: readonly Resource[]): Map<string, number> {
  const counts = new Map<string, number>();
  for (const r of resources) {
    const kind = r.resource?.kind ?? "";
    counts.set(kind, (counts.get(kind) ?? 0) + 1);
  }
  return counts;
}

/** The kinds a person names in what a project holds, in that order. */
const KINDS = ["service", "route", "variable", "secret", "volume", "task", "network", "backup.policy", "database.user"];

/**
 * Says what a list of resources is, by kind and count, such as "2 secrets and 1 volume", for
 * what goes with a project or stays in it. Anything else is "1 other resource".
 */
export function useResourceWords(): (resources: readonly Resource[]) => string {
  const { i18n } = useLingui();
  return useCallback(
    (resources) => {
      const counts = countKinds(resources);
      const words: string[] = [];
      for (const kind of KINDS) {
        const n = counts.get(kind) ?? 0;
        if (n === 0) continue;
        words.push(kindCount(kind, n));
      }
      const others = [...counts].filter(([kind]) => !KINDS.includes(kind)).reduce((sum, [, n]) => sum + n, 0);
      if (others > 0) words.push(plural(others, { one: "# other resource", other: "# other resources" }));
      const locale = i18n.locale.startsWith("en") ? "en-GB" : i18n.locale;
      return new Intl.ListFormat(locale, { style: "long", type: "conjunction" }).format(words);
    },
    [i18n],
  );
}

function kindCount(kind: string, n: number): string {
  switch (kind) {
    case "service":
      return plural(n, { one: "# app", other: "# apps" });
    case "route":
      return plural(n, { one: "# domain", other: "# domains" });
    case "variable":
      return plural(n, { one: "# variable", other: "# variables" });
    case "secret":
      return plural(n, { one: "# secret", other: "# secrets" });
    case "volume":
      return plural(n, { one: "# volume", other: "# volumes" });
    case "task":
      return plural(n, { one: "# scheduled task", other: "# scheduled tasks" });
    case "network":
      return plural(n, { one: "# network", other: "# networks" });
    case "backup.policy":
      return plural(n, { one: "# backup schedule", other: "# backup schedules" });
    default:
      return plural(n, { one: "# database user", other: "# database users" });
  }
}
