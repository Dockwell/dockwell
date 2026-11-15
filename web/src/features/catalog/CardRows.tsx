// SPDX-License-Identifier: AGPL-3.0-only

import type { ReactNode } from "react";

import "./CardRows.css";

/** One row of a card: what it's about, how it stands, and its one action. */
export interface CardRow {
  key: string;
  label: ReactNode;
  value: ReactNode;
  action?: ReactNode | undefined;
  /** What the action opens, under the row, such as the servers to choose from. */
  under?: ReactNode | undefined;
}

/**
 * A card's rows, each its label, its state and the one action that changes it, as
 * the interaction principles have it (#722): no line of prose, and no button that
 * floats away from what it acts on.
 */
export function CardRows({ rows, label }: { rows: readonly CardRow[]; label?: string }) {
  if (rows.length === 0) return null;
  return (
    <dl className="card-rows" aria-label={label}>
      {rows.map((r) => (
        <div key={r.key} className="card-row">
          <dt>{r.label}</dt>
          <dd className="card-row-value">{r.value}</dd>
          {r.action ? <dd className="card-row-action">{r.action}</dd> : null}
          {r.under ? <dd className="card-row-under">{r.under}</dd> : null}
        </div>
      ))}
    </dl>
  );
}
