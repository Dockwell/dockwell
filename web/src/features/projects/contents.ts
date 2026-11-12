// SPDX-License-Identifier: AGPL-3.0-only

import { createClient, type Transport } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import { ChangeService, type Resource, ResourceService } from "@dockwell/api";
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

/**
 * Plans deleting a project with everything in it, in one change: one edit that asks the engine to
 * take what lives in its environments too (Edit.with_contents), so it lists each item from what's
 * there as it plans, orders them by what uses what, and checks each as any edit (ADR 0074). The
 * CLI's `dockwell projects delete --with-contents` sends the same edit.
 */
export async function planDeletingProject(transport: Transport, project: Resource, summary: string) {
  return createClient(ChangeService, transport).proposeChange({
    summary,
    edits: [
      { resource: { kind: "project", name: project.resource?.name ?? project.id }, delete: true, withContents: true },
    ],
  });
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
