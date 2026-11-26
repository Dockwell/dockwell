// SPDX-License-Identifier: AGPL-3.0-only

import type { ReactNode } from "react";

import { cx } from "../../lib/cx";
import { useUIStrings } from "../../lib/strings";
import { Button } from "../Button/Button";
import "./PaginationBar.css";

export interface PaginationBarProps {
  /** What slice is showing, such as "1-50 of 67" or "Page 1". */
  summary?: ReactNode;
  /** Polite screen-reader announcement when the page changes, such as "Page 2". */
  announcement?: string;
  /** Accessible name for the pagination landmark, such as "Container pages". */
  label?: string;
  hasPrevious: boolean;
  hasNext: boolean;
  onPrevious: () => void;
  onNext: () => void;
  previousLabel?: ReactNode;
  nextLabel?: ReactNode;
  /** True while the next page is loading. */
  busy?: boolean;
  className?: string;
}

/**
 * The shared pagination footer for every paged list: summary on the left,
 * ghost Previous and Next buttons on the right, and a polite live region
 * for page changes.
 */
export function PaginationBar({
  summary,
  announcement,
  label,
  hasPrevious,
  hasNext,
  onPrevious,
  onNext,
  previousLabel,
  nextLabel,
  busy = false,
  className,
}: PaginationBarProps) {
  const strings = useUIStrings();
  const prev = previousLabel ?? strings.pagination.previous;
  const next = nextLabel ?? strings.pagination.next;
  const landmark = label ?? strings.pagination.label;

  return (
    <nav className={cx("dw-pager", className)} aria-label={landmark}>
      {summary ? <span className="dw-pager-summary">{summary}</span> : null}
      {announcement !== undefined ? (
        <span className="dw-sr" aria-live="polite">
          {announcement}
        </span>
      ) : null}
      <span className="dw-pager-buttons">
        <Button variant="ghost" size="sm" icon="arrow-left" disabled={!hasPrevious} onClick={onPrevious}>
          {prev}
        </Button>
        <Button variant="ghost" size="sm" icon="arrow-right" disabled={!hasNext} loading={busy} onClick={onNext}>
          {next}
        </Button>
      </span>
    </nav>
  );
}
