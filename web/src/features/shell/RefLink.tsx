// SPDX-License-Identifier: AGPL-3.0-only

import { Icon, type IconName } from "@dockwell/ui";
import { Link } from "@tanstack/react-router";
import type { ReactNode } from "react";

import "./RefLink.css";

/** The kinds of thing a reference names, each with the page it opens. */
export type RefKind = "app";

const ICONS: Record<RefKind, IconName> = { app: "service" };

export interface RefLinkProps {
  kind: RefKind;
  /** Its ID, which its page takes. */
  id: string;
  /** What it's called, as a person reads it; its ID when it has no name. */
  children?: ReactNode;
  className?: string;
}

/**
 * A name of a thing Dockwell keeps, as a link to its page: a chip with its kind's
 * icon, which answers to the pointer, focus and the keyboard as any link does. A
 * reference that names something is always this, never a plain name, so wherever a
 * person reads one they can go there (#566).
 */
export function RefLink({ kind, id, children, className }: RefLinkProps) {
  const name = children ?? id;
  const inner = (
    <>
      <Icon name={ICONS[kind]} size={12} />
      <span className="dw-ref-name">{name}</span>
    </>
  );
  const classes = ["dw-ref", className].filter(Boolean).join(" ");
  switch (kind) {
    case "app":
      return (
        <Link to="/apps/$serviceId" params={{ serviceId: id }} className={classes}>
          {inner}
        </Link>
      );
  }
}
