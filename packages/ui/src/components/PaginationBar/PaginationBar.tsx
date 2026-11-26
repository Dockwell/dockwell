// SPDX-License-Identifier: AGPL-3.0-only

import type { ReactNode } from "react";

import { cx } from "../../lib/cx";
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
  previousLabel = "Previous",
  nextLabel = "Next",
  busy = false,
  className,
}: PaginationBarProps) {
  const content = (
    <>
      {summary ? <span className="dw-pager-summary">{summary}</span> : null}
      {announcement !== undefined ? (
        <span className="dw-sr" aria-live="polite">
          {announcement}
        </span>
      ) : null}
      <span className="dw-pager-buttons">
        <Button variant="ghost" size="sm" icon="arrow-left" disabled={!hasPrevious} onClick={onPrevious}>
          {previousLabel}
        </Button>
        <Button variant="ghost" size="sm" icon="arrow-right" disabled={!hasNext} loading={busy} onClick={onNext}>
          {nextLabel}
        </Button>
      </span>
    </>
  );

  if (label) {
    return (
      <nav className={cx("dw-pager", className)} aria-label={label}>
        {content}
      </nav>
    );
  }
  return <div className={cx("dw-pager", className)}>{content}</div>;
}
