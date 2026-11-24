// SPDX-License-Identifier: AGPL-3.0-only

import { DockerCleanupKind } from "@dockwell/api";
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

export type CleanupKindParam = "images" | "volumes" | "networks" | "build-cache";

export function readCleanupKindParam(raw: unknown): CleanupKindParam | undefined {
  if (raw === "images" || raw === "volumes" || raw === "networks" || raw === "build-cache") return raw;
  return undefined;
}

export function cleanupKindToParam(kind: DockerCleanupKind): CleanupKindParam | undefined {
  switch (kind) {
    case DockerCleanupKind.IMAGE:
      return "images";
    case DockerCleanupKind.VOLUME:
      return "volumes";
    case DockerCleanupKind.NETWORK:
      return "networks";
    case DockerCleanupKind.BUILD_CACHE:
      return "build-cache";
    default:
      return undefined;
  }
}

export function cleanupKindFromParam(param: CleanupKindParam): DockerCleanupKind {
  switch (param) {
    case "images":
      return DockerCleanupKind.IMAGE;
    case "volumes":
      return DockerCleanupKind.VOLUME;
    case "networks":
      return DockerCleanupKind.NETWORK;
    case "build-cache":
      return DockerCleanupKind.BUILD_CACHE;
  }
}

function readCleanupKindsParam(rawKinds: unknown, rawKind?: unknown): CleanupKindParam[] | undefined {
  const list = Array.isArray(rawKinds)
    ? rawKinds
    : typeof rawKinds === "string"
      ? rawKinds.split(",")
      : rawKind !== undefined
        ? [rawKind]
        : [];
  const cleaned = [
    ...new Set(
      list
        .map((v) => readCleanupKindParam(typeof v === "string" ? v.trim() : v))
        .filter((v): v is CleanupKindParam => v !== undefined),
    ),
  ];
  return cleaned.length > 0 && cleaned.length < 4 ? cleaned : undefined;
}

function readServerIdsParam(raw: unknown): string[] | undefined {
  const list = Array.isArray(raw)
    ? raw.filter((v): v is string => typeof v === "string")
    : typeof raw === "string"
      ? raw.split(",")
      : [];
  const cleaned = [...new Set(list.map((v) => v.trim()).filter((v) => /^[A-Za-z0-9_-]{1,80}$/.test(v)))];
  return cleaned.length > 0 ? cleaned : undefined;
}

export interface ServerCleanupSearch {
  group?: string;
  kinds?: CleanupKindParam[];
}

export function readServerCleanupSearch(search: Record<string, unknown>): ServerCleanupSearch {
  const result: ServerCleanupSearch = {};
  if (typeof search["group"] === "string") result.group = search["group"];
  const kinds = readCleanupKindsParam(search["kinds"], search["kind"]);
  if (kinds) result.kinds = kinds;
  return result;
}

export interface FleetDockerSearch {
  view?: "containers" | "composes" | "images" | "networks" | "volumes" | "cleanup";
  attention?: boolean;
  kinds?: CleanupKindParam[];
  servers?: string[];
}

export function readFleetDockerSearch(search: Record<string, unknown>): FleetDockerSearch {
  const view = search["view"];
  if (view === "containers") return search["attention"] === true ? { view, attention: true } : { view };
  if (view === "cleanup") {
    const result: FleetDockerSearch = { view };
    const kinds = readCleanupKindsParam(search["kinds"], search["kind"]);
    if (kinds) result.kinds = kinds;
    const servers = readServerIdsParam(search["servers"]);
    if (servers) result.servers = servers;
    return result;
  }
  if (view === "composes" || view === "images" || view === "networks" || view === "volumes") return { view };
  return {};
}
