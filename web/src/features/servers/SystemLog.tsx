// SPDX-License-Identifier: AGPL-3.0-only

import { timestampMs } from "@bufbuild/protobuf/wkt";
import { type ConnectError, createClient } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import { LogLevel, ServerService, type SystemLogLine, SystemLogSource } from "@dockwell/api";
import { Button, Icon, type LogLine, LogStream, SegmentedControl } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link } from "@tanstack/react-router";
import { useCallback, useEffect, useMemo, useState } from "react";

import { sentence } from "../../lib/errors";
import { clock, dayNoteOf, dayOf } from "../logs/words";

/** How often a unit's journal is read again while it's open. */
export const SYSTEM_LOG_EVERY_MS = 5_000;

/** How many lines a page reads. */
const PAGE = 50;

type Level = "all" | "warn" | "error";

const LEVEL: Record<Level, LogLevel> = {
  all: LogLevel.UNSPECIFIED,
  warn: LogLevel.WARN,
  error: LogLevel.ERROR,
};

interface Read {
  /** Oldest first, as the log shows them. */
  lines: SystemLogLine[];
  more: boolean;
  source: SystemLogSource;
}

/**
 * A system service's journal (ADR 0082): its newest lines, read again every 5 s
 * while it's open, older ones a page at a time, narrowed to a level. Reading it
 * goes on record, once an hour for a reader and a server.
 */
export function SystemLog({ serverId, server, unit }: { serverId: string; server: string; unit: string }) {
  const { t, i18n } = useLingui();
  const transport = useTransport();
  const client = useMemo(() => createClient(ServerService, transport), [transport]);
  const [level, setLevel] = useState<Level>("all");
  const [read, setRead] = useState<Read>();
  const [error, setError] = useState<ConnectError>();
  const [older, setOlder] = useState(false);
  const [follow, setFollow] = useState(true);

  // The newest page, again and again: lines new since are added, and what was paged back stays.
  useEffect(() => {
    let stopped = false;
    let timer: ReturnType<typeof setTimeout> | undefined;
    setRead(undefined);
    setError(undefined);
    const again = async () => {
      try {
        const res = await client.readSystemLog({ serverId, unit, level: LEVEL[level], limit: PAGE });
        if (stopped) return;
        const newest = [...res.lines].reverse();
        setRead((was) => {
          if (!was) return { lines: newest, more: res.more, source: res.source };
          const known = new Set(was.lines.map((l) => l.cursor));
          const added = newest.filter((l) => !known.has(l.cursor));
          return added.length === 0 ? was : { ...was, lines: [...was.lines, ...added], source: res.source };
        });
        setError(undefined);
      } catch (err) {
        if (!stopped) setError(err as ConnectError);
      }
      if (!stopped) timer = setTimeout(again, SYSTEM_LOG_EVERY_MS);
    };
    void again();
    return () => {
      stopped = true;
      clearTimeout(timer);
    };
  }, [client, serverId, unit, level]);

  const readOlder = useCallback(async () => {
    const first = read?.lines[0];
    if (!first) return;
    setOlder(true);
    try {
      const res = await client.readSystemLog({
        serverId,
        unit,
        level: LEVEL[level],
        before: first.cursor,
        limit: PAGE,
      });
      setRead((was) => (was ? { ...was, lines: [...[...res.lines].reverse(), ...was.lines], more: res.more } : was));
    } catch (err) {
      setError(err as ConnectError);
    } finally {
      setOlder(false);
    }
  }, [client, serverId, unit, level, read]);

  const lines: LogLine[] = useMemo(
    () =>
      (read?.lines ?? []).map((l) => {
        const ms = l.time ? timestampMs(l.time) : 0;
        const time = ms ? clock(ms) : "";
        return {
          id: l.cursor,
          time,
          timeTitle: ms ? `${dayOf(ms, i18n.locale)}, ${time}` : "",
          // A unit's name without .service, which the column has room for; the whole in its title.
          source: l.unit.replace(/\.service$/, ""),
          sourceTitle: l.pid ? `${l.unit}[${l.pid}]` : l.unit,
          level: l.level === LogLevel.ERROR ? "error" : l.level === LogLevel.WARN ? "warn" : "info",
          text: l.message,
          at: ms || undefined,
          day: ms ? dayNoteOf(ms, i18n.locale) : undefined,
        };
      }),
    [read, i18n.locale],
  );

  const levels = [
    { value: "error" as const, label: t({ message: "Errors", context: "system log level" }) },
    { value: "warn" as const, label: t({ message: "Warnings and up", context: "system log level" }) },
    { value: "all" as const, label: t({ message: "Everything", context: "system log level" }) },
  ];

  const empty = !read ? (
    error ? (
      <p className="procs-note">{sentence(error)}</p>
    ) : (
      <p className="procs-note">{t`Reading…`}</p>
    )
  ) : read.source === SystemLogSource.NONE ? (
    <p className="procs-note">{t`${server} keeps no system log Dockwell reads.`}</p>
  ) : level === "all" ? (
    <p className="procs-note">{t`Nothing from ${unit} in the system log.`}</p>
  ) : (
    <p className="procs-note">{t`Nothing at this level from ${unit}.`}</p>
  );

  return (
    <section className="sys-log" aria-label={t`System log of ${unit}`}>
      {/* Above the log, not in its toolbar, so a phone's sheet keeps room for both. */}
      <div className="sys-log-bar">
        <SegmentedControl options={levels} value={level} onChange={setLevel} label={t`Level`} size="sm" />
      </div>
      <LogStream
        lines={lines}
        // An empty log is a line, not a box.
        height={lines.length === 0 ? 120 : 300}
        live={read !== undefined && error === undefined}
        follow={follow}
        onFollowChange={setFollow}
        empty={empty}
        top={
          read?.more ? (
            <Button size="sm" variant="ghost" loading={older} onClick={() => void readOlder()}>
              <Trans>Older lines</Trans>
            </Button>
          ) : undefined
        }
      />
      {read && error ? <p className="procs-note">{sentence(error)}</p> : null}
      <Link className="syslog-open" to="/servers/$serverId/log" params={{ serverId }} search={{ unit }}>
        <Trans>Open in the system log</Trans>
        <Icon name="arrow-right" size={14} />
      </Link>
    </section>
  );
}
