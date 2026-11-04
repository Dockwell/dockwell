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

const BYTE_UNITS = ["B", "KB", "MB", "GB", "TB", "PB"] as const;

/**
 * The largest unit, as an index into BYTE_UNITS, that keeps bytes at 1 or
 * more once rounded, so 1,023.6 KB reads "1 MB" rather than "1,024 KB".
 * From 10 up an amount has no decimals, so rounding to whole numbers is
 * what shows.
 */
function unitFor(bytes: number): number {
  let unit = 0;
  while (Math.round(bytes / 1024 ** unit) >= 1024 && unit < BYTE_UNITS.length - 1) unit++;
  return unit;
}

/** Bytes as a number in unit: one decimal below 10, none above, and no trailing zero. */
function amountIn(bytes: number, unit: number, locale: string): string {
  const value = bytes / 1024 ** unit;
  const digits = unit > 0 && value < 10 ? 1 : 0;
  return new Intl.NumberFormat(locale, { maximumFractionDigits: digits }).format(value);
}

/**
 * Bytes in the largest unit that keeps the number readable, with a space:
 * "512 MB", "2.1 GB", "8 GB", "18 GB".
 */
export function formatBytes(bytes: number | bigint, locale: string): string {
  const n = Number(bytes);
  const unit = unitFor(n);
  return `${amountIn(n, unit, locale)} ${BYTE_UNITS[unit]}`;
}

/**
 * A part of a whole, both in the whole's unit, for "7.4 of 8 GB": the part
 * as a bare number and the whole with its unit.
 */
export function formatBytesOf(part: number | bigint, whole: number | bigint, locale: string): [string, string] {
  const unit = unitFor(Number(whole));
  return [amountIn(Number(part), unit, locale), `${amountIn(Number(whole), unit, locale)} ${BYTE_UNITS[unit]}`];
}

/** A share of 100 as a whole percentage: "12%". */
export function formatPercent(value: number, locale: string): string {
  return new Intl.NumberFormat(locale, { style: "percent", maximumFractionDigits: 0 }).format(value / 100);
}

/** A number with a fixed count of decimals, in the locale's digits: "23.4", "0.84". */
export function formatFixed(value: number, digits: number, locale: string): string {
  return new Intl.NumberFormat(locale, { minimumFractionDigits: digits, maximumFractionDigits: digits }).format(value);
}

const RATE_UNITS = ["bit/s", "kbit/s", "Mbit/s", "Gbit/s"] as const;

/**
 * A rate in bytes per second as bits, the way links are sold, in the unit
 * that keeps the number readable: one decimal below 10, none above. Returns
 * the number and the unit apart, for a Metric's value and unit.
 */
export function formatBitRate(bytesPerSecond: number | bigint, locale: string): [string, string] {
  let value = Number(bytesPerSecond) * 8;
  let unit = 0;
  // Rounded first, so 999,960 bit/s reads "1 Mbit/s" rather than "1,000 kbit/s".
  while (Math.round(value) >= 1000 && unit < RATE_UNITS.length - 1) {
    value /= 1000;
    unit++;
  }
  const digits = unit > 0 && value < 10 ? 1 : 0;
  return [new Intl.NumberFormat(locale, { maximumFractionDigits: digits }).format(value), RATE_UNITS[unit] ?? ""];
}

/** A day and month the way the product writes dates: "12 Oct". */
export function formatDay(date: Date, locale: string): string {
  const tag = locale === "en" ? "en-GB" : locale;
  return new Intl.DateTimeFormat(tag, { day: "numeric", month: "short" }).format(date);
}

/**
 * A live part of a whole, for a Metric: the part with a fixed number of
 * decimals, so it keeps its width as it changes ("7.0 of 47 GB", "61 of
 * 160 GB"), and the whole with its unit. One decimal while the whole is
 * under 100 in its unit, none above.
 */
export function formatLiveBytesOf(part: number | bigint, whole: number | bigint, locale: string): [string, string] {
  const unit = unitFor(Number(whole));
  const digits = Number(whole) / 1024 ** unit < 100 ? 1 : 0;
  return [
    formatFixed(Number(part) / 1024 ** unit, digits, locale),
    `${amountIn(Number(whole), unit, locale)} ${BYTE_UNITS[unit]}`,
  ];
}
