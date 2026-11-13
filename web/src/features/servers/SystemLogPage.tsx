// SPDX-License-Identifier: AGPL-3.0-only

import { timestampMs } from "@bufbuild/protobuf/wkt";
import {
  Connection,
  LogLevel,
  type Server,
  SystemLogSource,
  type SystemServiceInfo,
  SystemServiceState,
} from "@dockwell/api";
import {
  Button,
  Icon,
  type LogLine,
  LogStream,
  PHONE_QUERY,
  SegmentedControl,
  TextField,
  useMediaQuery,
} from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { useEffect, useMemo, useState } from "react";

import { sentence } from "../../lib/errors";
import { clock, dayNoteOf, dayOf } from "../logs/words";
import { AgentOutdated, isAgentOutdated } from "./AgentOutdated";
import { ServerFrame } from "./ServerFrame";
import { MAX_QUERY, type SystemLogEntry, type SystemLogRange, type SystemLogSearch, useSystemLog } from "./systemLog";
import { useSystemServices } from "./systemServices";
import "../logs/LogsPage.css";
import "./ServerPanels.css";
import "./ProcessesPage.css";
import "./SystemLogPage.css";

/**
 * A server's system log (ADR 0067): its journal, followed live or read back over a
 * time range, narrowed to a system service, a level and a search, each in the URL so
 * a unit's sheet and a shared link open it as they were.
 */
export function SystemLogPage({
  serverId,
  search,
  onSearch,
}: {
  serverId: string;
  search: SystemLogSearch;
  onSearch: (next: SystemLogSearch) => void;
}) {
  return (
    <ServerFrame id={serverId}>
      {(server) => <SystemLog server={server} search={search} onSearch={onSearch} />}
    </ServerFrame>
  );
}

/** How long typing waits before it searches. */
const TYPED_MS = 400;

function SystemLog({
  server,
  search,
  onSearch,
}: {
  server: Server;
  search: SystemLogSearch;
  onSearch: (next: SystemLogSearch) => void;
}) {
  const { t, i18n } = useLingui();
  const phone = useMediaQuery(PHONE_QUERY);
  const connected = server.connection === Connection.CONNECTED;
  const log = useSystemLog(server.id, search, connected);
  const [follow, setFollow] = useState(true);
  const [typed, setTyped] = useState(search.q ?? "");
  // The search waits for typing to settle, then goes in the URL.
  useEffect(() => {
    if ((search.q ?? "") === typed) return;
    const timer = setTimeout(() => {
      const { q: _q, ...rest } = search;
      onSearch(typed.trim() ? { ...rest, q: typed } : rest);
    }, TYPED_MS);
    return () => clearTimeout(timer);
  }, [typed, search, onSearch]);

  const set = (patch: Partial<SystemLogSearch>) => {
    const next = { ...search, ...patch };
    for (const k of Object.keys(next) as (keyof SystemLogSearch)[]) if (!next[k]) delete next[k];
    onSearch(next);
  };

  const lines: LogLine[] = useMemo(() => log.entries.map((e) => drawEntry(e, i18n.locale)), [log.entries, i18n.locale]);
  // drawEntry's notes are said here, where t is.
  const shown: LogLine[] = useMemo(
    () =>
      lines.map((l, i) => {
        const e = log.entries[i];
        if (e?.kind === "dropped") {
          const n = e.count.toLocaleString(i18n.locale);
          return {
            ...l,
            text: plural(e.count, {
              one: `${n} line dropped here, past this follow's share of the server's rate`,
              other: `${n} lines dropped here, past this follow's share of the server's rate`,
            }),
          };
        }
        if (e?.kind === "gap")
          return { ...l, text: t`More came here than a follow carries on with. Pick a time range to read them` };
        return l;
      }),
    [lines, log.entries, i18n.locale, t],
  );

  const levels = [
    { value: "error" as const, label: t({ message: "Errors", context: "system log level" }) },
    { value: "warn" as const, label: t({ message: "Warnings and up", context: "system log level" }) },
    { value: "all" as const, label: t({ message: "Everything", context: "system log level" }) },
  ];
  const ranges: { value: SystemLogRange | "live"; label: string }[] = [
    { value: "live", label: t`Live` },
    { value: "15m", label: t`15 min` },
    { value: "1h", label: t`1 h` },
    { value: "24h", label: t`24 h` },
    { value: "7d", label: t({ message: "7 d", context: "a time range of seven days" }) },
  ];

  const searchedTo = log.searchedTo;
  const searchedWords = searchedTo !== undefined ? `${dayOf(searchedTo, i18n.locale)}, ${clock(searchedTo)}` : "";
  const unitName = search.unit ?? "";

  let empty: React.ReactNode;
  if (!connected) {
    empty = (
      <p className="procs-note">{t`The agent on ${server.name} isn't connected, so its system log can't be read now. This reads again when it reconnects.`}</p>
    );
  } else if (log.error && isAgentOutdated(log.error)) {
    empty = (
      <AgentOutdated
        server={server}
      >{t`It's older than the control plane, so it can't read its system log.`}</AgentOutdated>
    );
  } else if (!log.read) {
    empty = <p className="procs-note">{t`Reading…`}</p>;
  } else if (log.error && log.entries.length === 0) {
    empty = <p className="procs-note">{sentence(log.error)}</p>;
  } else if (log.source === SystemLogSource.NONE) {
    empty = <p className="procs-note">{t`${server.name} keeps no system log Dockwell reads.`}</p>;
  } else if (search.q) {
    empty = <p className="procs-note">{t`No line holds that.`}</p>;
  } else if (unitName) {
    empty = <p className="procs-note">{t`Nothing from ${unitName} here.`}</p>;
  } else {
    empty = <p className="procs-note">{t`Nothing here.`}</p>;
  }

  const top =
    log.more && log.entries.length > 0 ? (
      <div className="syslog-top">
        {searchedTo !== undefined ? <span className="procs-sub">{t`Searched back to ${searchedWords}`}</span> : null}
        <Button size="sm" variant="ghost" loading={log.readingOlder} onClick={() => void log.readOlder()}>
          {searchedTo !== undefined ? <Trans>Search older</Trans> : <Trans>Older lines</Trans>}
        </Button>
      </div>
    ) : undefined;

  return (
    <section className={phone ? "syslog is-phone" : "syslog"} aria-labelledby="syslog-title">
      <h2 className="dw-sr" id="syslog-title">
        {t`System log`}
      </h2>
      <div className="logs-tools syslog-tools" role="toolbar" aria-label={t`System log filters`}>
        <div className="logs-search syslog-search">
          <TextField
            size={phone ? "md" : "sm"}
            icon="search"
            mono
            value={typed}
            maxLength={MAX_QUERY}
            onChange={(e) => setTyped(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === "Escape") setTyped("");
            }}
            placeholder={t`Search lines`}
            aria-label={t`Search the system log`}
            action={
              typed ? (
                <Button
                  size="sm"
                  variant="ghost"
                  icon="x"
                  aria-label={t`Clear the search`}
                  onClick={() => setTyped("")}
                />
              ) : undefined
            }
          />
        </div>
        <UnitPicker server={server} value={unitName} onChange={(unit) => set({ unit })} />
        <SegmentedControl
          size="sm"
          label={t`Level`}
          value={search.level ?? "all"}
          onChange={(v) => set({ level: v === "all" ? undefined : v })}
          options={levels}
        />
        <span className="logs-fill" />
        <SegmentedControl
          size="sm"
          label={t`Time range`}
          value={search.range ?? "live"}
          onChange={(v) => set({ range: v === "live" ? undefined : v })}
          className="logs-range"
          options={ranges}
        />
      </div>
      {log.error && log.entries.length > 0 && !isAgentOutdated(log.error) ? (
        <p className="procs-note" role="status">
          {sentence(log.error)}
        </p>
      ) : null}
      <div className="syslog-pane">
        <LogStream
          lines={shown}
          title={<span className="syslog-title">{unitName || t`Every system service`}</span>}
          height="fill"
          live={log.live && log.following}
          follow={follow}
          onFollowChange={setFollow}
          showSource={!unitName}
          highlight={search.q}
          empty={empty}
          top={top}
        />
      </div>
    </section>
  );
}

/** One entry as the log stream draws it; notes get their words where t is. */
function drawEntry(e: SystemLogEntry, locale: string): LogLine {
  if (e.kind !== "line") {
    return {
      id: e.key,
      time: "",
      source: "",
      text: "",
      notice: e.kind === "gap" ? "info" : "warning",
      day: dayNoteOf(e.at, locale),
    };
  }
  const l = e.line;
  const ms = l.time ? timestampMs(l.time) : 0;
  const time = ms ? clock(ms) : "";
  return {
    id: l.cursor,
    time,
    timeTitle: ms ? `${dayOf(ms, locale)}, ${time}` : "",
    // A unit's name without .service, which the column has room for; the whole in its title.
    source: l.unit.replace(/\.service$/, ""),
    sourceTitle: l.pid ? `${l.unit}[${l.pid}]` : l.unit,
    level: l.level === LogLevel.ERROR ? "error" : l.level === LogLevel.WARN ? "warn" : "info",
    text: l.message,
    at: ms || undefined,
    day: ms ? dayNoteOf(ms, locale) : undefined,
  };
}

/**
 * Which system service to show, picked from the server's own list, failed ones
 * first: read once as the page opens, not watched.
 */
function UnitPicker({ server, value, onChange }: { server: Server; value: string; onChange: (unit: string) => void }) {
  const { t } = useLingui();
  const connected = server.connection === Connection.CONNECTED;
  const [read, setRead] = useState(false);
  const { list } = useSystemServices(server.id, connected && !read);
  const [units, setUnits] = useState<SystemServiceInfo[]>([]);
  useEffect(() => {
    if (!list) return;
    setUnits([...list.services]);
    setRead(true);
  }, [list]);
  const failed = units.filter((s) => s.state === SystemServiceState.FAILED);
  const rest = units.filter((s) => s.state !== SystemServiceState.FAILED).sort((a, b) => a.name.localeCompare(b.name));
  // A unit named in the URL is offered even before the list is read, or if it isn't in it.
  const known = units.some((s) => s.name === value);
  return (
    <label className="dw-input dw-input-sm logs-select syslog-unit">
      <span className="dw-sr">{t`System service`}</span>
      <select value={value} onChange={(e) => onChange(e.target.value)} className="dw-input-control">
        <option value="">{t`Every system service`}</option>
        {value && !known ? <option value={value}>{value}</option> : null}
        {failed.length > 0 ? (
          <optgroup label={t({ message: "Failed", context: "system services" })}>
            {failed.map((s) => (
              <option key={s.name} value={s.name}>
                {s.name}
              </option>
            ))}
          </optgroup>
        ) : null}
        {rest.length > 0 ? (
          <optgroup label={t`Every other`}>
            {rest.map((s) => (
              <option key={s.name} value={s.name}>
                {s.name}
              </option>
            ))}
          </optgroup>
        ) : null}
      </select>
      <Icon name="chevron-down" size={14} className="logs-select-icon" />
    </label>
  );
}
