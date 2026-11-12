// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { createClient } from "@connectrpc/connect";
import { createConnectQueryKey, useQuery, useTransport } from "@connectrpc/connect-query";
import {
  type CatalogInstall,
  type Domain,
  DomainService,
  ListResourcesResponseSchema,
  type Resource,
  ResourceService,
  type ServiceInfo,
  type WebsiteInfo,
} from "@dockwell/api";
import { useQuery as useTanstackQuery } from "@tanstack/react-query";
import { useMemo } from "react";

import { environmentOf } from "../apps/app";
import { DEFAULT_PROJECT, useServices } from "../apps/useServices";
import { useInstalls } from "../catalog/useCatalog";
import { useDatabases } from "../databases/useDatabases";
import { useWebsites } from "../websites/website";

/** One project, as the Projects list and its page show it: what's in it, by kind. */
export interface ProjectView {
  project: Resource;
  id: string;
  name: string;
  /** Its display name, or its name. */
  label: string;
  /** Where apps with no project go: the install's first, named default. */
  isDefault: boolean;
  /** Its environments, oldest first. */
  environments: Resource[];
  /** Everything its environments hold, by name and kind (ADR 0014). */
  inside: Resource[];
  /** Its apps: services that aren't a database or a website's own. */
  apps: ServiceInfo[];
  /** Its services that run a database. */
  databases: ServiceInfo[];
  /** What it serves, for a project made as a website (ADR 0039). */
  website: WebsiteInfo | undefined;
  /** The catalog install it is, if it is one (ADR 0029). */
  install: CatalogInstall | undefined;
  /** The domains its routes answer. */
  domains: Domain[];
  /** Holds no service and no route: nothing runs in it, and only what something used is left. */
  idle: boolean;
}

/** The most ListResources returns at once. */
const PAGE_SIZE = 1000;

/**
 * Every resource there is, of every kind, read a page at a time and kept as one list, under
 * ListResources' own query key, so a change applied in the plan sheet reads it again.
 */
export function useAllResources() {
  const transport = useTransport();
  const input = { pageSize: PAGE_SIZE };
  return useTanstackQuery({
    queryKey: createConnectQueryKey({
      schema: ResourceService.method.listResources,
      input,
      transport,
      cardinality: "finite",
    }),
    queryFn: async ({ signal }) => {
      const client = createClient(ResourceService, transport);
      const all = create(ListResourcesResponseSchema);
      let pageToken = "";
      do {
        const page = await client.listResources({ ...input, pageToken }, { signal });
        all.resources.push(...page.resources);
        pageToken = page.nextPageToken;
      } while (pageToken !== "");
      return all.resources;
    },
  });
}

/**
 * Every project the signed-in person can see, each with what's in it, sorted as people read
 * names, the default first. Undefined until what decides it is read.
 */
export function useProjects(): {
  projects: ProjectView[] | undefined;
  error: unknown;
  refetch: () => void;
  isFetching: boolean;
} {
  const all = useAllResources();
  const services = useServices().data;
  const databases = useDatabases().data?.databases;
  const websites = useWebsites().data;
  const installs = useInstalls().data?.installs;
  const domains = useQuery(DomainService.method.listDomains, {}, { staleTime: Number.POSITIVE_INFINITY }).data?.domains;
  const resources = all.data;

  const projects = useMemo(() => {
    if (!resources || !services) return undefined;
    const dbIds = new Set((databases ?? []).map((d) => d.id));
    const byProject = new Map<string, Resource[]>();
    const environments = resources.filter((r) => r.resource?.kind === "environment");
    const projectOf = new Map(environments.map((e) => [e.id, e.resource?.scope ?? ""]));
    for (const r of resources) {
      const project = projectOf.get(r.resource?.scope ?? "");
      if (!project) continue;
      byProject.set(project, [...(byProject.get(project) ?? []), r]);
    }
    const out: ProjectView[] = resources
      .filter((r) => r.resource?.kind === "project")
      .map((project) => {
        const envs = environments.filter((e) => e.resource?.scope === project.id);
        const envIds = new Set(envs.map((e) => e.id));
        const inside = byProject.get(project.id) ?? [];
        const own = services.filter((s) => envIds.has(environmentOf(s)));
        const website = websites?.find((w) => envIds.has(w.environmentId));
        const name = project.resource?.name ?? project.id;
        return {
          project,
          id: project.id,
          name,
          label: project.displayName || name,
          isDefault: name === DEFAULT_PROJECT,
          environments: envs,
          inside,
          apps: website ? [] : own.filter((s) => !dbIds.has(s.id)),
          databases: own.filter((s) => dbIds.has(s.id)),
          website,
          install: installs?.find((i) => i.projectId === project.id),
          domains: (domains ?? []).filter((d) => envIds.has(d.environmentId)),
          idle: !inside.some((r) => r.resource?.kind === "service" || r.resource?.kind === "route"),
        };
      });
    const collator = new Intl.Collator(undefined, { sensitivity: "base", numeric: true });
    return out.sort((a, b) =>
      a.isDefault !== b.isDefault ? (a.isDefault ? -1 : 1) : collator.compare(a.label, b.label),
    );
  }, [resources, services, databases, websites, installs, domains]);

  return {
    projects,
    error: all.error,
    refetch: () => void all.refetch(),
    isFetching: all.isFetching,
  };
}
