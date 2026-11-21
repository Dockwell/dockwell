// SPDX-License-Identifier: AGPL-3.0-only

import { type LogsSearch, readLogsSearch } from "../logs/LogsPage";

/**
 * What a container's page keeps in its address. It lives apart from the page, so the route
 * reads it without loading the page and everything the page loads when the panel opens.
 */
export interface ContainerDetailSearch extends LogsSearch {
  name?: string;
  view?: "inspect" | "logs";
}

export function readContainerDetailSearch(search: Record<string, unknown>): ContainerDetailSearch {
  const result: ContainerDetailSearch = readLogsSearch(search);
  if (typeof search["name"] === "string" && /^[A-Za-z0-9][A-Za-z0-9_.-]{0,254}$/.test(search["name"])) {
    result.name = search["name"];
  }
  if (search["view"] === "inspect" || search["view"] === "logs") result.view = search["view"];
  return result;
}
