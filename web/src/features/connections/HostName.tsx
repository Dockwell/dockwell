// SPDX-License-Identifier: AGPL-3.0-only

import { Fragment } from "react";

import "./HostName.css";

/** A host's label longer than this is shortened in the middle, as an account ID in it is. */
const LONGEST = 24;

/** A label shortened in the middle, keeping its start and end, which tell two apart. */
export function shortLabel(label: string): string {
  return label.length > LONGEST ? `${label.slice(0, 6)}…${label.slice(-4)}` : label;
}

/**
 * A host as a card shows it: a label too long for a line, such as an R2 account ID,
 * shortened in the middle, and a line break allowed only after a dot or a hyphen,
 * never inside a word. Hovering says it whole.
 */
export function HostName({ host }: { host: string }) {
  const labels = host.split(".");
  return (
    <code className="host-name" title={host}>
      {labels.map((label, i) => (
        // biome-ignore lint/suspicious/noArrayIndexKey: a host's labels are in a fixed order, and may repeat.
        <Fragment key={i}>
          {i > 0 ? "." : null}
          {i > 0 ? <wbr /> : null}
          {shortLabel(label)
            .split("-")
            .map((part, j) => (
              // biome-ignore lint/suspicious/noArrayIndexKey: a label's parts are in a fixed order, and may repeat.
              <Fragment key={j}>
                {j > 0 ? "-" : null}
                {j > 0 ? <wbr /> : null}
                {part}
              </Fragment>
            ))}
        </Fragment>
      ))}
    </code>
  );
}
