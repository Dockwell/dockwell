// SPDX-License-Identifier: AGPL-3.0-only

import { type Timestamp, timestampDate } from "@bufbuild/protobuf/wkt";
import { useLingui } from "@lingui/react/macro";
import { useCallback } from "react";

/**
 * How long ago something happened, the way the product writes it: "just
 * now", "3 min ago" and "2 h ago" under a day, then a date ("12 Oct").
 */
export function useAgo() {
  const { t, i18n } = useLingui();
  return useCallback(
    (ts: Timestamp | undefined, now: number = Date.now()): string => {
      if (!ts) return "";
      const date = timestampDate(ts);
      const minutes = Math.floor((now - date.getTime()) / 60_000);
      if (minutes < 1) return t`just now`;
      if (minutes < 60) return t`${minutes} min ago`;
      const hours = Math.floor(minutes / 60);
      if (hours < 24) return t`${hours} h ago`;
      return formatDay(date, i18n.locale, now);
    },
    [t, i18n.locale],
  );
}

/** A date in the product's style: "12 Oct", with the year when it isn't this one. */
export function formatDay(date: Date, locale: string, now: number = Date.now()): string {
  const tag = locale === "en" ? "en-GB" : locale;
  const sameYear = date.getFullYear() === new Date(now).getFullYear();
  return new Intl.DateTimeFormat(tag, {
    day: "numeric",
    month: "short",
    ...(sameYear ? {} : { year: "numeric" }),
  }).format(date);
}
