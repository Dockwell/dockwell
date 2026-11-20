// SPDX-License-Identifier: AGPL-3.0-only

import { useSyncExternalStore } from "react";

/**
 * The signed-in person's chosen IANA time zone for how the panel shows times, or
 * empty to follow this browser's own zone (#834). This module is the one place
 * that reads it; every screen formats dates and clocks through here.
 */
let displayTimeZone = "";
const timeZoneListeners = new Set<() => void>();

/** The signed-in person's chosen time zone, or "" for this browser's. */
export function getDisplayTimeZone(): string {
  return displayTimeZone;
}

/** Updates the active display time zone and notifies subscribers to re-render. */
export function setDisplayTimeZone(zone: string): void {
  if (displayTimeZone === zone) return;
  displayTimeZone = zone;
  for (const listener of timeZoneListeners) listener();
}

/**
 * Updates the active display time zone without notifying subscribers, for syncing
 * during render before child screens format their first frame.
 */
export function syncDisplayTimeZone(zone: string): void {
  displayTimeZone = zone;
}

function subscribeDisplayTimeZone(listener: () => void): () => void {
  timeZoneListeners.add(listener);
  return () => {
    timeZoneListeners.delete(listener);
  };
}

/** Subscribes to changes in the active display time zone. */
export function useDisplayTimeZone(): string {
  return useSyncExternalStore(subscribeDisplayTimeZone, getDisplayTimeZone, getDisplayTimeZone);
}

function effectiveZone(timeZone?: string): string | undefined {
  return timeZone || displayTimeZone || undefined;
}

/** The browser's own IANA time zone name (e.g. "America/Los_Angeles"), or "UTC". */
export function browserTimeZone(): string {
  return Intl.DateTimeFormat().resolvedOptions().timeZone || "UTC";
}

/** The IANA time zone times currently show in: the person's own, else this browser's. */
export function activeTimeZone(): string {
  return displayTimeZone || browserTimeZone();
}

interface CalendarParts {
  year: number;
  month: number;
  day: number;
}

const calendarFormats = new Map<string, Intl.DateTimeFormat>();

function calendarParts(date: Date, timeZone?: string): CalendarParts {
  const tz = effectiveZone(timeZone);
  if (!tz) {
    return { year: date.getFullYear(), month: date.getMonth() + 1, day: date.getDate() };
  }
  let fmt = calendarFormats.get(tz);
  if (!fmt) {
    fmt = new Intl.DateTimeFormat("en-CA", {
      timeZone: tz,
      year: "numeric",
      month: "2-digit",
      day: "2-digit",
    });
    calendarFormats.set(tz, fmt);
  }
  let year = 0;
  let month = 0;
  let day = 0;
  for (const part of fmt.formatToParts(date)) {
    if (part.type === "year") year = Number(part.value);
    else if (part.type === "month") month = Number(part.value);
    else if (part.type === "day") day = Number(part.value);
  }
  return { year, month, day };
}

/** Calendar day key "YYYY-MM-DD" in the active display time zone (or explicit override). */
export function dayKey(date: Date, timeZone?: string): string {
  const { year, month, day } = calendarParts(date, timeZone);
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${year}-${pad(month)}-${pad(day)}`;
}

/** Whether two instants fall on the same calendar day in the active display time zone. */
export function sameDay(a: Date, b: Date, timeZone?: string): boolean {
  const pa = calendarParts(a, timeZone);
  const pb = calendarParts(b, timeZone);
  return pa.year === pb.year && pa.month === pb.month && pa.day === pb.day;
}

/** Whole calendar days from b to a (a - b) in the active display time zone. */
export function dayDiff(a: Date, b: Date, timeZone?: string): number {
  const pa = calendarParts(a, timeZone);
  const pb = calendarParts(b, timeZone);
  return Math.round((Date.UTC(pa.year, pa.month - 1, pa.day) - Date.UTC(pb.year, pb.month - 1, pb.day)) / 86_400_000);
}

const offsetFormats = new Map<string, Intl.DateTimeFormat>();

/**
 * UTC offset in minutes for zone at date (positive east of UTC, such as +540 for
 * Asia/Tokyo and -480 for America/Los_Angeles in standard time).
 */
export function timeZoneOffsetMinutes(zone?: string, date: Date = new Date()): number {
  const tz = effectiveZone(zone);
  if (!tz) return -date.getTimezoneOffset();
  let fmt = offsetFormats.get(tz);
  if (!fmt) {
    fmt = new Intl.DateTimeFormat("en-GB", {
      timeZone: tz,
      year: "numeric",
      month: "2-digit",
      day: "2-digit",
      hour: "2-digit",
      minute: "2-digit",
      second: "2-digit",
      hourCycle: "h23",
    });
    offsetFormats.set(tz, fmt);
  }
  let year = 0;
  let month = 0;
  let day = 0;
  let hour = 0;
  let minute = 0;
  let second = 0;
  for (const part of fmt.formatToParts(date)) {
    if (part.type === "year") year = Number(part.value);
    else if (part.type === "month") month = Number(part.value);
    else if (part.type === "day") day = Number(part.value);
    else if (part.type === "hour") hour = Number(part.value);
    else if (part.type === "minute") minute = Number(part.value);
    else if (part.type === "second") second = Number(part.value);
  }
  const asUtc = Date.UTC(year, month - 1, day, hour === 24 ? 0 : hour, minute, second);
  const wholeSeconds = Math.floor(date.getTime() / 1000) * 1000;
  return Math.round((asUtc - wholeSeconds) / 60_000);
}

/** UTC offset formatted as "UTC+09:00", "UTC-08:00", or "UTC". */
export function formatTimeZoneOffset(zone?: string, date: Date = new Date()): string {
  const minutes = timeZoneOffsetMinutes(zone, date);
  if (minutes === 0) return "UTC";
  const sign = minutes < 0 ? "-" : "+";
  const abs = Math.abs(minutes);
  const h = String(Math.floor(abs / 60)).padStart(2, "0");
  const m = String(abs % 60).padStart(2, "0");
  return `UTC${sign}${h}:${m}`;
}

/**
 * The locale to format dates in: English follows the design system's
 * "12 Oct" style, day before month, rather than US order; every other
 * locale keeps its own.
 */
export function dateLocale(locale: string): string {
  return locale === "en" ? "en-GB" : locale;
}

/**
 * A date and time the way the product writes them: day before month, short
 * month names ("3 Nov 2026, 21:40"), and the locale's own order elsewhere.
 */
export function formatDateTime(date: Date, locale: string, timeZone?: string): string {
  const tz = effectiveZone(timeZone);
  return new Intl.DateTimeFormat(dateLocale(locale), {
    day: "numeric",
    month: "short",
    year: "numeric",
    hour: "2-digit",
    minute: "2-digit",
    ...(tz ? { timeZone: tz } : {}),
  }).format(date);
}

/** A moment to the second, with its time zone named: "7 Nov 2026, 14:03:07 CET". */
export function formatMoment(date: Date, locale: string, timeZone?: string): string {
  const tz = effectiveZone(timeZone);
  return new Intl.DateTimeFormat(dateLocale(locale), {
    day: "numeric",
    month: "short",
    year: "numeric",
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
    timeZoneName: "short",
    ...(tz ? { timeZone: tz } : {}),
  }).format(date);
}

/** The day as a file name has it, in the active time zone, as the screen shows times: "2026-11-07". */
export function fileDay(date: Date, timeZone?: string): string {
  return dayKey(date, timeZone);
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

/** A day and month the way the product writes dates: "12 Oct", with the year when it isn't this one. */
export function formatDay(date: Date, locale: string, now: number = Date.now(), timeZone?: string): string {
  const tz = effectiveZone(timeZone);
  const sameYear = calendarParts(date, timeZone).year === calendarParts(new Date(now), timeZone).year;
  return new Intl.DateTimeFormat(dateLocale(locale), {
    day: "numeric",
    month: "short",
    ...(sameYear ? {} : { year: "numeric" }),
    ...(tz ? { timeZone: tz } : {}),
  }).format(date);
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

/** A weekday and its date, the way a day's heading reads: "Tuesday 3 Nov". */
export function formatWeekday(date: Date, locale: string, timeZone?: string): string {
  const tz = effectiveZone(timeZone);
  return new Intl.DateTimeFormat(dateLocale(locale), {
    weekday: "long",
    day: "numeric",
    month: "short",
    ...(tz ? { timeZone: tz } : {}),
  })
    .format(date)
    .replace(",", "");
}

/** A time of day on the 24-hour clock where the locale uses it: "09:41". */
export function formatClock(date: Date, locale: string, timeZone?: string): string {
  const tz = effectiveZone(timeZone);
  return new Intl.DateTimeFormat(dateLocale(locale), {
    hour: "2-digit",
    minute: "2-digit",
    ...(tz ? { timeZone: tz } : {}),
  }).format(date);
}

/** A time of day to the second on the 24-hour clock: "09:41:05". */
export function formatClockSeconds(date: Date, locale: string, timeZone?: string): string {
  const tz = effectiveZone(timeZone);
  return new Intl.DateTimeFormat(dateLocale(locale), {
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
    hourCycle: "h23",
    ...(tz ? { timeZone: tz } : {}),
  }).format(date);
}
