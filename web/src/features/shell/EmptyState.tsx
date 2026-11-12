// SPDX-License-Identifier: AGPL-3.0-only

import { cx, Icon, type IconName } from "@dockwell/ui";
import type { ReactNode } from "react";

import "./EmptyState.css";

export interface EmptyStateProps {
  icon: IconName;
  /** The fact, short. */
  title: ReactNode;
  /** What it means, when the title and the action don't say it. One line. */
  children?: ReactNode;
  /** One large button or link, when there is a next step. */
  action?: ReactNode;
  /**
   * Cards to choose the next step from, such as the ways a first app comes in. They are
   * the one layer of cards, so the state draws no panel of its own around them.
   */
  choices?: ReactNode;
  className?: string;
}

/**
 * Nothing here yet: why, and the next step. The one way every screen says so: an icon,
 * the fact, a line at most and the action, on a resting panel. It is always one layer.
 * Standing in a page, it is the panel; inside a panel, a table or a sheet, it is that
 * container's body and draws no box of its own; and with choices, the choices are the
 * cards and it draws none either.
 */
export function EmptyState({ icon, title, children, action, choices, className }: EmptyStateProps) {
  return (
    <div className={cx("empty-state", choices ? "has-choices" : null, className)}>
      <span className="empty-state-icon">
        <Icon name={icon} size={20} />
      </span>
      <h2 className="dw-text-heading empty-state-title">{title}</h2>
      {children ? <div className="empty-state-text">{children}</div> : null}
      {action ? <div className="empty-state-action">{action}</div> : null}
      {choices ? <div className="empty-state-choices">{choices}</div> : null}
    </div>
  );
}
