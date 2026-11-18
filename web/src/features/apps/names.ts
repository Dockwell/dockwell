// SPDX-License-Identifier: AGPL-3.0-only

import { useQuery } from "@connectrpc/connect-query";
import { type CatalogInstall, CatalogService } from "@dockwell/api";
import { useCallback } from "react";

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
 * beside it; anything else by its own name.
 */
export function useShownName(): (
  thing: { id: string; resource?: { scope: string; name: string } | undefined },
  as: "app" | "database",
) => ShownName {
  const installs = useQuery(CatalogService.method.listCatalogInstalls, {}).data?.installs;
  return useCallback(
    (thing, as) => {
      const own = thing.resource?.name ?? thing.id;
      const install = installs?.find((i) => i.environmentId === (thing.resource?.scope ?? ""));
      if (!install) return { title: own, app: "" };
      // The install's web page is the app, and its database is the database: each the install.
      const isIt = as === "database" || install.webServiceId === thing.id;
      return isIt ? { title: install.project, app: appOf(install) } : { title: own, app: "" };
    },
    [installs],
  );
}
