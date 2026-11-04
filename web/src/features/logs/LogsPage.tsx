// SPDX-License-Identifier: AGPL-3.0-only

import { durationFromMs, timestampFromMs } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError, createClient } from "@connectrpc/connect";
import { useQuery, useTransport } from "@connectrpc/connect-query";
import {
  LogGapReason,
  LogKept,
  LogLevel,
  LogService,
  type LogSource,
  LogSourceState,
  ServerLogState,
  type ServerLogStatus,
} from "@dockwell/api";
import {
  Button,
  Callout,
  cx,
  Glyph,
  Icon,
  type LogLine,
  LogStream,
  PHONE_QUERY,
  plainText,
  SegmentedControl,
  TextField,
  TONE,
  useMediaQuery,
} from "@dockwell/ui";
import type { I18n } from "@lingui/core";
import { msg, plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import {
  memo,
  type ReactNode,
  useCallback,
  useEffect,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
  useSyncExternalStore,
} from "react";

import { reasonOf, sentence } from "../../lib/errors";
import { dateLocale, formatBytes, formatClock, formatDateTime } from "../../lib/format";
import { EmptyState } from "../shell/EmptyState";
import { type Entry, LogStore, MAX_LINES } from "./store";
import { type StreamState, useLogStream } from "./useLogStream";
import "./LogsPage.css";

/** The time ranges, as the URL names them. */
export type LogRange = "15m" | "1h" | "24h";

const RANGE_MS: Readonly<Record<LogRange, number>> = { "15m": 15 * 60_000, "1h": 3_600_000, "24h": 86_400_000 };

/** How many of each container's newest lines a live view starts with. */
const LIVE_TAIL = 1000;
/** How many lines a page of older lines holds. */
const OLDER_PAGE = 1000;

/** The levels a view can show, in the order the filters name them. */
const FILTER_LEVELS = [LogLevel.INFO, LogLevel.WARN, LogLevel.ERROR] as const;

/** Where a view of logs is, in its URL, so a link shows the same thing. */
export interface LogsSearch {
  range?: LogRange;
  /** Which levels show, such as "warn,error". Unset shows all. */
  levels?: string;
  /** Which replica shows, by its label. */
  replica?: string;
  /** Text to find. */
  q?: string;
  /** Search all that the servers keep, rather than what is loaded. */
  deep?: boolean;
  /** Which deploy shows, by the change that made it. */
  change?: string;
  /** Which deploy shows, by its revision. */
  revision?: number;
}

const LEVEL_NAMES: ReadonlyMap<string, LogLevel> = new Map([
  ["info", LogLevel.INFO],
  ["warn", LogLevel.WARN],
  ["error", LogLevel.ERROR],
]);

function levelsOf(search: LogsSearch): ReadonlySet<LogLevel> {
  const out = new Set<LogLevel>();
  for (const name of search.levels?.split(",") ?? []) {
    const level = LEVEL_NAMES.get(name);
    if (level !== undefined) out.add(level);
  }
  return out;
}

function levelNames(levels: ReadonlySet<LogLevel>): string | undefined {
  if (levels.size === 0 || levels.size === FILTER_LEVELS.length) return undefined;
  const names: string[] = [];
  for (const [name, level] of LEVEL_NAMES) if (levels.has(level)) names.push(name);
  return names.join(",");
}

/** A replica as a person reads it: its server, and which one it is there when there are more. */
export function replicaLabel(s: Pick<LogSource, "serverName" | "replica">): string {
  return s.replica > 1 ? `${s.serverName}-${s.replica}` : s.serverName;
}

function pad(n: number, width = 2): string {
  return String(n).padStart(width, "0");
}

/** Wall-clock time with milliseconds, in the viewer's zone, without Intl, as this runs for each line drawn. */
function clock(ms: number): string {
  const d = new Date(ms);
  return `${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}.${pad(d.getMilliseconds(), 3)}`;
}

const dayFormats = new Map<string, Intl.DateTimeFormat>();

/** A line's day, for its tooltip. Making a format is slow, and this runs for each line drawn, so there is one. */
function dayOf(ms: number, locale: string): string {
  let format = dayFormats.get(locale);
  if (!format) {
    format = new Intl.DateTimeFormat(dateLocale(locale), { day: "numeric", month: "short", year: "numeric" });
    dayFormats.set(locale, format);
  }
  return format.format(ms);
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

/**
 * A service's logs: following live, a time range, or one deploy's, with filters
 * by level and replica, and search (App-Logs, M-Logs). It sits in an app's tab,
 * and the page around it says which app.
 */
export function LogsPage({
  serviceId,
  search,
  onSearch,
}: {
  serviceId: string;
  search: LogsSearch;
  onSearch: (next: LogsSearch) => void;
}) {
  const { t, i18n } = useLingui();
  const phone = useMediaQuery(PHONE_QUERY);
  const store = useMemo(() => new LogStore(), []);
  const deepStore = useMemo(() => new LogStore(), []);

  const sources = useQuery(LogService.method.listLogSources, { id: serviceId }, { refetchOnWindowFocus: false });
  const service = sources.data?.service;
  const allSources = sources.data?.sources;
  const deploys = useMemo(() => deploysOf(allSources ?? []), [allSources]);
  const refetch = sources.refetch;
  const later = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);
  const refreshSources = useCallback(() => {
    if (later.current !== undefined) return;
    later.current = setTimeout(() => {
      later.current = undefined;
      void refetch();
    }, 500);
  }, [refetch]);
  useEffect(() => () => clearTimeout(later.current), []);

  // Which deploy shows. With no container now, as after a first deploy that was
  // rolled back, the newest one the server kept stands in.
  const asked =
    search.change || search.revision
      ? deploys.find((d) => (search.change ? d.change === search.change : d.revision === BigInt(search.revision ?? 0)))
      : undefined;
  const noCurrent = allSources !== undefined && allSources.length > 0 && !deploys.some((d) => d.current);
  const chosen = asked ?? (noCurrent && !search.change && !search.revision ? deploys[0] : undefined);
  // A deploy no server knows of has gone, unless a server couldn't answer.
  const askedDeploy = search.change !== undefined || search.revision !== undefined;
  const everyServerAnswered = (sources.data?.servers.length ?? 0) === 0;
  const deployGone = allSources !== undefined && askedDeploy && asked === undefined && everyServerAnswered;
  const reading = chosen !== undefined ? !chosen.current : askedDeploy;

  // A search of all the servers keep leaves this running, so coming back from it
  // finds the lines still here, and those written meanwhile.
  const loaded = allSources !== undefined;
  const range = search.range;
  const chosenChange = chosen?.change ?? search.change ?? "";
  const chosenRevision = chosen ? (chosen.change ? 0n : chosen.revision) : BigInt(search.revision ?? 0);
  const request = useMemo(() => {
    if (!loaded || deployGone) return undefined;
    const base = { id: serviceId, changeId: chosenChange, revision: chosenRevision };
    if (range) return { ...base, last: durationFromMs(RANGE_MS[range]), tail: MAX_LINES, once: true };
    if (reading) return { ...base, tail: MAX_LINES, once: true };
    return { ...base, tail: LIVE_TAIL };
  }, [serviceId, range, chosenChange, chosenRevision, reading, deployGone, loaded]);

  const stream = useLogStream(request, store, refreshSources);
  const deep = useDeepSearch(serviceId, search, deepStore);
  const active = search.deep && search.q ? deepStore : store;

  // How entries are drawn: their time, their source's name and the words of a note.
  const [sourceMap, setSourceMap] = useState<ReadonlyMap<string, LogSource>>(new Map());
  useEffect(() => {
    const merged = new Map<string, LogSource>();
    for (const s of allSources ?? []) merged.set(s.id, s);
    for (const [id, s] of stream.sources) merged.set(id, s);
    for (const s of deep.sources) merged.set(s.id, s);
    for (const [id, s] of merged) {
      store.setSource(id, replicaLabel(s), `${s.serverId}/${s.replica}`);
      deepStore.setSource(id, replicaLabel(s), `${s.serverId}/${s.replica}`);
    }
    setSourceMap(merged);
  }, [allSources, stream.sources, deep.sources, store, deepStore]);

  useEffect(() => {
    const draw = (e: Entry): LogLine => drawEntry(e, sourceMap, i18n);
    store.setDraw(draw);
    deepStore.setDraw(draw);
  }, [sourceMap, store, deepStore, i18n]);

  // Filters, from the URL. Typing in the search waits a moment for the next key,
  // and the URL follows.
  const levelsText = search.levels;
  const levels = useMemo(() => levelsOf({ ...(levelsText ? { levels: levelsText } : {}) }), [levelsText]);
  const label = search.replica ?? "";
  const [typed, setTyped] = useState(search.q ?? "");
  useEffect(() => setTyped(search.q ?? ""), [search.q]);
  const query = search.deep ? "" : (search.q ?? "");
  useEffect(() => {
    active.setFilter({ levels: search.deep ? new Set() : levels, label, query });
  }, [active, levels, label, query, search.deep]);
  const debounce = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);
  const latest = useRef(search);
  latest.current = search;
  const searchFor = (text: string) => {
    setTyped(text);
    clearTimeout(debounce.current);
    debounce.current = setTimeout(() => {
      const { q: _q, deep: _d, ...rest } = latest.current;
      onSearch(text ? { ...rest, q: text } : rest);
    }, 120);
  };
  useEffect(() => () => clearTimeout(debounce.current), []);
  const searchDeep = () => {
    clearTimeout(debounce.current);
    onSearch({ ...latest.current, q: typed, deep: true });
  };

  // The log takes what is left of the screen, so what scrolls is the log, and
  // what sits above it stays put.
  const section = useRef<HTMLElement>(null);
  useLayoutEffect(() => {
    const el = section.current;
    if (!el) return undefined;
    const measure = () => {
      const top = el.getBoundingClientRect().top + window.scrollY;
      const bar = document.querySelector<HTMLElement>(".tabbar");
      const below = bar ? bar.offsetHeight + 12 : 24;
      el.style.setProperty("--logs-height", `${Math.max(360, window.innerHeight - top - below)}px`);
    };
    measure();
    window.addEventListener("resize", measure);
    return () => window.removeEventListener("resize", measure);
  });

  const replicas = useMemo(() => {
    const seen = new Set<string>();
    for (const s of chosen ? chosen.sources : sourceMap.values())
      if (chosen || s.state !== LogSourceState.KEPT) seen.add(replicaLabel(s));
    return [...seen].sort();
  }, [chosen, sourceMap]);

  const setLevels = (next: ReadonlySet<LogLevel>) => {
    const { levels: _l, ...rest } = search;
    const names = levelNames(next);
    onSearch(names ? { ...rest, levels: names } : rest);
  };
  const setReplica = (value: string) => {
    const { replica: _r, ...rest } = search;
    onSearch(value === "all" ? rest : { ...rest, replica: value });
  };
  const setRange = (value: string) => {
    const { range: _r, ...rest } = search;
    onSearch(value === "live" ? rest : { ...rest, range: value as LogRange });
  };
  const setDeploy = (value: string) => {
    const { change: _c, revision: _r, ...rest } = search;
    const d = deploys.find((x) => x.key === value);
    if (!d || value === "current") onSearch(rest);
    else if (d.change) onSearch({ ...rest, change: d.change });
    else onSearch({ ...rest, revision: Number(d.revision) });
  };
  const clearFilters = () => {
    const { levels: _l, replica: _r, q: _q, deep: _d, ...rest } = search;
    setTyped("");
    onSearch(rest);
  };

  // Live says lines can come. With no server to read from, the note above says why they can't.
  const statuses = [...stream.servers.values()];
  const noneReading =
    statuses.length > 0 &&
    statuses.every((s) => s.state === ServerLogState.OFFLINE || s.state === ServerLogState.UNAVAILABLE);
  const live = !search.deep && !search.range && !reading && stream.status === "live" && !noneReading;
  const [follow, setFollow] = useState(true);
  // biome-ignore lint/correctness/useExhaustiveDependencies: a new view of the logs starts at its end
  useEffect(() => setFollow(true), [request]);

  // What the servers say of themselves: at most one note, what matters most first.
  const down = statuses.filter((s) => s.state === ServerLogState.OFFLINE || s.state === ServerLogState.UNAVAILABLE);
  const stopped =
    !reading && deploys.flatMap((d) => (d.current ? d.sources : [])).filter((s) => s.state === LogSourceState.STOPPED);
  const unbounded = (allSources ?? []).filter((s) => s.unbounded && s.state !== LogSourceState.KEPT);
  const logErrors = [...sourceMap.values()].filter((s) => s.logError);

  const refused = stream.error?.code === Code.PermissionDenied || sources.error?.code === Code.PermissionDenied;
  const missing = stream.error?.code === Code.NotFound || sources.error?.code === Code.NotFound;
  if (refused) {
    return (
      <div className="logs-panel">
        <EmptyState icon="lock" title={t`You can't read these logs`}>
          <p>
            {reasonOf(stream.error ?? sources.error) === "token_logs" ? (
              sentence(stream.error ?? sources.error)
            ) : (
              <Trans>
                Logs often hold secrets that apps print, so reading them takes permission. Ask the owner of this install
                for it.
              </Trans>
            )}
          </p>
        </EmptyState>
      </div>
    );
  }
  if (missing) {
    return (
      <div className="logs-panel">
        <EmptyState icon="service" title={t`This app isn't here`}>
          <p>
            <Trans>It may have been deleted. Dockwell has no logs under this address.</Trans>
          </p>
        </EmptyState>
      </div>
    );
  }
  if (deployGone) {
    const { change: _c, revision: _r, ...rest } = search;
    return (
      <div className="logs-panel">
        <EmptyState
          icon="archive"
          title={t`This deploy's logs are gone`}
          action={
            <Button onClick={() => onSearch(rest)}>
              <Trans>Show the current logs</Trans>
            </Button>
          }
        >
          <p>
            <Trans>
              A container's log goes when it is removed. Each server keeps the logs of the last failed and the last
              replaced container of each app, for 30 days, within 256 MB in all.
            </Trans>
          </p>
        </EmptyState>
      </div>
    );
  }

  const name = service?.name ?? "";
  const scopeText = reading && chosen ? deployName(chosen, i18n) : (service?.environment ?? "");
  const count = replicas.length;
  const title = phone
    ? capitalize(scopeText)
    : [name, scopeText, count > 1 ? plural(count, { one: "# replica", other: "# replicas" }) : ""]
        .filter(Boolean)
        .join(" · ");

  const levelFiltered = levels.size > 0;
  const download = () =>
    saveText(active, `${name || "logs"}-${new Date().toISOString().replaceAll(":", "-")}.log`, sourceMap);

  return (
    <section ref={section} className={cx("logs", phone && "is-phone")} aria-label={t`Logs`}>
      <Note
        name={name}
        stream={stream}
        down={down}
        stopped={stopped === false ? [] : stopped}
        logErrors={logErrors}
        unbounded={unbounded}
        chosen={chosen}
        noCurrent={noCurrent && !asked}
        locale={i18n.locale}
      />
      <div className="logs-tools" role="toolbar" aria-label={t`Log filters`}>
        <div className="logs-search">
          <TextField
            size={phone ? "md" : "sm"}
            icon="search"
            mono
            value={typed}
            onChange={(e) => searchFor(e.target.value)}
            placeholder={t`Search lines`}
            aria-label={t`Search logs`}
          />
        </div>
        {phone ? (
          <PhoneFilters
            levels={levels}
            onLevels={setLevels}
            replicas={replicas}
            replica={label}
            onReplica={setReplica}
          />
        ) : (
          <>
            <LevelToggles levels={levels} onLevels={setLevels} />
            <span className="logs-fill" />
            {replicas.length > 4 ? (
              <label className="dw-input dw-input-sm logs-select">
                <span className="dw-sr">{t`Replica`}</span>
                <select
                  value={label || "all"}
                  onChange={(e) => setReplica(e.target.value)}
                  className="dw-input-control"
                >
                  <option value="all">{t`All replicas`}</option>
                  {replicas.map((r) => (
                    <option key={r} value={r}>
                      {r}
                    </option>
                  ))}
                </select>
                <Icon name="chevron-down" size={14} className="logs-select-icon" />
              </label>
            ) : replicas.length > 1 ? (
              <SegmentedControl
                size="sm"
                label={t`Replica`}
                value={label || "all"}
                onChange={setReplica}
                options={[
                  { value: "all", label: t`All replicas` },
                  ...replicas.map((r) => ({ value: r, label: <span className="logs-mono">{r}</span> })),
                ]}
              />
            ) : null}
          </>
        )}
        {!reading && !search.deep ? (
          <SegmentedControl
            size="sm"
            label={t`Time range`}
            value={search.range ?? "live"}
            onChange={setRange}
            className="logs-range"
            options={[
              { value: "live", label: t`Live` },
              { value: "15m", label: t`15 min` },
              { value: "1h", label: t`1 h` },
              { value: "24h", label: t`24 h` },
            ]}
          />
        ) : null}
      </div>
      {deploys.length > 1 ? (
        <DeployChooser deploys={deploys} chosen={chosen} onChange={setDeploy} noCurrent={noCurrent} />
      ) : null}
      <SearchReach
        search={search}
        store={active}
        deep={deep}
        levelFiltered={levelFiltered}
        reading={reading}
        onDeep={searchDeep}
        onLoaded={() => {
          const { deep: _d, ...rest } = search;
          onSearch(rest);
        }}
      />
      <Skipped store={active} />
      <LogPane
        store={active}
        title={title}
        live={live}
        follow={follow}
        onFollow={setFollow}
        label={label}
        toolbar={
          phone ? undefined : (
            <>
              <CopyLines store={active} sources={sourceMap} />
              <Button
                size="sm"
                variant="ghost"
                icon="download"
                aria-label={t`Download these lines`}
                title={t`Download these lines`}
                onClick={download}
              />
            </>
          )
        }
        top={
          search.deep ? (
            deep.next ? (
              <div className="logs-earlier">
                <Button size="sm" variant="ghost" icon="chevron-up-down" loading={deep.loading} onClick={deep.loadMore}>
                  <Trans>Show older matches</Trans>
                </Button>
              </div>
            ) : undefined
          ) : !search.q ? (
            <EarlierLines serviceId={serviceId} store={active} chosen={chosen} search={search} />
          ) : undefined
        }
        stream={stream}
        deep={search.deep ? deep : undefined}
        name={name}
        hasSources={(allSources?.length ?? 0) > 0}
        filtered={levelFiltered || label !== "" || query !== ""}
        onClear={clearFilters}
        highlight={search.deep ? (search.q ?? "") : query}
      />
      {phone ? (
        <div className="logs-bar">
          <Button icon="download" onClick={download}>
            <Trans>Download</Trans>
          </Button>
          <CopyLines store={active} sources={sourceMap} wide />
        </div>
      ) : null}
    </section>
  );
}

function capitalize(s: string): string {
  return s ? s.charAt(0).toUpperCase() + s.slice(1) : s;
}

function deployName(d: Deploy, i18n: I18n): string {
  const revision = d.revision > 0n ? `r${d.revision}` : (d.sources[0]?.id ?? "");
  if (d.failed) return i18n._(msg`${revision} · failed`);
  if (!d.current) return i18n._(msg`${revision} · replaced`);
  return revision;
}

function drawEntry(e: Entry, sources: ReadonlyMap<string, LogSource>, i18n: I18n): LogLine {
  const s = sources.get(e.source);
  const source = s ? replicaLabel(s) : e.source;
  if (e.gap) return { id: e.id, time: "", source, text: gapWords(e, source, i18n), notice: "warning" };
  if (e.started) {
    const revision = s && s.revision > 0n ? `r${s.revision}` : e.source;
    const container = e.source;
    return {
      id: e.id,
      time: "",
      source,
      text: i18n._(msg`${source} started ${revision}, in container ${container}`),
      notice: "info",
    };
  }
  const level = e.level === LogLevel.ERROR ? "error" : e.level === LogLevel.WARN ? "warn" : "info";
  const line: LogLine = {
    id: e.id,
    time: clock(e.ms),
    timeTitle: `${dayOf(e.ms, i18n.locale)}, ${clock(e.ms)}`,
    source,
    level,
    text: e.text,
  };
  if (s) {
    const revision = s.revision > 0n ? `r${s.revision} · ` : "";
    line.sourceTitle = `${source} · ${revision}${e.source}`;
  }
  if (e.cut > 0) line.cut = formatBytes(e.cut, i18n.locale);
  return line;
}

/** What a marker in the stream says: what is missing, and why. */
function gapWords(e: Entry, source: string, i18n: I18n): string {
  const gap = e.gap;
  if (!gap) return "";
  const lines = Number(gap.lines).toLocaleString(i18n.locale);
  switch (gap.reason) {
    case LogGapReason.TOO_FAST:
      return plural(Number(gap.lines), {
        one: `${lines} line from ${source} not shown: it wrote faster than a live view carries.`,
        other: `${lines} lines from ${source} not shown: it wrote faster than a live view carries.`,
      });
    case LogGapReason.READER_BEHIND:
      return plural(Number(gap.lines), {
        one: `${lines} line from ${source} not shown: this page or the network to it fell behind.`,
        other: `${lines} lines from ${source} not shown: this page or the network to it fell behind.`,
      });
    case LogGapReason.GONE: {
      const from = gap.startTime ? clock(Number(gap.startTime.seconds) * 1000) : "";
      const to = gap.endTime ? clock(Number(gap.endTime.seconds) * 1000) : "";
      return to
        ? i18n._(msg`Lines from ${source} between ${from} and ${to} are gone: the server no longer has them.`)
        : i18n._(msg`Lines from ${source} after ${from} are gone: the server no longer has this container's log.`);
    }
    default:
      return i18n._(msg`Some lines from ${source} are missing here.`);
  }
}

/** A line as text, for copying and downloading: no codes, and everything a reader saw. */
function textOf(e: Entry, sources: ReadonlyMap<string, LogSource>): string {
  const s = sources.get(e.source);
  const source = s ? replicaLabel(s) : e.source;
  const time = new Date(e.ms).toISOString();
  if (e.gap) return `${time} ${source} -- ${Number(e.gap.lines)} lines not shown --`;
  if (e.started) return `${time} ${source} -- new container ${e.source} --`;
  const level = e.level === LogLevel.ERROR ? "ERROR " : e.level === LogLevel.WARN ? "WARN " : "";
  return `${time} ${source} ${level}${plainText(e.text)}`;
}

function saveText(store: LogStore, name: string, sources: ReadonlyMap<string, LogSource>) {
  const blob = new Blob([store.text((e) => textOf(e, sources))], { type: "text/plain;charset=utf-8" });
  const url = URL.createObjectURL(blob);
  const a = document.createElement("a");
  a.href = url;
  a.download = name;
  document.body.appendChild(a);
  a.click();
  a.remove();
  setTimeout(() => URL.revokeObjectURL(url), 10_000);
}

function CopyLines({
  store,
  sources,
  wide = false,
}: {
  store: LogStore;
  sources: ReadonlyMap<string, LogSource>;
  wide?: boolean;
}) {
  const { t } = useLingui();
  const [copied, setCopied] = useState(false);
  useEffect(() => {
    if (!copied) return undefined;
    const timer = setTimeout(() => setCopied(false), 2000);
    return () => clearTimeout(timer);
  }, [copied]);
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(store.text((e) => textOf(e, sources)));
      setCopied(true);
    } catch {
      // Clipboard access was refused; Download still works.
    }
  };
  return (
    <>
      {wide ? (
        <Button icon={copied ? "check" : "copy"} onClick={() => void copy()}>
          {copied ? <Trans>Copied</Trans> : <Trans>Copy</Trans>}
        </Button>
      ) : (
        <Button
          size="sm"
          variant="ghost"
          icon={copied ? "check" : "copy"}
          aria-label={t`Copy these lines`}
          title={t`Copy these lines`}
          onClick={() => void copy()}
        />
      )}
      <span className="dw-sr" aria-live="polite">
        {copied ? t`Copied the lines shown` : ""}
      </span>
    </>
  );
}

/** At most one note on how the logs stand, what matters most first. */
function Note({
  name,
  stream,
  down,
  stopped,
  logErrors,
  unbounded,
  chosen,
  noCurrent,
  locale,
}: {
  name: string;
  stream: StreamState;
  down: ServerLogStatus[];
  stopped: LogSource[];
  logErrors: LogSource[];
  unbounded: LogSource[];
  chosen: Deploy | undefined;
  noCurrent: boolean;
  locale: string;
}) {
  const { t, i18n } = useLingui();
  const first = down[0];
  if (first) {
    return (
      <Callout
        tone={first.state === ServerLogState.OFFLINE ? "critical" : "warning"}
        title={
          first.state === ServerLogState.OFFLINE
            ? t`${first.serverName} is offline`
            : t`${first.serverName} can't read logs`
        }
      >
        {sentence(new ConnectError(first.message))}
        {first.state === ServerLogState.OFFLINE ? (
          <>
            {" "}
            <Trans>The lines from while it was away arrive when it returns, as far as Docker kept them.</Trans>
          </>
        ) : null}
      </Callout>
    );
  }
  if (stream.status === "reconnecting") {
    return (
      <Callout tone="warning" title={t`Reconnecting to the control plane`}>
        <Trans>The lines written meanwhile arrive once it answers, with none twice.</Trans>
      </Callout>
    );
  }
  const failing = logErrors[0];
  if (failing) {
    return (
      <Callout tone="warning" title={t`${failing.serverName} can't read a container's log`}>
        {failing.logError}
      </Callout>
    );
  }
  if (chosen && !chosen.current) {
    const reason = chosen.failed ? t`This deploy failed and was rolled back.` : t`This deploy was replaced.`;
    return (
      <Callout
        tone={chosen.failed ? "critical" : "info"}
        title={noCurrent ? t`${name} has no container now` : deployName(chosen, i18n)}
      >
        {noCurrent ? t`These are the logs of its last deploy.` : reason}{" "}
        <Trans>The server keeps the last failed and the last replaced deploy of each app, for 30 days.</Trans>
        {chosen.sources.some((s) => s.cutShort) ? (
          <>
            {" "}
            <Trans>It wrote more than the server keeps of one container, so only its newest lines are here.</Trans>
          </>
        ) : null}
      </Callout>
    );
  }
  const off = stopped[0];
  if (off) {
    const at = off.finishTime ? formatClock(new Date(Number(off.finishTime.seconds) * 1000), locale) : "";
    const code = off.exitCode;
    return (
      <Callout tone={code === 0 ? "info" : "warning"} title={t`${name} isn't running on ${off.serverName}`}>
        <Trans>
          Its container stopped at {at} with exit code {code}. These are its last lines, and new lines show once it
          starts again.
        </Trans>
      </Callout>
    );
  }
  const wild = unbounded[0];
  if (wild) {
    return (
      <Callout tone="warning" title={t`This container's log has no limit on ${wild.serverName}`}>
        <Trans>
          It was made without rotation, so its log can fill the disk. Dockwell sets rotation on the containers it makes,
          so the next deploy of this app fixes it.
        </Trans>
      </Callout>
    );
  }
  return null;
}

/** The levels as the wide layout has them: each one on or off. */
function LevelToggles({
  levels,
  onLevels,
}: {
  levels: ReadonlySet<LogLevel>;
  onLevels: (next: ReadonlySet<LogLevel>) => void;
}) {
  const { t } = useLingui();
  const on = (l: LogLevel) => levels.size === 0 || levels.has(l);
  const toggle = (l: LogLevel) => {
    const next = new Set(levels.size === 0 ? FILTER_LEVELS : levels);
    if (next.has(l)) next.delete(l);
    else next.add(l);
    onLevels(next.size === 0 ? new Set() : next);
  };
  return (
    // biome-ignore lint/a11y/useSemanticElements: a fieldset can't be laid out as this row of chips
    <div
      className="logs-levels"
      role="group"
      aria-label={t`Levels`}
      title={t`Levels are read from each line's text. A line that names none counts as info.`}
    >
      <LevelChip pressed={on(LogLevel.INFO)} onClick={() => toggle(LogLevel.INFO)}>
        <Trans>Info</Trans>
      </LevelChip>
      <LevelChip pressed={on(LogLevel.WARN)} onClick={() => toggle(LogLevel.WARN)} level="warning">
        <Trans>Warn</Trans>
      </LevelChip>
      <LevelChip pressed={on(LogLevel.ERROR)} onClick={() => toggle(LogLevel.ERROR)} level="critical">
        <Trans>Error</Trans>
      </LevelChip>
    </div>
  );
}

function LevelChip({
  pressed,
  onClick,
  level,
  mono,
  children,
}: {
  pressed: boolean;
  onClick: () => void;
  level?: "warning" | "critical";
  mono?: boolean;
  children: ReactNode;
}) {
  return (
    <button
      type="button"
      className={cx("logs-chip", pressed && "is-on", mono && "is-mono")}
      aria-pressed={pressed}
      onClick={onClick}
    >
      <span className="logs-chip-face">
        {level ? (
          <span className={`logs-chip-glyph is-${level}`}>
            <Glyph shape={TONE[level].shape} size={9} />
          </span>
        ) : null}
        <span>{children}</span>
      </span>
    </button>
  );
}

/** The levels and replicas as a phone has them: one row of chips, one level at a time. */
function PhoneFilters({
  levels,
  onLevels,
  replicas,
  replica,
  onReplica,
}: {
  levels: ReadonlySet<LogLevel>;
  onLevels: (next: ReadonlySet<LogLevel>) => void;
  replicas: readonly string[];
  replica: string;
  onReplica: (value: string) => void;
}) {
  const { t } = useLingui();
  const only = (l: LogLevel) => levels.size === 1 && levels.has(l);
  const pick = (l: LogLevel) => onLevels(only(l) ? new Set() : new Set([l]));
  return (
    // biome-ignore lint/a11y/useSemanticElements: a fieldset can't scroll as this row of chips does
    <div className="logs-chips" role="group" aria-label={t`Filter log lines`}>
      <LevelChip pressed={levels.size === 0} onClick={() => onLevels(new Set())}>
        <Trans>All levels</Trans>
      </LevelChip>
      <LevelChip pressed={only(LogLevel.ERROR)} onClick={() => pick(LogLevel.ERROR)} level="critical">
        <Trans>Errors</Trans>
      </LevelChip>
      <LevelChip pressed={only(LogLevel.WARN)} onClick={() => pick(LogLevel.WARN)} level="warning">
        <Trans>Warnings</Trans>
      </LevelChip>
      <LevelChip pressed={only(LogLevel.INFO)} onClick={() => pick(LogLevel.INFO)}>
        <Trans>Info</Trans>
      </LevelChip>
      {replicas.length > 1 ? (
        <>
          <span aria-hidden="true" className="logs-chips-rule" />
          {replicas.map((r) => (
            <LevelChip key={r} pressed={replica === r} mono onClick={() => onReplica(replica === r ? "all" : r)}>
              {r}
            </LevelChip>
          ))}
        </>
      ) : null}
    </div>
  );
}

function DeployChooser({
  deploys,
  chosen,
  onChange,
  noCurrent,
}: {
  deploys: readonly Deploy[];
  chosen: Deploy | undefined;
  onChange: (key: string) => void;
  noCurrent: boolean;
}) {
  const { t, i18n } = useLingui();
  const current = deploys.filter((d) => d.current);
  // Two to five, as the control takes; a server keeps few.
  const others = deploys.filter((d) => !d.current).slice(0, noCurrent ? 5 : 4);
  const options = [
    ...(noCurrent ? [] : [{ value: "current", label: current[0] ? t`Running now` : t`Current` }]),
    ...others.map((d) => ({ value: d.key, label: <span className="logs-mono">{deployName(d, i18n)}</span> })),
  ];
  if (options.length < 2) return null;
  const value = chosen && !chosen.current ? chosen.key : "current";
  return (
    <div className="logs-deploys">
      <span className="logs-deploys-label">
        <Trans>Deployment</Trans>
      </span>
      <SegmentedControl size="sm" label={t`Deployment`} value={value} onChange={onChange} options={options} />
    </div>
  );
}

/**
 * How many lines a reader has missed, while it goes on: markers in the stream say
 * so where it happened, and at a flood's pace they scroll past as fast as the rest.
 */
function Skipped({ store }: { store: LogStore }) {
  const { i18n } = useLingui();
  useSyncExternalStore(store.subscribe, store.snapshot);
  if (store.skipped === 0) return null;
  const count = store.skipped.toLocaleString(i18n.locale);
  return (
    <p className="logs-skipped">
      <Glyph shape={TONE.warning.shape} size={8} />
      {plural(store.skipped, {
        one: `${count} line not shown so far, as markers in the log say. dockwell logs read gets what the server still has.`,
        other: `${count} lines not shown so far, as markers in the log say. dockwell logs read gets what the server still has.`,
      })}
    </p>
  );
}

/** How far a search reached, and how to go further. */
function SearchReach({
  search,
  store,
  deep,
  levelFiltered,
  reading,
  onDeep,
  onLoaded,
}: {
  search: LogsSearch;
  store: LogStore;
  deep: DeepSearch;
  levelFiltered: boolean;
  reading: boolean;
  onDeep: () => void;
  onLoaded: () => void;
}) {
  const { i18n } = useLingui();
  useSyncExternalStore(store.subscribe, store.snapshot);
  const words = levelFiltered ? (
    <Trans>Levels are read from each line's text. A line that names none counts as info.</Trans>
  ) : null;
  if (search.deep && search.q) {
    const when = deep.reached ? formatDateTime(new Date(deep.reached), i18n.locale) : "";
    const lines = deep.lines.toLocaleString(i18n.locale);
    return (
      <p className="logs-reach" aria-live="polite">
        {deep.loading ? (
          <Trans>Searching all the servers keep</Trans>
        ) : deep.error ? (
          sentence(deep.error)
        ) : deep.reached ? (
          deep.cutShort ? (
            <Trans>
              Searched {lines} lines, back to {when}, where the search stopped at its limit. Search for something more
              specific to reach further.
            </Trans>
          ) : (
            <Trans>
              Searched {lines} lines, back to {when}: as far as the servers keep logs, about 50 MB of each container,
              and the last failed and last replaced deploys.
            </Trans>
          )
        ) : (
          <Trans>The servers have no lines to search.</Trans>
        )}{" "}
        <button type="button" className="logs-link" onClick={onLoaded}>
          <Trans>Back to the lines loaded</Trans>
        </button>
      </p>
    );
  }
  if (search.q) {
    const shown = store.length.toLocaleString(i18n.locale);
    const total = store.total.toLocaleString(i18n.locale);
    return (
      <p className="logs-reach" aria-live="polite">
        {plural(store.length, {
          one: `${shown} of the ${total} lines loaded matches.`,
          other: `${shown} of the ${total} lines loaded match.`,
        })}{" "}
        {reading ? null : (
          <button type="button" className="logs-link" onClick={onDeep}>
            <Trans>Search all the servers keep</Trans>
          </button>
        )}
        {words ? <> {words}</> : null}
      </p>
    );
  }
  if (words) return <p className="logs-reach">{words}</p>;
  return null;
}

export interface DeepSearch {
  loading: boolean;
  reached: number;
  lines: bigint;
  cutShort: boolean;
  next: string;
  error?: ConnectError;
  sources: readonly LogSource[];
  loadMore(): void;
}

/** Searches all the servers keep, through ReadLogs: what Docker has of each container, and what they kept. */
function useDeepSearch(serviceId: string, search: LogsSearch, store: LogStore): DeepSearch {
  const transport = useTransport();
  const [state, setState] = useState<Omit<DeepSearch, "loadMore">>({
    loading: false,
    reached: 0,
    lines: 0n,
    cutShort: false,
    next: "",
    sources: [],
  });
  const on = Boolean(search.deep && search.q);
  const query = search.q ?? "";
  const levelList = [...levelsOf(search)].join(",");
  const run = useRef<(token: string, keep: boolean) => void>(() => {});
  useEffect(() => {
    if (!on) return undefined;
    const abort = new AbortController();
    const client = createClient(LogService, transport);
    const wanted = levelList === "" ? [] : (levelList.split(",").map(Number) as LogLevel[]);
    const read = (pageToken: string, keep: boolean) => {
      if (!keep) store.clear();
      setState((was) => ({ ...was, loading: true, error: undefined }));
      void client
        .readLogs({ id: serviceId, query, levels: wanted, pageSize: 5000, pageToken }, { signal: abort.signal })
        .then((res) => {
          store.add(res.chunks);
          const t = res.searchedBackTo;
          setState((was) => ({
            ...was,
            loading: false,
            reached: t ? Number(t.seconds) * 1000 : was.reached,
            lines: was.lines + res.searchedLines,
            cutShort: res.searchCutShort,
            next: res.nextPageToken,
            sources: [...was.sources, ...res.sources],
          }));
        })
        .catch((err: unknown) => {
          if (!abort.signal.aborted) setState((was) => ({ ...was, loading: false, error: ConnectError.from(err) }));
        });
    };
    run.current = read;
    setState({ loading: true, reached: 0, lines: 0n, cutShort: false, next: "", sources: [] });
    read("", false);
    return () => abort.abort();
  }, [on, serviceId, query, levelList, store, transport]);
  return { ...state, loadMore: () => run.current(state.next, true) };
}

/** A way to read the lines before those loaded, at the top of the log. */
function EarlierLines({
  serviceId,
  store,
  chosen,
  search,
}: {
  serviceId: string;
  store: LogStore;
  chosen: Deploy | undefined;
  search: LogsSearch;
}) {
  const { i18n } = useLingui();
  const transport = useTransport();
  const [state, setState] = useState<{ loading: boolean; token: string; ended: boolean; error?: ConnectError }>({
    loading: false,
    token: "",
    ended: false,
  });
  useSyncExternalStore(store.subscribe, store.snapshot);
  const epochKey = `${chosen?.key ?? ""}/${search.range ?? ""}`;
  // biome-ignore lint/correctness/useExhaustiveDependencies: a new view starts over
  useEffect(() => setState({ loading: false, token: "", ended: false }), [epochKey]);
  if (store.total === 0 || state.ended || search.range || (chosen && !chosen.current)) return null;
  if (store.evicted > 0) {
    return (
      <p className="logs-held">
        <Trans>
          This view holds the newest {MAX_LINES.toLocaleString(i18n.locale)} lines, and older ones have left it to keep
          it quick.
        </Trans>
      </p>
    );
  }
  const older = () => {
    const oldest = store.oldest();
    if (!oldest) return;
    setState((was) => ({ ...was, loading: true, error: undefined }));
    void createClient(LogService, transport)
      .readLogs({
        id: serviceId,
        pageSize: OLDER_PAGE,
        pageToken: state.token,
        changeId: chosen?.change ?? "",
        // Strictly before what is here: a page token, once there is one, says so itself.
        ...(state.token ? {} : { until: timestampFromMs(oldest.ms - 1) }),
      })
      .then((res) => {
        store.addOlder(res.chunks);
        setState({ loading: false, token: res.nextPageToken, ended: res.nextPageToken === "" });
      })
      .catch((err: unknown) => setState((was) => ({ ...was, loading: false, error: ConnectError.from(err) })));
  };
  return (
    <div className="logs-earlier">
      <Button size="sm" variant="ghost" icon="chevron-up-down" loading={state.loading} onClick={older}>
        <Trans>Show earlier lines</Trans>
      </Button>
      {state.error ? <span role="alert">{sentence(state.error)}</span> : null}
    </div>
  );
}

/** The stream itself, apart so that a batch of lines redraws it alone, and not the page around it. */
const LogPane = memo(function LogPane({
  store,
  title,
  live,
  follow,
  onFollow,
  label,
  toolbar,
  top,
  stream,
  deep,
  name,
  hasSources,
  filtered,
  onClear,
  highlight,
}: {
  store: LogStore;
  title: string;
  live: boolean;
  follow: boolean;
  onFollow: (follow: boolean) => void;
  label: string;
  toolbar: ReactNode;
  top: ReactNode;
  stream: StreamState;
  deep: DeepSearch | undefined;
  name: string;
  hasSources: boolean;
  filtered: boolean;
  onClear: () => void;
  highlight: string;
}) {
  useSyncExternalStore(store.subscribe, store.snapshot);
  // A column for the source only when there is more than one.
  const showSource = label === "" && store.labels().length > 1;
  // A plain object each time, as React compares it, over what the store holds.
  const lines = useMemo(
    () => ({
      get length() {
        return store.length;
      },
      get start() {
        return store.start;
      },
      get version() {
        return store.version;
      },
      get epoch() {
        return store.epoch;
      },
      at: (p: number) => store.at(p),
      positionOf: (id: string | number) => store.positionOf(id),
    }),
    [store],
  );

  let empty: ReactNode;
  if (deep) {
    empty = deep.loading ? (
      <Trans>Searching all the servers keep</Trans>
    ) : (
      <Trans>Nothing matches, in all that the servers keep.</Trans>
    );
  } else if (store.total > 0 && filtered) {
    empty = (
      <div className="logs-empty">
        <p>
          <Trans>Nothing here matches these filters.</Trans>
        </p>
        <Button size="sm" onClick={onClear}>
          <Trans>Clear filters</Trans>
        </Button>
      </div>
    );
  } else if (stream.caughtUp && !hasSources) {
    empty = (
      <div className="logs-empty">
        <p>
          <Trans>{name} has no containers yet. Deploy it, and its logs show here as it starts.</Trans>
        </p>
      </div>
    );
  } else if (stream.caughtUp || stream.status === "done") {
    empty = (
      <div className="logs-empty">
        <p>
          {live ? (
            <Trans>No logs yet. {name} hasn't written anything; its first lines show here as it writes them.</Trans>
          ) : (
            <Trans>Nothing was written in this time.</Trans>
          )}
        </p>
      </div>
    );
  } else if (stream.error) {
    empty = (
      <p role="alert">
        {sentence(stream.error)} <Trans>Dockwell keeps trying.</Trans>
      </p>
    );
  } else {
    empty = (
      <p aria-busy="true">
        <Trans>Reading logs</Trans>
      </p>
    );
  }

  return (
    <LogStream
      className="logs-stream"
      lines={lines}
      title={title}
      live={live}
      follow={follow}
      onFollowChange={onFollow}
      height="fill"
      toolbar={toolbar}
      showSource={showSource}
      highlight={highlight}
      empty={empty}
      top={top}
    />
  );
});
