// SPDX-License-Identifier: AGPL-3.0-only

import { LogGapReason, LogKept, LogLevel, type LogSource, LogSourceState } from "@dockwell/api";
import type { LogLine } from "@dockwell/ui";
import { plainText } from "@dockwell/ui";
import type { I18n } from "@lingui/core";
import { msg, plural } from "@lingui/core/macro";

import { dateLocale } from "../../lib/format";
import type { Entry } from "./store";

/** A source, with what the log calls it where those around it decide that. */
export type LabelledSource = LogSource & { label?: string | undefined };

/** A replica as a person reads it: its server, and which one it is there when there are more. */
export function replicaLabel(s: Pick<LogSource, "serverName" | "replica"> & { label?: string | undefined }): string {
  return s.label ?? (s.replica > 1 ? `${s.serverName}-${s.replica}` : s.serverName);
}

/**
 * The name a person knows a container by: web-3, of Docker's default.production.web-3,
 * or of env_01kat-web-3, as one made before containers took their environment's name
 * has it (ADR 0035). A project's and an environment's names have no dots.
 */
export function instanceName(s: Pick<LogSource, "containerName">): string {
  const dot = s.containerName.lastIndexOf(".");
  return dot >= 0 ? s.containerName.slice(dot + 1) : s.containerName.replace(/^[^-]*-/, "");
}

/**
 * What the log calls each source. A service has one container on a server at rest, so
 * its server says which, however many deploys it has had. Where more than one has been
 * read here, as both run through a rolling deploy, each goes by its own name, so the
 * log can say which wrote what. What was only kept, and isn't being read, doesn't count.
 */
export function withLabels(
  all: ReadonlyMap<string, LogSource>,
  read: ReadonlyMap<string, LogSource>,
): Map<string, LabelledSource> {
  const reading = [...all.values()].filter((s) => s.state !== LogSourceState.KEPT || read.has(s.id));
  const servers = new Set(reading.map((s) => s.serverId));
  const names = new Map<string, Set<string>>();
  for (const s of reading) names.set(s.serverId, (names.get(s.serverId) ?? new Set()).add(instanceName(s)));
  const out = new Map<string, LabelledSource>();
  for (const [id, s] of all) {
    const several = reading.includes(s) && (names.get(s.serverId)?.size ?? 0) > 1;
    const name = instanceName(s);
    out.set(id, several ? { ...s, label: servers.size > 1 ? `${s.serverName}/${name}` : name } : s);
  }
  return out;
}

function pad(n: number, width = 2): string {
  return String(n).padStart(width, "0");
}

/** Wall-clock time with milliseconds, in the viewer's zone, without Intl, as this runs for each line drawn. */
export function clock(ms: number): string {
  const d = new Date(ms);
  return `${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}.${pad(d.getMilliseconds(), 3)}`;
}

/** Wall-clock time to the second, for a marker, whose times aren't exact to less. */
export function clockSeconds(ms: number): string {
  const d = new Date(ms);
  return `${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`;
}

/** Wall-clock time to the minute, for what a person reads as words. */
export function clockMinutes(ms: number): string {
  const d = new Date(ms);
  return `${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

/** A time as the screen has it, with its offset, so that it says when wherever it is read. */
export function localIso(ms: number): string {
  const d = new Date(ms);
  const offset = -d.getTimezoneOffset();
  const sign = offset < 0 ? "-" : "+";
  const hours = pad(Math.floor(Math.abs(offset) / 60));
  const minutes = pad(Math.abs(offset) % 60);
  const day = `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`;
  return `${day}T${clock(ms)}${sign}${hours}:${minutes}`;
}

/** Which zone the times on screen are in, short, as "UTC+01:00". */
export function zoneOffset(ms = Date.now()): string {
  const offset = -new Date(ms).getTimezoneOffset();
  if (offset === 0) return "UTC";
  const sign = offset < 0 ? "-" : "+";
  return `UTC${sign}${pad(Math.floor(Math.abs(offset) / 60))}:${pad(Math.abs(offset) % 60)}`;
}

/** Which zone by name, for a tooltip. */
export function zoneName(): string {
  return Intl.DateTimeFormat().resolvedOptions().timeZone;
}

const dayFormats = new Map<string, Intl.DateTimeFormat>();
const dayLabels = new Map<string, Map<number, string>>();

/** A line's day, in words. Making a format is slow, and this runs for each line drawn, so a day is worked out once. */
export function dayOf(ms: number, locale: string): string {
  const d = new Date(ms);
  const key = d.getFullYear() * 10_000 + d.getMonth() * 100 + d.getDate();
  let labels = dayLabels.get(locale);
  if (!labels) {
    labels = new Map();
    dayLabels.set(locale, labels);
  }
  let label = labels.get(key);
  if (label === undefined) {
    let format = dayFormats.get(locale);
    if (!format) {
      format = new Intl.DateTimeFormat(dateLocale(locale), {
        weekday: "short",
        day: "numeric",
        month: "short",
        year: "numeric",
      });
      dayFormats.set(locale, format);
    }
    label = format.format(ms);
    labels.set(key, label);
  }
  return label;
}

const dayNotes = new Map<string, Map<number, string>>();

/** A line's day with the zone its times are in, one string for each day, as many lines hold it. */
export function dayNoteOf(ms: number, locale: string): string {
  const d = new Date(ms);
  const key = d.getFullYear() * 10_000 + d.getMonth() * 100 + d.getDate();
  let notes = dayNotes.get(locale);
  if (!notes) {
    notes = new Map();
    dayNotes.set(locale, notes);
  }
  let note = notes.get(key);
  if (note === undefined) {
    note = `${dayOf(ms, locale)} · ${zoneOffset(ms)}`;
    notes.set(key, note);
  }
  return note;
}

/** A number as a reader acts on it: to two significant figures, and only "about" if that rounded it. */
export function roughly(n: number): { value: number; rounded: boolean } {
  if (n < 100) return { value: n, rounded: false };
  const unit = 10 ** (Math.floor(Math.log10(n)) - 1);
  const value = Math.round(n / unit) * unit;
  return { value, rounded: value !== n };
}

/** What a marker in the stream says: what is missing, and why. */
export function gapWords(e: Entry, source: string, i18n: I18n): string {
  const gap = e.gap;
  if (!gap) return "";
  const lines = Number(gap.lines).toLocaleString(i18n.locale);
  switch (gap.reason) {
    case LogGapReason.TOO_FAST:
      return plural(Number(gap.lines), {
        one: `${lines} line from ${source} not shown: it wrote faster than a live view can carry. Read it with dockwell logs read.`,
        other: `${lines} lines from ${source} not shown: it wrote faster than a live view can carry. Read them with dockwell logs read.`,
      });
    case LogGapReason.SHARED:
      return plural(Number(gap.lines), {
        one: `${lines} line from ${source} not shown: this server shares what it carries live between everyone reading it, and this was past this view's share. Read it with dockwell logs read.`,
        other: `${lines} lines from ${source} not shown: this server shares what it carries live between everyone reading it, and these were past this view's share. Read them with dockwell logs read.`,
      });
    case LogGapReason.READER_BEHIND:
      return plural(Number(gap.lines), {
        one: `${lines} line from ${source} not shown: this page or the network to it fell behind.`,
        other: `${lines} lines from ${source} not shown: this page or the network to it fell behind.`,
      });
    case LogGapReason.MATCH_LIMIT:
      return plural(Number(gap.lines), {
        one: `${lines} older match from ${source} not shown: a read to the end holds the newest 2 MB of each container.`,
        other: `${lines} older matches from ${source} not shown: a read to the end holds the newest 2 MB of each container.`,
      });
    case LogGapReason.GONE: {
      const from = gap.startTime ? clockSeconds(Number(gap.startTime.seconds) * 1000) : "";
      const to = gap.endTime ? clockSeconds(Number(gap.endTime.seconds) * 1000) : "";
      return to
        ? i18n._(msg`Lines from ${source} between ${from} and ${to} are gone: the server no longer has them.`)
        : i18n._(msg`Lines from ${source} after ${from} are gone: the server no longer has this container's log.`);
    }
    default:
      return i18n._(msg`Some lines from ${source} are missing here.`);
  }
}

const NAMES_ITS_LEVEL = /\b(?:error|err|fatal|crit|critical|panic|warn|warning)\b/i;

/**
 * A line as text, for copying and downloading: no codes, what a reader saw, and
 * when, as the screen has it, with its offset. A marker has no source where a line
 * has one, so a line that prints the words of a marker can't pass for one.
 */
export function textOf(e: Entry, sources: ReadonlyMap<string, LogSource>): string {
  const s = sources.get(e.source);
  const source = s ? replicaLabel(s) : e.source;
  const time = localIso(e.ms);
  if (e.gap) return `${time} -- ${Number(e.gap.lines)} lines from ${source} not shown --`;
  if (e.left !== undefined) return `${time} -- ${e.left} lines left this view while it was paused --`;
  if (e.away) return `${time} -- not read while this view was away, to ${localIso(e.away.to)} --`;
  if (e.started) return `${time} -- new container ${(s && instanceName(s)) || e.source} of ${source} --`;
  const plain = plainText(e.text);
  const named = NAMES_ITS_LEVEL.test(plain.slice(0, 120));
  const level = named ? "" : e.level === LogLevel.ERROR ? "ERROR " : e.level === LogLevel.WARN ? "WARN " : "";
  return `${time} ${source} ${level}${plain}`;
}

/** A line as a selection copies it: what the screen shows of it. */
export function selectionText(line: LogLine): string {
  return `${line.time} ${line.source} ${plainText(line.text)}`;
}

/** Which deploys there are logs of. */
export interface Deploy {
  key: string;
  revision: bigint;
  change: string;
  sources: LogSource[];
  /** Docker has it: it runs now, or stopped, as opposed to one the server kept. */
  current: boolean;
  failed: boolean;
}

/** Groups containers by the deploy that made them, newest first. */
export function deploysOf(sources: readonly LogSource[]): Deploy[] {
  const out = new Map<string, Deploy>();
  for (const s of sources) {
    const key = s.changeId || (s.revision > 0n ? `r${s.revision}` : s.id);
    let d = out.get(key);
    if (!d) {
      d = { key, revision: s.revision, change: s.changeId, sources: [], current: false, failed: false };
      out.set(key, d);
    }
    d.sources.push(s);
    d.current = d.current || s.state !== LogSourceState.KEPT;
    d.failed = d.failed || s.kept === LogKept.FAILED;
  }
  return [...out.values()];
}

/** How a deploy is named: its revision, or a container's ID when nothing says. */
export function deployRevision(d: Deploy): string {
  return d.revision > 0n ? `r${d.revision}` : (d.sources[0]?.id ?? "");
}
