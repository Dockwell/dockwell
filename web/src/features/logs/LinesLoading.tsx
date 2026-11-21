// SPDX-License-Identifier: AGPL-3.0-only

import { Loading, Skeleton } from "@dockwell/ui";
import { useLingui } from "@lingui/react/macro";

import "./LinesLoading.css";

/**
 * Lines on their way, in the log's own columns: a time, a source where there are
 * several, and the line. Past a few seconds, what the wait is.
 */
export function LinesLoading({
  source,
  searching = false,
  label,
  slow,
}: {
  /** The log shows a source column, where there are several. */
  source: boolean;
  /** A search the person started, of all the servers keep. */
  searching?: boolean;
  /** What is read, where it isn't an app's logs, such as "Reading the system log". */
  label?: string;
  /** What a long wait for it is. */
  slow?: string;
}) {
  const { t } = useLingui();
  const widths = ["72%", "48%", "86%", "34%", "64%", "78%", "42%", "58%", "90%", "38%", "68%", "52%"];
  return (
    <Loading
      label={label ?? (searching ? t`Searching all the servers keep` : t`Reading logs`)}
      slow={
        slow ??
        (searching ? t`It reads each container's log, at most for 30 s.` : t`The server is slow to send these lines`)
      }
      slowAfter={searching ? 0 : undefined}
      className="logs-skeleton"
    >
      {widths.map((width, index) => (
        <div key={width} className={source ? "logs-skeleton-line has-source" : "logs-skeleton-line"} aria-hidden="true">
          <Skeleton text="code-sm" width={72} />
          {source ? <Skeleton text="code-sm" width={[88, 64, 96][index % 3]} /> : null}
          <span />
          <Skeleton text="code-sm" width={width} />
        </div>
      ))}
    </Loading>
  );
}
