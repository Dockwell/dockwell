// SPDX-License-Identifier: AGPL-3.0-only

import { useLingui } from "@lingui/react/macro";

/** How wide the bar is, as the mock draws it. */
const WIDTH = 56;

/**
 * How much of something is used, as a short bar beside its words, such as a database's
 * connections: "4 of 100". With no most, it's the number alone.
 */
export function Meter({ used, of, locale }: { used: number; of: number; locale: string }) {
  const { t } = useLingui();
  const format = (n: number) => n.toLocaleString(locale);
  if (of <= 0) return <span>{format(used)}</span>;
  const filled = Math.max(2, Math.round((WIDTH * Math.min(used, of)) / of));
  const usedWords = format(used);
  const ofWords = format(of);
  return (
    <span className="databases-meter">
      <span className="databases-meter-bar" aria-hidden="true">
        <span className="databases-meter-fill" style={{ width: `${filled}px` }} />
      </span>
      <span>{t`${usedWords} of ${ofWords}`}</span>
    </span>
  );
}
