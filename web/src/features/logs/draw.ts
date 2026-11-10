// SPDX-License-Identifier: AGPL-3.0-only

import { LogLevel, type LogSource } from "@dockwell/api";
import type { LogLine } from "@dockwell/ui";
import type { I18n } from "@lingui/core";
import { msg, plural } from "@lingui/core/macro";
import { formatBytes } from "../../lib/format";

import type { Entry } from "./store";
import { clock, clockSeconds, dayNoteOf, dayOf, gapWords, instanceName, replicaLabel } from "./words";

/** How an entry shows: a line of what a container printed, or a note about a place in the log. */
export function drawEntry(e: Entry, sources: ReadonlyMap<string, LogSource>, i18n: I18n): LogLine {
  const s = sources.get(e.source);
  const source = s ? replicaLabel(s) : e.source;
  // With the zone, as a phone's log head has no room for it, and a day says when.
  const day = dayNoteOf(e.ms, i18n.locale);
  if (e.gap) return { id: e.id, time: "", source, text: gapWords(e, source, i18n), notice: "warning", day };
  if (e.away) {
    const from = clockSeconds(e.away.from);
    const to = clockSeconds(e.away.to);
    return {
      id: e.id,
      time: "",
      source: "",
      notice: "info",
      day,
      text: i18n._(
        msg`This tab was away from ${from} to ${to}, so what came then isn't all here. The note above reads it.`,
      ),
    };
  }
  if (e.left !== undefined) {
    const count = e.left.toLocaleString(i18n.locale);
    return {
      id: e.id,
      time: "",
      source,
      notice: "warning",
      day,
      text: plural(e.left, {
        one: `${count} line came while this view was paused, and left it unseen.`,
        other: `${count} lines came while this view was paused, and left it unseen.`,
      }),
    };
  }
  if (e.started) {
    const revision = s && s.revision > 0n ? `r${s.revision}` : e.source;
    const container = (s && instanceName(s)) || e.source;
    return {
      id: e.id,
      time: "",
      source,
      text:
        source === container
          ? i18n._(msg`${source} started ${revision}`)
          : i18n._(msg`${source} started ${revision}, in container ${container}`),
      notice: "info",
      day,
    };
  }
  const level = e.level === LogLevel.ERROR ? "error" : e.level === LogLevel.WARN ? "warn" : "info";
  const time = clock(e.ms);
  const line: LogLine = {
    id: e.id,
    time,
    timeTitle: `${dayOf(e.ms, i18n.locale)}, ${time}`,
    source,
    level,
    text: e.text,
    at: e.ms,
    day,
  };
  if (s) {
    const revision = s.revision > 0n ? `r${s.revision} · ` : "";
    line.sourceTitle = `${source} · ${revision}${e.source}`;
  }
  if (e.cut > 0) line.cut = formatBytes(e.cut, i18n.locale);
  return line;
}
