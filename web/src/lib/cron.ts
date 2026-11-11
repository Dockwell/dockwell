// SPDX-License-Identifier: AGPL-3.0-only

/**
 * Reads a five-field cron schedule and says when it next runs in a time zone, as
 * internal/cron does on the control plane and the agent (ADR 0053): lists, ranges,
 * steps and names, the @ words but @reboot, both day fields either-or as in Vixie
 * cron, and the same rules for a change of daylight saving. The panel shows the next
 * runs from it while a schedule is being written, before anything is planned, so it
 * has to agree with what runs.
 */

/** A parsed schedule: each field as the set of values it takes. */
export interface Schedule {
  minute: Set<number>;
  hour: Set<number>;
  dom: Set<number>;
  month: Set<number>;
  dow: Set<number>;
  domStar: boolean;
  dowStar: boolean;
  hourStar: boolean;
}

interface Field {
  min: number;
  max: number;
  names?: string[];
}

const FIELDS: Field[] = [
  { min: 0, max: 59 },
  { min: 0, max: 23 },
  { min: 1, max: 31 },
  { min: 1, max: 12, names: ["JAN", "FEB", "MAR", "APR", "MAY", "JUN", "JUL", "AUG", "SEP", "OCT", "NOV", "DEC"] },
  { min: 0, max: 7, names: ["SUN", "MON", "TUE", "WED", "THU", "FRI", "SAT"] },
];

/** The @ schedules, as five fields. */
export const CRON_WORDS: Readonly<Record<string, string>> = {
  "@yearly": "0 0 1 1 *",
  "@annually": "0 0 1 1 *",
  "@monthly": "0 0 1 * *",
  "@weekly": "0 0 * * 0",
  "@daily": "0 0 * * *",
  "@midnight": "0 0 * * *",
  "@hourly": "0 * * * *",
};

/** Reads a schedule, such as "30 2 * * *" or "@daily", or undefined for one the control plane would refuse. */
export function parseCron(expr: string): Schedule | undefined {
  let s = expr.trim();
  if (s.startsWith("@")) {
    const w = CRON_WORDS[s.toLowerCase()];
    if (!w) return undefined;
    s = w;
  }
  const parts = s.split(/\s+/);
  if (parts.length !== 5) return undefined;
  const sets: Set<number>[] = [];
  for (const [i, part] of parts.entries()) {
    const set = parseField(FIELDS[i] as Field, i === 4, part);
    if (!set) return undefined;
    sets.push(set);
  }
  const [minute, hour, dom, month, dow] = sets as [Set<number>, Set<number>, Set<number>, Set<number>, Set<number>];
  // Sunday is 0 and 7.
  if (dow.delete(7)) dow.add(0);
  return {
    minute,
    hour,
    dom,
    month,
    dow,
    domStar: (parts[2] ?? "").startsWith("*"),
    dowStar: (parts[4] ?? "").startsWith("*"),
    hourStar: (parts[1] ?? "").startsWith("*"),
  };
}

function parseField(f: Field, weekday: boolean, text: string): Set<number> | undefined {
  const set = new Set<number>();
  for (const item of text.split(",")) {
    if (item === "") return undefined;
    const [range = "", stepText] = item.split("/", 2);
    if (item.split("/").length > 2) return undefined;
    let step = 1;
    if (stepText !== undefined) {
      if (!/^\d+$/.test(stepText) || Number(stepText) < 1) return undefined;
      step = Number(stepText);
    }
    let lo = f.min;
    // * means Sunday to Saturday once.
    let hi = weekday ? 6 : f.max;
    if (range !== "*") {
      const [a = "", b] = range.split("-", 2);
      if (range.split("-").length > 2) return undefined;
      const from = value(f, a);
      if (from === undefined) return undefined;
      lo = from;
      hi = from;
      if (b !== undefined) {
        const to = value(f, b);
        if (to === undefined || to < from) return undefined;
        hi = to;
      } else if (stepText !== undefined) {
        // "5/15" is from 5 to the end, every 15.
        hi = f.max;
      }
    }
    for (let v = lo; v <= hi; v += step) set.add(v);
  }
  return set;
}

function value(f: Field, text: string): number | undefined {
  const named = f.names?.findIndex((n) => n.toLowerCase() === text.toLowerCase()) ?? -1;
  if (named >= 0) return f.min + named;
  if (!/^\d+$/.test(text)) return undefined;
  const n = Number(text);
  return n < f.min || n > f.max ? undefined : n;
}

/** A wall-clock minute, as a time zone reads an instant. */
interface Wall {
  year: number;
  month: number;
  day: number;
  hour: number;
  minute: number;
  weekday: number;
}

const formatters = new Map<string, Intl.DateTimeFormat>();
const WEEKDAYS: Record<string, number> = { Sun: 0, Mon: 1, Tue: 2, Wed: 3, Thu: 4, Fri: 5, Sat: 6 };

/** How zone reads the instant at ms. */
function wallOf(ms: number, zone: string): Wall {
  let f = formatters.get(zone);
  if (!f) {
    f = new Intl.DateTimeFormat("en-US", {
      timeZone: zone,
      hourCycle: "h23",
      year: "numeric",
      month: "numeric",
      day: "numeric",
      hour: "numeric",
      minute: "numeric",
      weekday: "short",
    });
    formatters.set(zone, f);
  }
  const parts: Partial<Record<Intl.DateTimeFormatPartTypes, string>> = {};
  for (const p of f.formatToParts(new Date(ms))) parts[p.type] = p.value;
  return {
    year: Number(parts.year),
    month: Number(parts.month),
    day: Number(parts.day),
    hour: Number(parts.hour) % 24,
    minute: Number(parts.minute),
    weekday: WEEKDAYS[parts.weekday ?? ""] ?? 0,
  };
}

/** A wall clock as if it were UTC, which makes its date arithmetic plain. */
const asUTC = (w: Pick<Wall, "year" | "month" | "day" | "hour" | "minute">) =>
  Date.UTC(w.year, w.month - 1, w.day, w.hour, w.minute);

/** The wall clock of a UTC-as-wall time, weekday included. */
function plainWall(ms: number): Wall {
  const d = new Date(ms);
  return {
    year: d.getUTCFullYear(),
    month: d.getUTCMonth() + 1,
    day: d.getUTCDate(),
    hour: d.getUTCHours(),
    minute: d.getUTCMinutes(),
    weekday: d.getUTCDay(),
  };
}

const sameWall = (a: Wall, b: Wall) =>
  a.year === b.year && a.month === b.month && a.day === b.day && a.hour === b.hour && a.minute === b.minute;

const MINUTE = 60_000;
const HOUR = 60 * MINUTE;

function dayMatches(s: Schedule, w: Wall): boolean {
  const dom = s.dom.has(w.day);
  const dow = s.dow.has(w.weekday);
  return s.domStar || s.dowStar ? dom && dow : dom || dow;
}

/** The zone's offsets from UTC around a wall time: a day before and after, and at it. */
function offsetsNear(guess: number, zone: string): number[] {
  const out = new Set<number>();
  for (const at of [guess - 24 * HOUR, guess, guess + 24 * HOUR]) out.add(asUTC(wallOf(at, zone)) - at);
  return [...out];
}

/** The instants that read as wall in zone, earliest first: none in a gap, two in an hour that comes twice. */
function instantsOf(wall: Wall, zone: string): number[] {
  const guess = asUTC(wall);
  return offsetsNear(guess, zone)
    .map((offset) => guess - offset)
    .filter((at) => sameWall(wallOf(at, zone), wall))
    .sort((a, b) => a - b);
}

/**
 * When a wall-clock minute happens in zone: the first time, where a change of daylight
 * saving makes it come twice, and the minute the clocks jumped to, where one skipped it.
 */
function instant(wall: Wall, zone: string): number {
  const [first] = instantsOf(wall, zone);
  if (first !== undefined) return first;
  // Skipped: one minute after the last minute before it that exists.
  const guess = asUTC(wall);
  for (let m = 1; m <= 24 * 60; m++) {
    const [at] = instantsOf(plainWall(guess - m * MINUTE), zone);
    if (at !== undefined) return at + MINUTE;
  }
  return guess;
}

/** The first run after the instant after, in ms, or undefined where none comes in eight years. */
export function nextRun(s: Schedule, after: number, zone: string): number | undefined {
  return s.hourStar ? nextReal(s, after, zone) : nextWall(s, after, zone);
}

/** Walks wall-clock minutes from the one after, as cron does, skipping what can't match. */
function nextWall(s: Schedule, after: number, zone: string): number | undefined {
  const local = wallOf(after, zone);
  let wall = asUTC(local) + MINUTE;
  const end = wall + 8 * 366 * 24 * HOUR;
  while (wall < end) {
    const w = plainWall(wall);
    if (!s.month.has(w.month)) {
      wall = Date.UTC(w.year, w.month, 1);
      continue;
    }
    if (!dayMatches(s, w)) {
      wall = Date.UTC(w.year, w.month - 1, w.day + 1);
      continue;
    }
    if (!s.hour.has(w.hour)) {
      wall = Date.UTC(w.year, w.month - 1, w.day, w.hour + 1);
      continue;
    }
    if (!s.minute.has(w.minute)) {
      wall += MINUTE;
      continue;
    }
    const at = instant(w, zone);
    if (at > after) return at;
    wall += MINUTE;
  }
  return undefined;
}

/** Walks real minutes, as they read in zone, skipping what can't match. */
function nextReal(s: Schedule, after: number, zone: string): number | undefined {
  let at = Math.floor(after / MINUTE) * MINUTE + MINUTE;
  const end = at + 8 * 366 * 24 * HOUR;
  while (at < end) {
    const w = wallOf(at, zone);
    let next: number;
    if (!s.month.has(w.month)) {
      next = instant(
        {
          ...w,
          month: w.month === 12 ? 1 : w.month + 1,
          year: w.month === 12 ? w.year + 1 : w.year,
          day: 1,
          hour: 0,
          minute: 0,
        },
        zone,
      );
    } else if (!dayMatches(s, w)) {
      next = instant(plainWall(Date.UTC(w.year, w.month - 1, w.day + 1)), zone);
    } else if (!s.hour.has(w.hour)) {
      next = at + (60 - w.minute) * MINUTE;
    } else if (!s.minute.has(w.minute)) {
      next = at + MINUTE;
    } else {
      return at;
    }
    // A midnight a change of daylight saving skips reads as a time before it.
    at = next > at ? next : at + MINUTE;
  }
  return undefined;
}

/** The next count runs after the instant after, in ms. */
export function nextRuns(s: Schedule, after: number, zone: string, count: number): number[] {
  const out: number[] = [];
  let from = after;
  for (let i = 0; i < count; i++) {
    const at = nextRun(s, from, zone);
    if (at === undefined) break;
    out.push(at);
    from = at;
  }
  return out;
}
