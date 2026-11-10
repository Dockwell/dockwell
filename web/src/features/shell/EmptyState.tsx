// SPDX-License-Identifier: AGPL-3.0-only

import { Icon, type IconName } from "@dockwell/ui";
import type { ReactNode } from "react";

import "./EmptyState.css";

export interface EmptyStateProps {
  icon: IconName;
  /** The fact, short. */
  title: ReactNode;
  /** What it means, when the title and the action don't say it. */
  children?: ReactNode;
  /** One large button or link, when there is a next step. */
  action?: ReactNode;
}

/** A panel with nothing in it yet, which says why and invites the next step. */
export function EmptyState({ icon, title, children, action }: EmptyStateProps) {
  return (
    <div className="empty-state">
      <span className="empty-state-icon">
        <Icon name={icon} size={20} />
      </span>
      <h2 className="dw-text-heading">{title}</h2>
      {children ? <div className="empty-state-text">{children}</div> : null}
      {action}
    </div>
  );
}
