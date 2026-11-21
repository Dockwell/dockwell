// SPDX-License-Identifier: AGPL-3.0-only

import { type LogsSearch, readLogsSearch } from "../logs/search";

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

export type ComposeRunAction = "start" | "stop" | "restart";

export interface ComposeDetailSearch extends LogsSearch {
  view?: "file" | "logs" | "environment" | "backups";
  service?: string;
  action?: "delete" | "take-on" | ComposeRunAction;
}

export function readComposeDetailSearch(search: Record<string, unknown>): ComposeDetailSearch {
  const result: ComposeDetailSearch = readLogsSearch(search);
  const view = search["view"];
  if (view === "file" || view === "logs" || view === "environment" || view === "backups") result.view = view;
  const service = search["service"];
  if (typeof service === "string" && /^[\w.-]{1,128}$/.test(service)) result.service = service;
  const action = search["action"];
  if (action === "delete" || action === "take-on" || action === "start" || action === "stop" || action === "restart")
    result.action = action;
  return result;
}

export interface FleetDockerSearch {
  view?: "containers" | "composes" | "images" | "cleanup";
  attention?: boolean;
}

export function readFleetDockerSearch(search: Record<string, unknown>): FleetDockerSearch {
  if (search["view"] === "composes" || search["view"] === "images" || search["view"] === "cleanup")
    return { view: search["view"] };
  if (search["view"] === "containers")
    return { view: "containers", ...(search["attention"] === true ? { attention: true } : {}) };
  return {};
}
