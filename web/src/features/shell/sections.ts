// SPDX-License-Identifier: AGPL-3.0-only

import type { IconName } from "@dockwell/ui";
import type { MessageDescriptor } from "@lingui/core";
import { msg } from "@lingui/core/macro";

import type { PageWidth } from "./Page";

interface Section {
  label: MessageDescriptor;
  icon: IconName;
  /** Reached from the phone's More page, rather than its tab bar. */
  inMore: boolean;
  /** How wide its screen grows, by its kind, which the page saying it isn't here yet takes already. */
  width: PageWidth;
}

/**
 * The sections in the design's rail, nearly all with no screen yet. The rail and
 * the phone's More page show them where the design puts them, and each opens a
 * page that says so, so the panel's layout stays put as screens arrive. A section
 * with a screen gets its own route, which takes over its path, as Backups has.
 */
export const UPCOMING = {
  websites: { label: msg`Websites`, icon: "browser", inMore: true, width: "wide" },
  databases: { label: msg`Databases`, icon: "database", inMore: true, width: "wide" },
  connections: { label: msg`Connections`, icon: "key", inMore: true, width: "wide" },
  backups: { label: msg`Backups`, icon: "archive", inMore: true, width: "wide" },
  "fleet-actions": { label: msg`Fleet actions`, icon: "play", inMore: true, width: "wide" },
  alerts: { label: msg`Alerts`, icon: "bell", inMore: true, width: "wide" },
  "scheduled-tasks": { label: msg`Scheduled tasks`, icon: "calendar", inMore: true, width: "wide" },
} as const satisfies Record<string, Section>;

export type UpcomingSection = keyof typeof UPCOMING;

/** The rail's resource sections between Apps and Domains, which have screens of their own, in its order. */
export const BEFORE_DOMAINS = ["websites", "databases", "connections"] as const satisfies readonly UpcomingSection[];

/** The rail's resource sections between Domains and the catalog, which have screens of their own, in its order. */
export const AFTER_DOMAINS = ["backups"] as const satisfies readonly UpcomingSection[];

/** The rail's operate sections after Activity, which has its screen, in its order. */
export const OPERATE = ["alerts", "scheduled-tasks"] as const satisfies readonly UpcomingSection[];

export function isUpcoming(section: string): section is UpcomingSection {
  return Object.hasOwn(UPCOMING, section);
}

/** Whether a path's first segment is Activity's: its own, or a change's page over it. */
export function inActivity(segment: string): boolean {
  return segment === "activity" || segment === "changes";
}

/** The first segment of a path, such as "servers" for /servers/srv_1. */
export function firstSegment(pathname: string): string {
  return pathname.split("/").find((part) => part !== "") ?? "";
}
