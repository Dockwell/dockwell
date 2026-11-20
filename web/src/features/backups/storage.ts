// SPDX-License-Identifier: AGPL-3.0-only

import { type BackupDestinationInfo, type ConnectionInfo, LinkKind } from "@dockwell/api";
import { useMemo } from "react";

import { hostOf } from "../connections/providers";
import { appsOf, type ReadingApp } from "../connections/storage";
import { useConnections } from "../connections/useConnections";
import { useDestinations } from "./DestinationsPanel";

/**
 * One piece of object storage, combined from S3 connections and backup destinations
 * so Backups, Integrations and Connections count each bucket once (ADR 0099, #831).
 */
export interface StorageItem {
  /** Stable key across pickers and lists: `conn:<name>` or `dest:<name>`. */
  key: string;
  /** The connection's name where there is one, otherwise the destination's. */
  name: string;
  /** What the panel calls it: its connection's label, or its name. */
  label: string;
  /** Endpoint hostname, where the caller may see it (empty below admin). */
  host: string;
  /** Bucket name, where the caller may see it (empty below admin). */
  bucket: string;
  /** The S3 connection, where backed by or offered as one. */
  connection?: ConnectionInfo;
  /** The backup destination paired with this connection, or a standalone legacy one. */
  destination?: BackupDestinationInfo;
  /** True for a legacy inline destination that names no connection. */
  standalone: boolean;
}

export type PickerStatus =
  | { state: "ready"; destinationName: string }
  | { state: "available" }
  | { state: "serves-app"; apps: ReadingApp[] }
  | { state: "shared" }
  | { state: "held" };

/** Backup destinations that use the named connection. */
export function destinationsOn(
  connectionName: string,
  destinations: readonly BackupDestinationInfo[],
): BackupDestinationInfo[] {
  return destinations.filter((d) => d.connection === connectionName);
}

/**
 * Combines S3 connections and backup destinations into one deduplicated list:
 * S3 connections first (paired with any destination that names them), then any
 * standalone legacy destination that names no visible connection.
 */
export function combineStorage(
  connections: readonly ConnectionInfo[],
  destinations: readonly BackupDestinationInfo[],
): StorageItem[] {
  const s3 = connections.filter((c) => c.kind === LinkKind.S3);
  const byConn = new Map<string, BackupDestinationInfo[]>();
  for (const d of destinations) {
    if (!d.connection) continue;
    const list = byConn.get(d.connection) ?? [];
    list.push(d);
    byConn.set(d.connection, list);
  }
  const pairedIds = new Set<string>();
  const items: StorageItem[] = [];
  for (const c of s3) {
    const matched = byConn.get(c.name) ?? [];
    const primary = matched[0];
    if (primary) pairedIds.add(primary.id);
    items.push({
      key: `conn:${c.name}`,
      name: c.name,
      label: c.displayName || c.name,
      host: hostOf(c.endpoint || primary?.endpoint || ""),
      bucket: c.bucket || primary?.bucket || "",
      connection: c,
      destination: primary,
      standalone: false,
    });
    // If more than one destination ever names the same connection, keep extras visible too.
    for (const extra of matched.slice(1)) {
      pairedIds.add(extra.id);
      items.push({
        key: `dest:${extra.name}`,
        name: extra.name,
        label: extra.name,
        host: hostOf(extra.endpoint || c.endpoint || ""),
        bucket: extra.bucket || c.bucket || "",
        connection: c,
        destination: extra,
        standalone: false,
      });
    }
  }
  for (const d of destinations) {
    if (pairedIds.has(d.id)) continue;
    items.push({
      key: `dest:${d.name}`,
      name: d.name,
      label: d.name,
      host: hostOf(d.endpoint),
      bucket: d.bucket,
      destination: d,
      standalone: d.connection === "",
    });
  }
  return items;
}

/** Whether a storage item can take backups right now, or why it can't. */
export function pickerStatus(item: StorageItem): PickerStatus {
  if (item.destination) return { state: "ready", destinationName: item.destination.name };
  const c = item.connection;
  if (!c) return { state: "available" };
  if (c.uses.length > 0) return { state: "serves-app", apps: appsOf(c) };
  if (c.shared) return { state: "shared" };
  if (c.held) return { state: "held" };
  return { state: "available" };
}

/**
 * Picks a free backup.destination name from a connection's name: the same name
 * where free, then numbered.
 */
export function freeDestinationName(base: string, existing: readonly BackupDestinationInfo[]): string {
  const taken = new Set(existing.map((d) => d.name));
  const clean = base || "backups";
  if (!taken.has(clean)) return clean;
  for (let i = 2; ; i++) if (!taken.has(`${clean}-${i}`)) return `${clean}-${i}`;
}

/**
 * The default choice for a new schedule: the first existing backup destination,
 * or the first unused S3 connection when the caller may set one up, else local-only.
 */
export function firstAvailableStorage(items: readonly StorageItem[], maySetUp: boolean): string {
  const ready = items.find((i) => pickerStatus(i).state === "ready");
  if (ready) return ready.key;
  if (maySetUp) {
    const avail = items.find((i) => pickerStatus(i).state === "available");
    if (avail) return avail.key;
  }
  return "";
}

/** Finds the storage item key matching an existing policy's destination name. */
export function storageKeyForDestination(items: readonly StorageItem[], destinationName: string): string {
  if (!destinationName) return "";
  const matched = items.find((i) => i.destination?.name === destinationName);
  return matched ? matched.key : `dest:${destinationName}`;
}

/**
 * Reads both S3 connections and backup destinations and combines them into one
 * deduplicated list. Optional overrides let callers pass pre-read destinations.
 */
export function useStorage(overrideDestinations?: readonly BackupDestinationInfo[]) {
  const connQuery = useConnections();
  const destQuery = useDestinations();
  const allConnections = connQuery.data?.connections ?? [];
  const destinations = overrideDestinations ?? destQuery.data?.destinations ?? [];
  const items = useMemo(() => combineStorage(allConnections, destinations), [allConnections, destinations]);
  const s3Connections = useMemo(() => allConnections.filter((c) => c.kind === LinkKind.S3), [allConnections]);
  const ready = connQuery.data !== undefined && (overrideDestinations !== undefined || destQuery.data !== undefined);
  return {
    items,
    connections: s3Connections,
    allConnections,
    destinations,
    ready,
    isPending: !ready && !destQuery.error && !connQuery.error,
    isFetching: connQuery.isFetching || destQuery.isFetching,
    error: destQuery.error ?? connQuery.error,
    refetch: () => Promise.all([connQuery.refetch(), destQuery.refetch()]),
  };
}
