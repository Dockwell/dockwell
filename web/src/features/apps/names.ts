// SPDX-License-Identifier: AGPL-3.0-only

import { useQuery } from "@connectrpc/connect-query";
import { type CatalogInstall, CatalogService, DatabaseService } from "@dockwell/api";
import { useCallback, useMemo } from "react";

/** What a page calls what it's about: its title, and the catalog app it was installed as. */
export interface ShownName {
  /** The name the person gave it at install, or its own where it's no install's. */
  title: string;
  /** The catalog app, such as "Gitea", a label beside the title. Empty where it's none. */
  app: string;
}

/** The catalog app an install is, as its card names it. */
export function appOf(install: CatalogInstall): string {
  return install.name || install.pin?.entry || "";
}

/**
 * One name for each thing across the panel (#796): a catalog install's app, and its
 * database, go by the name the person gave the install, with the catalog app's name
 * beside it; anything else by its own name. A database's own install, as PostgreSQL,
 * is the database: a tool beside it, as Adminer, keeps its own name.
 */
export function useShownName(): (
  thing: { id: string; resource?: { scope: string; name: string } | undefined },
  as: "app" | "database",
) => ShownName {
  const installs = useQuery(CatalogService.method.listCatalogInstalls, {}).data?.installs;
  const ids = useMemo(() => [...new Set((installs ?? []).map((i) => i.pin?.entry ?? "").filter(Boolean))], [installs]);
  // Which installs are a database's own, as the catalog's category says, as the database's page reads it.
  const entries = useQuery(
    CatalogService.method.listCatalogEntries,
    { ids },
    { enabled: ids.length > 0, staleTime: 30_000 },
  ).data?.entries;
  const databases = useQuery(DatabaseService.method.listDatabases, {}).data?.databases;
  return useCallback(
    (thing, as) => {
      const own = thing.resource?.name ?? thing.id;
      const install = installs?.find((i) => i.environmentId === (thing.resource?.scope ?? ""));
      if (!install) return { title: own, app: "" };
      const named = { title: install.project, app: appOf(install) };
      if (as === "database") return named;
      const ofDatabase = entries?.some((e) => e.id === install.pin?.entry && e.category === "databases") ?? false;
      if (ofDatabase) return databases?.some((d) => d.id === thing.id) ? named : { title: own, app: "" };
      // The install's web page is the app.
      return install.webServiceId === thing.id ? named : { title: own, app: "" };
    },
    [installs, entries, databases],
  );
}
