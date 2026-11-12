// SPDX-License-Identifier: AGPL-3.0-only

import { useInstalls } from "../catalog/useCatalog";
import { engineOfImage } from "../databases/useDatabases";
import { environmentOf, imageOf } from "./app";
import { usePlaces, useServices } from "./useServices";

/**
 * Where what a project runs is used, for a form that says its name is taken: its
 * database's page, or the page of what its install opens, or of one of its apps.
 * Undefined for an empty name, or a project with nothing in it to open.
 */
export function useProjectPage(project: string) {
  const places = usePlaces();
  const services = useServices().data;
  const installs = useInstalls().data?.installs;
  if (project === "") return undefined;
  const environments = new Set(places.environments.filter((e) => e.project === project).map((e) => e.id));
  const inIt = (services ?? []).filter((s) => environments.has(environmentOf(s)));
  const database = inIt.find((s) => engineOfImage(imageOf(s)) !== undefined);
  if (database) return { to: "/databases/$serviceId", params: { serviceId: database.id } } as const;
  const web = installs?.find((i) => i.project === project)?.webServiceId || inIt[0]?.id;
  return web ? ({ to: "/apps/$serviceId", params: { serviceId: web } } as const) : undefined;
}
