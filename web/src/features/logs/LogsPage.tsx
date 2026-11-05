// SPDX-License-Identifier: AGPL-3.0-only

import { durationFromMs, timestampFromMs } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError, createClient } from "@connectrpc/connect";
import { useQuery, useTransport } from "@connectrpc/connect-query";
import {
  LogLevel,
  LogService,
  type LogSource,
  LogSourceState,
  ServerLogState,
  type ServerLogStatus,
} from "@dockwell/api";
import {
  Button,
  cx,
  Glyph,
  Icon,
  type LogLine,
  LogStream,
  type LogStreamHandle,
  PHONE_QUERY,
  SegmentedControl,
  Sheet,
  Switch,
  TextField,
  TONE,
  useMediaQuery,
} from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import {
  type MouseEvent,
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
import { EmptyState } from "../shell/EmptyState";
import { drawEntry } from "./draw";
import { LogContext } from "./LogContext";
import { DeepWords, LogFoot } from "./LogFoot";
import { LogNote } from "./LogNote";
import { useLogsSearchButton } from "./phoneSearch";
import { type Entry, LogStore, MAX_LINES } from "./store";
import { type DeepSearch, useDeepSearch } from "./useDeepSearch";
import { type StreamState, useLogStream } from "./useLogStream";
import {
  type Deploy,
  deployRevision,
  deploysOf,
  replicaLabel,
  selectionText,
  textOf,
  withLabels,
  zoneName,
  zoneOffset,
} from "./words";
import "./LogsPage.css";

export type { Deploy };
export { deploysOf, replicaLabel };

/** The time ranges, as the URL names them. */
export type LogRange = "15m" | "1h" | "24h";

const RANGE_MS: Readonly<Record<LogRange, number>> = { "15m": 15 * 60_000, "1h": 3_600_000, "24h": 86_400_000 };

/** How many of each container's newest lines a live view starts with. */
const LIVE_TAIL = 1000;
/** How many lines a page of older lines holds. */
const OLDER_PAGE = 1000;
/** How long typing has to stop for a screen reader to hear what it found. */
const SAY_AFTER_MS = 800;
/** The longest search the API takes. */
const MAX_QUERY = 1024;

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
  /** Cuts long lines at the edge, rather than wrapping them. */
  nowrap?: boolean;
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
  const pane = useRef<LogStreamHandle>(null);

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
  const [rangeRead, setRangeRead] = useState(0);
  const request = useMemo(() => {
    if (!loaded || deployGone) return undefined;
    const base = { id: serviceId, changeId: chosenChange, revision: chosenRevision };
    if (range) return { ...base, last: durationFromMs(RANGE_MS[range]), tail: MAX_LINES, once: true };
    if (reading) return { ...base, tail: MAX_LINES, once: true };
    return { ...base, tail: LIVE_TAIL };
  }, [serviceId, range, chosenChange, chosenRevision, reading, deployGone, loaded]);
  const asking = request;
  // biome-ignore lint/correctness/useExhaustiveDependencies: a range is read again when it changes, and reaches to then
  const rangeEnd = useMemo(() => Date.now(), [range, rangeRead]);

  const stream = useLogStream(asking, store, refreshSources, rangeRead);
  const levels = useMemo(() => levelsOf({ ...(search.levels ? { levels: search.levels } : {}) }), [search.levels]);
  const deepOn = Boolean(search.deep && search.q);
  const deepLevels = useMemo(() => [...levels], [levels]);
  const deep = useDeepSearch(serviceId, deepOn, search.q ?? "", deepLevels, deepStore);
  const active = deepOn ? deepStore : store;

  // How entries are drawn: their time, their source's name and the words of a note.
  const [sourceMap, setSourceMap] = useState<ReadonlyMap<string, LogSource>>(new Map());
  useEffect(() => {
    const merged = new Map<string, LogSource>();
    for (const s of allSources ?? []) merged.set(s.id, s);
    for (const [id, s] of stream.sources) merged.set(id, s);
    for (const s of deep.sources) merged.set(s.id, s);
    const read = new Map(stream.sources);
    for (const s of deep.sources) read.set(s.id, s);
    const labelled = withLabels(merged, read);
    for (const [id, s] of labelled) {
      store.setSource(id, replicaLabel(s), `${s.serverId}/${s.replica}`);
      deepStore.setSource(id, replicaLabel(s), `${s.serverId}/${s.replica}`);
    }
    setSourceMap(labelled);
  }, [allSources, stream.sources, deep.sources, store, deepStore]);

  useEffect(() => {
    const draw = (e: Entry): LogLine => drawEntry(e, sourceMap, i18n);
    store.setDraw(draw);
    deepStore.setDraw(draw);
  }, [sourceMap, store, deepStore, i18n]);

  // Filters, from the URL. Typing in the search waits a moment for the next key,
  // and the URL follows. A search of all the servers keep waits for Enter.
  // A name is only so for as long as what it picks out is here: a deploy moves on from it.
  const label = [...sourceMap.values()].some((s) => replicaLabel(s) === search.replica) ? (search.replica ?? "") : "";
  const [typed, setTyped] = useState(search.q ?? "");
  useEffect(() => setTyped(search.q ?? ""), [search.q]);
  const query = search.deep ? "" : (search.q ?? "");
  useEffect(() => {
    active.setFilter({ levels: search.deep ? new Set() : levels, label, query });
  }, [active, levels, label, query, search.deep]);
  const debounce = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);
  const latest = useRef(search);
  latest.current = search;
  const commit = useCallback(
    (text: string) => {
      clearTimeout(debounce.current);
      const { q: _q, ...rest } = latest.current;
      onSearch(text ? { ...rest, q: text } : rest);
    },
    [onSearch],
  );
  const searchFor = (text: string) => {
    setTyped(text);
    clearTimeout(debounce.current);
    if (search.deep) return;
    debounce.current = setTimeout(() => commit(text), 120);
  };
  useEffect(() => () => clearTimeout(debounce.current), []);
  const searchDeep = () => {
    clearTimeout(debounce.current);
    onSearch({ ...latest.current, q: typed, deep: true });
  };
  const setScope = (scope: string) => {
    clearTimeout(debounce.current);
    const { q: _q, deep: _d, ...rest } = latest.current;
    const q = typed ? { q: typed } : {};
    onSearch(scope === "all" ? { ...rest, ...q, deep: true } : { ...rest, ...q });
  };
  const stale = Boolean(search.deep && typed && typed !== (search.q ?? ""));

  // Stepping through what a search found, and the errors, and what a reader is on.
  const [current, setCurrent] = useState<number | undefined>(undefined);
  const [step, setStep] = useState<number | undefined>(undefined);
  const [errorAt, setErrorAt] = useState<number | undefined>(undefined);
  const stepAfter = useRef<1 | -1 | undefined>(undefined);
  // Going through a search's matches stops following. Once the search is cleared,
  // nothing is being gone through, so it goes back to following.
  const followAfterSearch = useRef(false);
  const following = useRef(true);
  const stepPosition = useRef<number | undefined>(undefined);
  // biome-ignore lint/correctness/useExhaustiveDependencies: what was found has changed, so stepping starts over
  useEffect(() => {
    setStep(undefined);
    stepPosition.current = undefined;
    setCurrent(undefined);
    setErrorAt(undefined);
  }, [active, search.q, search.levels, label]);
  const stepThrough = useCallback(
    (by: 1 | -1) => {
      const start = active.start;
      const end = start + active.length;
      const from = stepPosition.current;
      // The first step goes to the newest, near where a reader is, and on up the log.
      const next =
        (from === undefined ? active.lineFrom(end - 1, -1) : active.lineFrom(from + by, by)) ??
        active.lineFrom(by > 0 ? start : end - 1, by);
      if (next === undefined) return;
      let number = 0;
      for (let p = start; p <= next; p++) {
        const e = active.entryAt(p);
        if (e && !e.gap && !e.started && e.left === undefined) number++;
      }
      stepPosition.current = next;
      if (following.current) followAfterSearch.current = true;
      setStep(number);
      setCurrent(active.entryAt(next)?.id);
      pane.current?.show(next);
    },
    [active],
  );
  useEffect(() => {
    if (stepAfter.current === undefined || !search.q || search.q !== typed) return;
    const by = stepAfter.current;
    stepAfter.current = undefined;
    stepThrough(by);
  }, [search.q, typed, stepThrough]);
  const onSearchKey = (e: React.KeyboardEvent<HTMLInputElement>) => {
    if (e.key === "Escape") {
      if (!typed) {
        e.currentTarget.blur();
        return;
      }
      e.preventDefault();
      setTyped("");
      commit("");
      return;
    }
    if (e.key !== "Enter") return;
    e.preventDefault();
    const by = e.shiftKey ? 1 : -1;
    if (!typed) return;
    if (typed !== (search.q ?? "")) {
      if (!search.deep) stepAfter.current = by;
      commit(typed);
      return;
    }
    stepThrough(by);
  };
  const clearSearch = () => {
    setTyped("");
    commit("");
  };

  const showError = (index: number) => {
    const e = active.errorAt(index);
    if (!e) return;
    const p = active.positionOfEntry(e);
    if (p === undefined) return;
    setErrorAt(index);
    setCurrent(e.id);
    pane.current?.show(p);
  };

  // The log takes what is left of the screen, so what scrolls is the log, and
  // what sits above it stays put.
  const section = useRef<HTMLElement>(null);
  useLayoutEffect(() => {
    const el = section.current;
    if (!el) return undefined;
    const measure = () => {
      const top = el.getBoundingClientRect().top + window.scrollY;
      const below = phone ? 0 : 24;
      el.style.setProperty("--logs-height", `${Math.max(phone ? 240 : 360, window.innerHeight - top - below)}px`);
    };
    measure();
    window.addEventListener("resize", measure);
    return () => window.removeEventListener("resize", measure);
  });

  const replicas = useMemo(() => {
    const seen = new Set<string>();
    for (const s of chosen ? chosen.sources : sourceMap.values())
      if (chosen || (s.state !== LogSourceState.KEPT && s.state !== LogSourceState.GONE)) seen.add(replicaLabel(s));
    return [...seen].sort();
  }, [chosen, sourceMap]);
  // How many run now, which is what a head can say: what this view has read of those
  // that have gone is another matter, and a service has one but for a rolling deploy.
  const runningNow = reading ? 0 : [...sourceMap.values()].filter((s) => s.state === LogSourceState.RUNNING).length;

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
  const setWrap = (wrap: boolean) => {
    const { nowrap: _n, ...rest } = search;
    onSearch(wrap ? rest : { ...rest, nowrap: true });
  };
  const clearFilters = () => {
    const { levels: _l, replica: _r, q: _q, deep: _d, ...rest } = search;
    setTyped("");
    onSearch(rest);
  };

  // Following says lines can come. With no server to read from, the note above says why they can't.
  const statuses = [...stream.servers.values()];
  const noneReading =
    statuses.length > 0 &&
    statuses.every((s) => s.state === ServerLogState.OFFLINE || s.state === ServerLogState.UNAVAILABLE);
  const live = !deepOn && !search.range && !reading && stream.status === "live" && !noneReading;
  // A failed deploy opens at what went wrong, not at its last line.
  const opensAtError = Boolean(chosen?.failed && !chosen.current && !search.q);
  const [follow, setFollow] = useState(true);
  following.current = follow;
  useEffect(() => {
    if (!search.q && followAfterSearch.current) {
      followAfterSearch.current = false;
      setFollow(true);
    }
  }, [search.q]);
  // biome-ignore lint/correctness/useExhaustiveDependencies: a new view of the logs starts at its end
  useEffect(() => setFollow(!opensAtError), [asking, opensAtError]);
  useLayoutEffect(() => store.setHolding(live && !follow), [store, live, follow]);
  const opened = useRef<unknown>(undefined);
  const showErrorRef = useRef(showError);
  showErrorRef.current = showError;
  useEffect(() => {
    if (!opensAtError || stream.status !== "done" || opened.current === asking) return;
    opened.current = asking;
    if (active.errors > 0) showErrorRef.current(0);
  }, [opensAtError, stream.status, asking, active]);

  // What the servers say of themselves: at most one note, what matters most first.
  const heard = statuses.length > 0 ? statuses : (sources.data?.servers ?? []);
  const down = heard.filter((s) => s.state === ServerLogState.OFFLINE || s.state === ServerLogState.UNAVAILABLE);
  const stopped =
    !reading && deploys.flatMap((d) => (d.current ? d.sources : [])).filter((s) => s.state === LogSourceState.STOPPED);
  const unbounded = (allSources ?? []).filter((s) => s.unbounded && s.state !== LogSourceState.KEPT);
  const logErrors = [...sourceMap.values()].filter((s) => s.logError);

  // A phone's header has the button that opens the search.
  const [searching, setSearching] = useState(false);
  const opening = useLogsSearchButton();
  useEffect(() => {
    if (!opening) return undefined;
    opening.current = () => setSearching(true);
    return () => {
      opening.current = null;
    };
  }, [opening]);
  const [viewOpen, setViewOpen] = useState(false);
  const [context, setContext] = useState<Entry | undefined>(undefined);

  const refused = stream.error?.code === Code.PermissionDenied || sources.error?.code === Code.PermissionDenied;
  const missing = stream.error?.code === Code.NotFound || sources.error?.code === Code.NotFound;
  if (refused) {
    return (
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
    );
  }
  if (missing) {
    return (
      <EmptyState icon="service" title={t`This app isn't here`}>
        <p>
          <Trans>It may have been deleted. Dockwell has no logs under this address.</Trans>
        </p>
      </EmptyState>
    );
  }
  if (deployGone) {
    const { change: _c, revision: _r, ...rest } = search;
    return (
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
            replaced container of each app, for 30 days, within 256 MB in all, and those of a deleted app for 7.
          </Trans>
        </p>
      </EmptyState>
    );
  }

  const name = service?.name ?? "";
  const environment = service?.environment ?? "";
  const levelFiltered = levels.size > 0;
  const showsErrors = levels.size === 0 || levels.has(LogLevel.ERROR);
  const currentDeploy = deploys.find((d) => d.current);
  const failedDeploy = deploys.find((d) => d.failed);
  const title: ReactNode = phone ? (
    reading && chosen ? (
      <DeployLabel deploy={chosen} />
    ) : (
      environment.charAt(0).toUpperCase() + environment.slice(1)
    )
  ) : (
    <>
      {name}
      {reading && chosen ? (
        <>
          {" · "}
          <DeployLabel deploy={chosen} />
        </>
      ) : environment ? (
        ` · ${environment}`
      ) : null}
      {runningNow > 1 ? ` · ${plural(runningNow, { one: "# container", other: "# containers" })}` : ""}
    </>
  );
  const download = () =>
    saveText(active, `${name || "logs"}-${new Date().toISOString().replaceAll(":", "-")}.log`, sourceMap);
  const rangeLabel = range ? { "15m": t`Last 15 min`, "1h": t`Last 1 h`, "24h": t`Last 24 h` }[range] : "";
  const showScope = !reading;
  const replicasControl =
    replicas.length > 4 ? (
      <label className="dw-input dw-input-sm logs-select">
        <span className="dw-sr">{t`Container`}</span>
        <select value={label || "all"} onChange={(e) => setReplica(e.target.value)} className="dw-input-control">
          <option value="all">{t`All containers`}</option>
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
        label={t`Container`}
        value={label || "all"}
        onChange={setReplica}
        options={[
          { value: "all", label: t`All containers` },
          ...replicas.map((r) => ({ value: r, label: <span className="logs-mono">{r}</span> })),
        ]}
      />
    ) : null;
  const rangeControl =
    !reading && !search.deep ? (
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
    ) : null;
  const chooser =
    deploys.length > 1 ? (
      <DeployChooser deploys={deploys} chosen={chosen} onChange={setDeploy} noCurrent={noCurrent} />
    ) : null;
  const searchField = (
    <div className="logs-search">
      <TextField
        size={phone ? "md" : "sm"}
        icon="search"
        mono
        value={typed}
        maxLength={MAX_QUERY}
        autoFocus={phone && searching}
        onChange={(e) => searchFor(e.target.value)}
        onKeyDown={onSearchKey}
        placeholder={t`Search lines`}
        aria-label={t`Search logs`}
        title={t`Enter goes to the next match up the log, and Shift+Enter down it. Escape clears the search.`}
        action={
          typed ? (
            <span className="logs-search-end">
              {!search.deep && search.q ? (
                <span className="logs-search-count">
                  {step !== undefined
                    ? t`${step.toLocaleString(i18n.locale)} of ${active.matches.toLocaleString(i18n.locale)}`
                    : active.matches.toLocaleString(i18n.locale)}
                </span>
              ) : null}
              {phone ? null : (
                <Button size="sm" variant="ghost" icon="x" aria-label={t`Clear the search`} onClick={clearSearch} />
              )}
            </span>
          ) : undefined
        }
      />
    </div>
  );
  const scopeControl = showScope ? (
    <SegmentedControl
      size="sm"
      label={t`What to search`}
      value={search.deep ? "all" : "loaded"}
      onChange={setScope}
      className="logs-scope"
      options={[
        { value: "loaded", label: t`Lines loaded` },
        { value: "all", label: t`All the servers keep` },
      ]}
    />
  ) : null;

  return (
    <section ref={section} className={cx("logs", phone && "is-phone")} aria-label={t`Logs`}>
      <LogNote
        name={name}
        stream={stream}
        down={down}
        stopped={stopped === false ? [] : stopped}
        logErrors={logErrors}
        unbounded={unbounded}
        chosen={chosen}
        rolledBackTo={chosen?.failed && currentDeploy ? deployRevision(currentDeploy) : undefined}
        noCurrent={noCurrent && !asked}
        locale={i18n.locale}
      />
      <div className="logs-tools" role="toolbar" aria-label={t`Log filters`}>
        {phone ? (
          searching || typed ? (
            <>
              {searchField}
              <Button
                icon="x"
                variant="ghost"
                aria-label={t`Close the search`}
                className="logs-search-close"
                onClick={() => {
                  clearSearch();
                  setSearching(false);
                }}
              />
              {scopeControl}
            </>
          ) : (
            <PhoneFilters
              levels={levels}
              onLevels={setLevels}
              replicas={replicas}
              replica={label}
              onReplica={setReplica}
              view={
                <button type="button" className="logs-chip" aria-haspopup="dialog" onClick={() => setViewOpen(true)}>
                  <span className="logs-chip-face">
                    <span>{chosen && reading ? deployRevision(chosen) : rangeLabel || t`Live`}</span>
                    <Icon name="chevron-down" size={14} />
                  </span>
                </button>
              }
            />
          )
        ) : (
          <>
            {searchField}
            {scopeControl}
            <LevelToggles levels={levels} onLevels={setLevels} />
            <span className="logs-fill" />
            {replicasControl}
            {rangeControl}
          </>
        )}
      </div>
      {phone ? null : chooser}
      <LogPane
        paneRef={pane}
        store={active}
        title={title}
        live={live}
        follow={follow}
        onFollow={setFollow}
        label={label}
        wrap={!search.nowrap}
        onWrap={setWrap}
        phone={phone}
        locale={i18n.locale}
        name={name}
        fullSources={sourceMap}
        current={current}
        searching={Boolean(search.q)}
        onContext={setContext}
        onDownload={download}
        onRefresh={range ? () => setRangeRead((n) => n + 1) : undefined}
        errorAt={showsErrors && !search.q ? errorAt : undefined}
        showErrors={showsErrors && !search.q}
        onError={showError}
        top={
          search.deep ? (
            deep.next ? (
              <div className="logs-earlier">
                <Button size="sm" variant="ghost" icon="chevron-up" loading={deep.loading} onClick={deep.loadMore}>
                  <Trans>Show older matches</Trans>
                </Button>
              </div>
            ) : undefined
          ) : !search.q ? (
            <EarlierLines serviceId={serviceId} store={active} chosen={chosen} search={search} />
          ) : undefined
        }
        stream={stream}
        deep={deepOn ? deep : undefined}
        deepPending={Boolean(search.deep) && !deepOn}
        query={query}
        down={down}
        failedDeploy={failedDeploy && !reading ? failedDeploy : undefined}
        onDeploy={setDeploy}
        onSearchAll={searchDeep}
        hasSources={(allSources?.length ?? 0) > 0}
        filtered={levelFiltered || label !== ""}
        onClear={clearFilters}
        highlight={search.deep ? (search.q ?? "") : query}
        foot={
          <LogFoot
            store={active}
            deep={deepOn ? deep : undefined}
            query={query}
            stale={stale}
            live={live}
            following={follow}
            rangeLabel={rangeLabel}
            rangeEnd={rangeEnd}
            levelFiltered={levelFiltered}
            phone={phone}
            onPause={() => setFollow(false)}
            onLastSkip={() => {
              const e = active.lastSkip;
              const p = e ? active.positionOfEntry(e) : undefined;
              if (p !== undefined) pane.current?.show(p);
            }}
          />
        }
      />
      <SearchSaid store={active} deep={deepOn ? deep : undefined} typed={typed} />
      {phone ? (
        <div className="logs-bar">
          <Button icon="download" onClick={download} disabled={active.length === 0}>
            <Trans>Download</Trans>
          </Button>
          <CopyLines store={active} sources={sourceMap} wide />
        </div>
      ) : null}
      {phone && viewOpen ? (
        <Sheet open onClose={() => setViewOpen(false)} title={t`What to show`} className="logs-view">
          <div className="logs-view-body">
            {rangeControl ? (
              <div className="logs-view-part">
                <span className="logs-view-label">
                  <Trans>Time</Trans>
                </span>
                {rangeControl}
              </div>
            ) : null}
            {chooser ? <div className="logs-view-part">{chooser}</div> : null}
            <div className="logs-view-part">
              <Switch checked={!search.nowrap} onChange={setWrap} label={t`Wrap long lines`} />
            </div>
          </div>
        </Sheet>
      ) : null}
      {context ? (
        <LogContext serviceId={serviceId} entry={context} sources={sourceMap} onClose={() => setContext(undefined)} />
      ) : null}
    </section>
  );
}

/** A deploy as a person reads it: its revision in mono, and how it ended in a glyph and a word. */
function DeployLabel({ deploy }: { deploy: Deploy }) {
  const revision = deployRevision(deploy);
  return (
    <span className="logs-deploy">
      <code>{revision}</code>
      {deploy.failed ? (
        <span className="logs-deploy-state is-critical">
          <Glyph shape={TONE.critical.shape} size={8} />
          <Trans>failed</Trans>
        </span>
      ) : !deploy.current ? (
        <span className="logs-deploy-state">
          <Trans>replaced</Trans>
        </span>
      ) : null}
    </span>
  );
}

function saveText(store: LogStore, name: string, sources: ReadonlyMap<string, LogSource>) {
  if (store.length === 0) return;
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
  useSyncExternalStore(store.subscribe, store.snapshot);
  const [copied, setCopied] = useState(false);
  useEffect(() => {
    if (!copied) return undefined;
    const timer = setTimeout(() => setCopied(false), 2000);
    return () => clearTimeout(timer);
  }, [copied]);
  const nothing = store.length === 0;
  const copy = async () => {
    if (nothing) return;
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
        <Button icon={copied ? "check" : "copy"} onClick={() => void copy()} disabled={nothing}>
          {copied ? <Trans>Copied</Trans> : <Trans>Copy</Trans>}
        </Button>
      ) : (
        <Button
          size="sm"
          variant="ghost"
          icon={copied ? "check" : "copy"}
          aria-label={t`Copy these lines`}
          title={nothing ? t`There are no lines to copy` : t`Copy these lines`}
          onClick={() => void copy()}
          disabled={nothing}
        />
      )}
      <span className="dw-sr" aria-live="polite">
        {copied ? t`Copied the lines shown` : ""}
      </span>
    </>
  );
}

/** The one line's copy button, and what else can be done with one line. */
function LineActions({
  line,
  position,
  store,
  sources,
  focusable,
  searching,
  onContext,
}: {
  line: LogLine;
  position: number;
  store: LogStore;
  sources: ReadonlyMap<string, LogSource>;
  focusable: boolean;
  searching: boolean;
  onContext: (entry: Entry) => void;
}) {
  const { t } = useLingui();
  const [copied, setCopied] = useState(false);
  useEffect(() => {
    if (!copied) return undefined;
    const timer = setTimeout(() => setCopied(false), 2000);
    return () => clearTimeout(timer);
  }, [copied]);
  const entry = store.entryAt(position);
  if (!entry || line.notice) return null;
  const tabIndex = focusable ? 0 : -1;
  return (
    <>
      {searching ? (
        <Button size="sm" variant="ghost" tabIndex={tabIndex} onClick={() => onContext(entry)}>
          <Trans>Show in context</Trans>
        </Button>
      ) : null}
      <Button
        size="sm"
        variant="ghost"
        icon={copied ? "check" : "copy"}
        tabIndex={tabIndex}
        aria-label={copied ? t`Copied this line` : t`Copy this line`}
        title={t`Copy this line`}
        onClick={() => {
          void navigator.clipboard
            .writeText(textOf(entry, sources))
            .then(() => setCopied(true))
            .catch(() => {});
        }}
      />
    </>
  );
}

/** The levels as the wide layout has them: each one on or off, and one by itself on asking. */
function LevelToggles({
  levels,
  onLevels,
}: {
  levels: ReadonlySet<LogLevel>;
  onLevels: (next: ReadonlySet<LogLevel>) => void;
}) {
  const { t } = useLingui();
  const on = (l: LogLevel) => levels.size === 0 || levels.has(l);
  const toggle = (l: LogLevel, e: MouseEvent) => {
    if (e.shiftKey) {
      onLevels(new Set([l]));
      return;
    }
    const next = new Set(levels.size === 0 ? FILTER_LEVELS : levels);
    if (!next.has(l)) next.add(l);
    // The last one stays on: all off shows nothing, and all on isn't what was asked.
    else if (next.size > 1) next.delete(l);
    onLevels(next);
  };
  const props = (l: LogLevel) => ({
    pressed: on(l),
    onClick: (e: MouseEvent) => toggle(l, e),
    onDoubleClick: () => onLevels(new Set([l])),
  });
  return (
    // biome-ignore lint/a11y/useSemanticElements: a fieldset can't be laid out as this row of chips
    <div
      className="logs-levels"
      role="group"
      aria-label={t`Levels`}
      title={t`Levels are read from each line's text. A line that names none counts as info. Double-click or Shift-click one to show it alone.`}
    >
      <LevelChip {...props(LogLevel.INFO)}>
        <Trans>Info</Trans>
      </LevelChip>
      <LevelChip {...props(LogLevel.WARN)} level="warning">
        <Trans>Warnings</Trans>
      </LevelChip>
      <LevelChip {...props(LogLevel.ERROR)} level="critical">
        <Trans>Errors</Trans>
      </LevelChip>
    </div>
  );
}

function LevelChip({
  pressed,
  onClick,
  onDoubleClick,
  level,
  mono,
  children,
}: {
  pressed: boolean;
  onClick: (e: MouseEvent) => void;
  onDoubleClick?: () => void;
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
      onDoubleClick={onDoubleClick}
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
  view,
}: {
  levels: ReadonlySet<LogLevel>;
  onLevels: (next: ReadonlySet<LogLevel>) => void;
  replicas: readonly string[];
  replica: string;
  onReplica: (value: string) => void;
  view: ReactNode;
}) {
  const { t } = useLingui();
  const only = (l: LogLevel) => levels.size === 1 && levels.has(l);
  const pick = (l: LogLevel) => onLevels(only(l) ? new Set() : new Set([l]));
  return (
    // biome-ignore lint/a11y/useSemanticElements: a fieldset can't scroll as this row of chips does
    <div className="logs-chips" role="group" aria-label={t`Filter log lines`}>
      {view}
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
  const { t } = useLingui();
  const current = deploys.filter((d) => d.current);
  // Two to five, as the control takes; a server keeps few.
  const others = deploys.filter((d) => !d.current).slice(0, noCurrent ? 5 : 4);
  const now = current[0];
  const options = [
    ...(noCurrent
      ? []
      : [
          {
            value: "current",
            label: now ? (
              <span className="logs-deploy">
                <Trans>Running now</Trans> · <code>{deployRevision(now)}</code>
              </span>
            ) : (
              t`Current`
            ),
          },
        ]),
    ...others.map((d) => ({ value: d.key, label: <DeployLabel deploy={d} /> })),
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
 * What a search found, for a screen reader: once, after typing stops, as the running
 * count would otherwise be read out at every key and every batch of lines.
 */
function SearchSaid({ store, deep, typed }: { store: LogStore; deep: DeepSearch | undefined; typed: string }) {
  const { i18n } = useLingui();
  const [said, setSaid] = useState("");
  const latest = useRef({ store, deep, typed });
  latest.current = { store, deep, typed };
  const loading = deep?.loading ?? false;
  // biome-ignore lint/correctness/useExhaustiveDependencies: each is a reason to say it again, once it has settled
  useEffect(() => {
    const timer = setTimeout(() => {
      const now = latest.current;
      if (!now.typed || now.deep?.loading) {
        setSaid("");
        return;
      }
      const count = now.store.matches.toLocaleString(i18n.locale);
      setSaid(
        plural(now.store.matches, {
          one: `${count} line matches.`,
          other: `${count} lines match.`,
        }),
      );
    }, SAY_AFTER_MS);
    return () => clearTimeout(timer);
  }, [typed, loading, i18n]);
  return (
    <span className="dw-sr" aria-live="polite">
      {said}
    </span>
  );
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
  if (store.total === 0 || search.range || (chosen && !chosen.current)) return null;
  if (state.ended) {
    return (
      <p className="logs-held">
        <Trans>Start of the log</Trans>
      </p>
    );
  }
  if (store.evicted > 0) {
    const most = MAX_LINES.toLocaleString(i18n.locale);
    return (
      <p className="logs-held">
        <Trans>
          This view holds the newest {most} lines. Read older ones with <code>dockwell logs read</code>.
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
      <Button size="sm" variant="ghost" icon="chevron-up" loading={state.loading} onClick={older}>
        <Trans>Show earlier lines</Trans>
      </Button>
      {state.error ? <span role="alert">{sentence(state.error)}</span> : null}
    </div>
  );
}

/** Going through the errors, with how many there are. */
function ErrorSteps({
  store,
  at,
  onError,
}: {
  store: LogStore;
  at: number | undefined;
  onError: (index: number) => void;
}) {
  const { t, i18n } = useLingui();
  const errors = store.errors;
  if (errors === 0) return null;
  const count = errors.toLocaleString(i18n.locale);
  const number = at === undefined ? undefined : (at + 1).toLocaleString(i18n.locale);
  const prev = at === undefined ? errors - 1 : (at - 1 + errors) % errors;
  const next = at === undefined ? 0 : (at + 1) % errors;
  return (
    // biome-ignore lint/a11y/useSemanticElements: a fieldset can't sit in this row of tools
    <span className="logs-errors" role="group" aria-label={t`Errors in this log`}>
      <span className="logs-errors-count">
        <Glyph shape={TONE.critical.shape} size={8} />
        {number ? t`${number} of ${count}` : plural(errors, { one: `${count} error`, other: `${count} errors` })}
      </span>
      <Button
        size="sm"
        variant="ghost"
        icon="chevron-up"
        aria-label={t`Previous error`}
        onClick={() => onError(prev)}
      />
      <Button size="sm" variant="ghost" icon="chevron-down" aria-label={t`Next error`} onClick={() => onError(next)} />
    </span>
  );
}

/** The control plane's message as a sentence, leaving a machine's name as written if one comes first. */
function sentenceKeeping(error: ConnectError, names: readonly string[]): string {
  const text = (error.rawMessage || error.message).trim();
  if (!names.some((name) => text.startsWith(name))) return sentence(error);
  return /[.!?]$/.test(text) ? text : `${text}.`;
}

/** What a log with nothing to show says, of why and what next. */
function emptyOf({
  deep,
  deepPending,
  store,
  query,
  filtered,
  name,
  down,
  hasSources,
  stream,
  live,
  failedDeploy,
  onDeploy,
  onSearchAll,
  onClear,
  highlight,
  locale,
  sources,
}: {
  sources: ReadonlyMap<string, LogSource>;
  deep: DeepSearch | undefined;
  deepPending: boolean;
  store: LogStore;
  query: string;
  filtered: boolean;
  name: string;
  down: readonly ServerLogStatus[];
  hasSources: boolean;
  stream: StreamState;
  live: boolean;
  failedDeploy: Deploy | undefined;
  onDeploy: (key: string) => void;
  onSearchAll: () => void;
  onClear: () => void;
  highlight: string;
  locale: string;
}): ReactNode {
  const servers = down.map((s) => s.serverName).join(", ");
  const names = [...new Set([...sources.values()].map((s) => s.serverName))];
  if (deepPending) {
    return (
      <EmptyState icon="search" title={<Trans>Search all the servers keep</Trans>}>
        <p>
          <Trans>Type what to find, and press Enter. It reads each container's log, at most for 30 s.</Trans>
        </p>
      </EmptyState>
    );
  }
  if (deep) {
    if (deep.loading) {
      return (
        <EmptyState icon="search" title={<Trans>Searching all the servers keep</Trans>}>
          <p aria-busy="true" className="logs-working">
            <Icon name="refresh" size={14} className="dw-spin" />
            <Trans>It reads each container's log, at most for 30 s.</Trans>
          </p>
        </EmptyState>
      );
    }
    if (deep.error) {
      return (
        <EmptyState
          icon="alert"
          title={<Trans>The search failed</Trans>}
          action={
            <Button onClick={deep.retry}>
              <Trans>Try again</Trans>
            </Button>
          }
        >
          <p>{sentenceKeeping(deep.error, names)}</p>
        </EmptyState>
      );
    }
    if (deep.reached === 0 && deep.servers.length > 0) {
      const first = deep.servers.map((s) => s.serverName).join(", ");
      return (
        <EmptyState icon="alert" title={<Trans>{first} couldn't be searched</Trans>}>
          <p>
            <Trans>Its agent isn't connected, so its lines weren't read. Check the server, then search again.</Trans>
          </p>
        </EmptyState>
      );
    }
    return (
      <EmptyState icon="search" title={<Trans>Nothing in all the servers keep has "{highlight}".</Trans>}>
        <p>
          {deep.reached > 0 ? (
            <DeepWords store={store} deep={deep} />
          ) : (
            <Trans>The servers have no lines of this app.</Trans>
          )}
        </p>
      </EmptyState>
    );
  }
  if (store.total > 0 && query) {
    const total = store.total.toLocaleString(locale);
    return (
      <EmptyState
        icon="search"
        title={
          <Trans>
            Nothing in the {total} lines loaded has "{query}".
          </Trans>
        }
        action={
          <Button onClick={onSearchAll}>
            <Trans>Search all the servers keep</Trans>
          </Button>
        }
      >
        <p>
          <Trans>What the servers keep goes back further than what is loaded here.</Trans>
        </p>
      </EmptyState>
    );
  }
  if (store.total > 0 && filtered) {
    return (
      <EmptyState
        icon="filter"
        title={<Trans>Nothing here matches these filters.</Trans>}
        action={
          <Button onClick={onClear}>
            <Trans>Clear filters</Trans>
          </Button>
        }
      >
        <p>
          <Trans>Levels are read from each line's text. A line that names none counts as info.</Trans>
        </p>
      </EmptyState>
    );
  }
  if (down.length > 0) {
    return (
      <EmptyState icon="server" title={<Trans>Nothing to show until {servers} is back</Trans>}>
        <p>
          <Trans>Its logs show here once they can be read.</Trans>
        </p>
      </EmptyState>
    );
  }
  if (stream.caughtUp && !hasSources) {
    return (
      <EmptyState icon="terminal" title={<Trans>{name} has no containers yet</Trans>}>
        <p>
          <Trans>Deploy it, and its logs show here as it starts.</Trans>
        </p>
      </EmptyState>
    );
  }
  if (stream.caughtUp || stream.status === "done") {
    return live ? (
      <EmptyState
        icon="terminal"
        title={<Trans>No logs yet</Trans>}
        action={
          failedDeploy ? (
            <Button onClick={() => onDeploy(failedDeploy.key)}>
              <Trans>Read the failed deploy's logs</Trans>
            </Button>
          ) : undefined
        }
      >
        <p>
          <Trans>{name} hasn't written anything. Its first lines show here as it writes them.</Trans>
        </p>
      </EmptyState>
    ) : (
      <EmptyState icon="clock" title={<Trans>Nothing was written in this time</Trans>}>
        <p>
          <Trans>Choose a longer range, or follow what it writes next.</Trans>
        </p>
      </EmptyState>
    );
  }
  if (stream.error) {
    return (
      <EmptyState icon="alert" title={<Trans>Can't read these logs now</Trans>}>
        <p role="alert">
          {sentence(stream.error)} <Trans>Dockwell keeps trying.</Trans>
        </p>
      </EmptyState>
    );
  }
  return (
    <EmptyState icon="terminal" title={<Trans>Reading logs</Trans>}>
      <p aria-busy="true" className="logs-working">
        <Icon name="refresh" size={14} className="dw-spin" />
      </p>
    </EmptyState>
  );
}

/** The stream itself, apart so that a batch of lines redraws it alone, and not the page around it. */
const LogPane = memo(function LogPane({
  paneRef,
  store,
  title,
  live,
  follow,
  onFollow,
  label,
  wrap,
  onWrap,
  phone,
  locale,
  name,
  fullSources,
  current,
  searching,
  onContext,
  onDownload,
  onRefresh,
  errorAt,
  showErrors,
  onError,
  top,
  stream,
  deep,
  deepPending,
  query,
  down,
  failedDeploy,
  onDeploy,
  onSearchAll,
  hasSources,
  filtered,
  onClear,
  highlight,
  foot,
}: {
  paneRef: React.RefObject<LogStreamHandle | null>;
  store: LogStore;
  title: ReactNode;
  live: boolean;
  follow: boolean;
  onFollow: (follow: boolean) => void;
  label: string;
  wrap: boolean;
  onWrap: (wrap: boolean) => void;
  phone: boolean;
  locale: string;
  name: string;
  fullSources: ReadonlyMap<string, LogSource>;
  current: number | undefined;
  searching: boolean;
  onContext: (entry: Entry) => void;
  onDownload: () => void;
  onRefresh: (() => void) | undefined;
  errorAt: number | undefined;
  showErrors: boolean;
  onError: (index: number) => void;
  top: ReactNode;
  stream: StreamState;
  deep: DeepSearch | undefined;
  deepPending: boolean;
  query: string;
  down: readonly ServerLogStatus[];
  failedDeploy: Deploy | undefined;
  onDeploy: (key: string) => void;
  onSearchAll: () => void;
  hasSources: boolean;
  filtered: boolean;
  onClear: () => void;
  highlight: string;
  foot: ReactNode;
}) {
  const { t } = useLingui();
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
      get arrived() {
        return store.arrived;
      },
      at: (p: number) => store.at(p),
      positionOf: (id: string | number) => store.positionOf(id),
    }),
    [store],
  );

  const actions = useCallback(
    (line: LogLine, position: number) => (
      <LineActions
        line={line}
        position={position}
        store={store}
        sources={fullSources}
        focusable={current !== undefined && line.id === current}
        searching={searching}
        onContext={onContext}
      />
    ),
    [store, fullSources, current, searching, onContext],
  );

  const empty = emptyOf({
    deep,
    deepPending,
    store,
    query,
    filtered,
    name,
    down,
    hasSources,
    stream,
    live,
    failedDeploy,
    onDeploy,
    onSearchAll,
    onClear,
    highlight,
    locale,
    sources: fullSources,
  });

  const nothing = store.length === 0;
  const toolbar = (
    <>
      {showErrors ? <ErrorSteps store={store} at={errorAt} onError={onError} /> : null}
      {phone ? null : (
        <>
          {onRefresh ? (
            <Button
              size="sm"
              variant="ghost"
              icon="refresh"
              aria-label={t`Read this range again`}
              title={t`Read this range again`}
              onClick={onRefresh}
            />
          ) : null}
          <Button
            size="sm"
            variant="ghost"
            icon="wrap-text"
            aria-pressed={wrap}
            aria-label={t`Wrap long lines`}
            title={t`Wrap long lines`}
            className={cx("logs-wrap", wrap && "is-on")}
            onClick={() => onWrap(!wrap)}
          />
          <CopyLines store={store} sources={fullSources} />
          <Button
            size="sm"
            variant="ghost"
            icon="download"
            aria-label={t`Download these lines`}
            title={nothing ? t`There are no lines to download` : t`Download these lines`}
            onClick={onDownload}
            disabled={nothing}
          />
        </>
      )}
    </>
  );

  return (
    <LogStream
      ref={paneRef}
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
      foot={foot}
      meta={<span title={zoneName()}>{zoneOffset()}</span>}
      wrap={wrap}
      current={current}
      actions={actions}
      copyLine={selectionText}
      holdFollow={!phone}
    />
  );
});
