// SPDX-License-Identifier: AGPL-3.0-only

/**
 * A date and time the way the product writes them: day before month, short
 * month names ("3 Nov 2026, 21:40"), and the locale's own order elsewhere.
 */
export function formatDateTime(date: Date, locale: string): string {
  // English follows the design system's "12 Oct" style rather than US order.
  const tag = locale === "en" ? "en-GB" : locale;
  return new Intl.DateTimeFormat(tag, {
    day: "numeric",
    month: "short",
    year: "numeric",
    hour: "2-digit",
    minute: "2-digit",
  }).format(date);
}
