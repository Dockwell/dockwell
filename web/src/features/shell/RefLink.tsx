// SPDX-License-Identifier: AGPL-3.0-only

import { Icon, type IconName } from "@dockwell/ui";
import { Link } from "@tanstack/react-router";
import type { ReactNode } from "react";

import "./RefLink.css";

/** The kinds of thing a reference names, each with the page it opens. */
export type RefKind = "app" | "database" | "server" | "change" | "project" | "website" | "entry" | "alert";

/** Every kind, for a test that each opens its page. */
export const REF_KINDS: readonly RefKind[] = [
  "app",
  "database",
  "server",
  "change",
  "project",
  "website",
  "entry",
  "alert",
];

const ICONS: Record<RefKind, IconName> = {
  app: "service",
  database: "database",
  server: "server",
  change: "commit",
  project: "folder",
  website: "globe",
  entry: "grid",
  alert: "bell",
};

/** The kinds whose ID says what it is, by its prefix, as IDs are made (internal/id). */
const BY_PREFIX: Record<string, RefKind> = { srv: "server", svc: "app", chg: "change", prj: "project", alt: "alert" };

/** The kind of thing an ID names, by its prefix, or undefined for one with no page. */
export function refKindOf(id: string): RefKind | undefined {
  return BY_PREFIX[id.split("_")[0] ?? ""];
}

export interface RefLinkProps {
  kind: RefKind;
  /** Its ID, which its page takes. */
  id: string;
  /** What it's called, as a person reads it; its ID when it has no name. */
  children?: ReactNode;
  /**
   * How it reads: a chip with its kind's icon, where references stand alone, as in
   * a table's cell; or plain, in the text around it, as a fact's value is.
   */
  look?: "chip" | "plain";
  className?: string;
}

/**
 * A name of a thing Dockwell keeps, as a link to its page, which answers to the
 * pointer, focus and the keyboard as any link does. A reference that names
 * something is always this, never a plain name, so wherever a person reads one
 * they can go there (#566).
 */
export function RefLink({ kind, id, children, look = "chip", className }: RefLinkProps) {
  const name = children ?? id;
  const classes = [look === "chip" ? "dw-ref" : "dw-ref-plain", className].filter(Boolean).join(" ");
  const inner =
    look === "chip" ? (
      <>
        <Icon name={ICONS[kind]} size={12} />
        <span className="dw-ref-name">{name}</span>
      </>
    ) : (
      name
    );
  switch (kind) {
    case "app":
      return (
        <Link to="/apps/$serviceId" params={{ serviceId: id }} className={classes}>
          {inner}
        </Link>
      );
    case "database":
      return (
        <Link to="/databases/$serviceId" params={{ serviceId: id }} className={classes}>
          {inner}
        </Link>
      );
    case "server":
      return (
        <Link to="/servers/$serverId" params={{ serverId: id }} className={classes}>
          {inner}
        </Link>
      );
    case "change":
      return (
        <Link to="/changes/$changeId" params={{ changeId: id }} className={classes}>
          {inner}
        </Link>
      );
    case "project":
      return (
        <Link to="/projects/$projectId" params={{ projectId: id }} className={classes}>
          {inner}
        </Link>
      );
    case "website":
      return (
        <Link to="/websites/$websiteId" params={{ websiteId: id }} className={classes}>
          {inner}
        </Link>
      );
    case "entry":
      return (
        <Link to="/catalog/$entryId" params={{ entryId: id }} className={classes}>
          {inner}
        </Link>
      );
    case "alert":
      return (
        <Link to="/alerts/$alertId" params={{ alertId: id }} className={classes}>
          {inner}
        </Link>
      );
  }
}
