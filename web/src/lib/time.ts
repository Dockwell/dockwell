// SPDX-License-Identifier: AGPL-3.0-only

import { plural } from "@lingui/core/macro";
import { useLingui } from "@lingui/react/macro";
import { useEffect, useMemo, useState } from "react";

import { formatDay } from "./format";

const SECOND = 1_000;
const MINUTE = 60 * SECOND;
const HOUR = 60 * MINUTE;
const DAY = 24 * HOUR;

/** The current time, updated every interval, for "2 s ago" that keeps counting. */
export function useNow(interval = SECOND): number {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const timer = setInterval(() => setNow(Date.now()), interval);
    return () => clearInterval(timer);
  }, [interval]);
  return now;
}

export interface TimeWords {
  /** A length of time, rounded to what a person acts on: "40 s", "12 min", "5 hours", "41 days". */
  span(ms: number): string;
  /**
   * How long ago something was: "1 s ago" at the least, then "3 min ago",
   * "2 h ago", then a date, "12 Oct". Never "now", which reads oddly after
   * "last reported", and never a time to come, which a small difference
   * between clocks could otherwise give.
   */
  ago(then: Date, now: number): string;
}

/** Times in words, in the active locale. */
export function useTimeWords(): TimeWords {
  const { t, i18n } = useLingui();
  return useMemo<TimeWords>(
    () => ({
      span(ms) {
        if (ms < MINUTE) {
          const count = Math.max(0, Math.floor(ms / SECOND));
          return t`${count} s`;
        }
        if (ms < HOUR) {
          const count = Math.floor(ms / MINUTE);
          return t`${count} min`;
        }
        if (ms < DAY) {
          const count = Math.floor(ms / HOUR);
          return plural(count, { one: "# hour", other: "# hours" });
        }
        const count = Math.floor(ms / DAY);
        return plural(count, { one: "# day", other: "# days" });
      },
      ago(then, now) {
        const ms = now - then.getTime();
        if (ms < MINUTE) {
          const count = Math.max(1, Math.floor(ms / SECOND));
          return t`${count} s ago`;
        }
        if (ms < HOUR) {
          const count = Math.floor(ms / MINUTE);
          return t`${count} min ago`;
        }
        if (ms < DAY) {
          const count = Math.floor(ms / HOUR);
          return t`${count} h ago`;
        }
        return formatDay(then, i18n.locale);
      },
    }),
    // The words change with the locale, which t and i18n carry.
    [t, i18n],
  );
}
