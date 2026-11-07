// SPDX-License-Identifier: AGPL-3.0-only

import { create, type MessageInitShape, toJsonString } from "@bufbuild/protobuf";
import { timestampDate, timestampFromDate } from "@bufbuild/protobuf/wkt";
import { createClient } from "@connectrpc/connect";
import { useInfiniteQuery, useTransport } from "@connectrpc/connect-query";
import {
  ActorKind,
  AuditActor,
  type AuditEvent,
  AuditEventSchema,
  AuditService,
  ListAuditEventsRequestSchema,
  Surface,
} from "@dockwell/api";
import {
  Button,
  Callout,
  cx,
  Icon,
  type IconName,
  PHONE_QUERY,
  SelectField,
  TextField,
  useMediaQuery,
} from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link } from "@tanstack/react-router";
import { Fragment, useEffect, useId, useMemo, useState } from "react";

import { sentence } from "../../lib/errors";
import { formatClock, formatDateTime, formatWeekday } from "../../lib/format";
import { useControlPlaneNow } from "../servers/useFleet";
import { Page } from "../shell/Page";
import "./Settings.css";

const HOUR = 3_600_000;
const DAY = 24 * HOUR;
/** A page of the log as the screen reads it. */
const PAGE = 100;
/** The most an export holds, as a browser keeps it all before saving it (ADR 0047). */
export const MOST_EXPORTED = 10_000;

type Range = "day" | "week" | "month" | "all";
const RANGES: Readonly<Record<Range, number | undefined>> = {
  day: DAY,
  week: 7 * DAY,
  month: 30 * DAY,
  all: undefined,
};

/** What the screen asks for: the filters, with the time range as it was when chosen. */
interface Filters {
  query: string;
  range: Range;
  actor: AuditActor;
  surface: Surface;
}

type Asked = MessageInitShape<typeof ListAuditEventsRequestSchema>;

function request(f: Filters, now: number): Asked {
  const span = RANGES[f.range];
  return {
    query: f.query.trim(),
    actor: f.actor,
    surface: f.surface,
    ...(span === undefined ? {} : { since: timestampFromDate(new Date(now - span)) }),
  };
}

/**
 * The audit log, as Audit-Log.dc.html has it: every event Dockwell recorded,
 * refusals and first uses included, by day, narrowed by words, time, who and
 * where from, each opening to what it said; and an export of what the filters
 * keep (ADR 0047).
 */
export function AuditLogPage() {
  const { t } = useLingui();
  const phone = useMediaQuery(PHONE_QUERY);
  const [typed, setTyped] = useState("");
  const [filters, setFilters] = useState<Filters>({
    query: "",
    range: "day",
    actor: AuditActor.UNSPECIFIED,
    surface: Surface.UNSPECIFIED,
  });
  // The range runs back from when it was chosen, so pages read on from one place.
  const [from, setFrom] = useState(() => Date.now());
  // Words narrow the log a moment after typing stops, not on each key.
  useEffect(() => {
    const timer = setTimeout(() => setFilters((f) => (f.query === typed ? f : { ...f, query: typed })), 300);
    return () => clearTimeout(timer);
  }, [typed]);
  const asked = useMemo(() => request(filters, from), [filters, from]);
  const log = useInfiniteQuery(
    AuditService.method.listAuditEvents,
    { ...asked, pageSize: PAGE, pageToken: "" },
    { pageParamKey: "pageToken", getNextPageParam: (page) => page.nextPageToken || undefined },
  );
  const events = useMemo(() => log.data?.pages.flatMap((page) => page.events) ?? [], [log.data]);
  const set = (next: Partial<Filters>) => {
    setFrom(Date.now());
    setFilters((f) => ({ ...f, ...next }));
  };

  const ranges = [
    { value: "day", label: t`Last 24 hours` },
    { value: "week", label: t`Last 7 days` },
    { value: "month", label: t`Last 30 days` },
    { value: "all", label: t`Everything` },
  ];
  const actors = [
    { value: String(AuditActor.UNSPECIFIED), label: t`All actors` },
    { value: String(AuditActor.PERSON), label: t`People` },
    { value: String(AuditActor.TOKEN), label: t`API tokens` },
    { value: String(AuditActor.DOCKWELL), label: t`Dockwell` },
    { value: String(AuditActor.SERVER), label: t`Servers` },
    { value: String(AuditActor.SHELL), label: t`Shells on the control server` },
    { value: String(AuditActor.STRANGER), label: t`Not signed in` },
  ];
  const surfaces = [
    { value: String(Surface.UNSPECIFIED), label: t`All sources` },
    { value: String(Surface.PANEL), label: t`Panel` },
    { value: String(Surface.CLI), label: t`CLI` },
    { value: String(Surface.API), label: t`API` },
    { value: String(Surface.AUTOMATIC), label: t`Automatic` },
  ];
  const narrowed =
    filters.query !== "" || filters.actor !== AuditActor.UNSPECIFIED || filters.surface !== Surface.UNSPECIFIED;

  return (
    <Page
      title={t`Audit log`}
      lead={t`Every change and every way in, by people, tokens, servers and Dockwell · kept for as long as this install`}
      back="/settings"
      actions={<Export asked={asked} />}
    >
      <section className="audit" aria-label={t`Events`}>
        <search className="audit-filters">
          <TextField
            className="audit-search"
            icon="search"
            aria-label={t`Filter events`}
            placeholder={t`Filter by actor, action, target or address`}
            value={typed}
            onChange={(event) => setTyped(event.target.value)}
          />
          <SelectField
            aria-label={t`Time range`}
            options={ranges}
            value={filters.range}
            onChange={(event) => set({ range: event.target.value as Range })}
          />
          <SelectField
            aria-label={t`Actor`}
            options={actors}
            value={String(filters.actor)}
            onChange={(event) => set({ actor: Number(event.target.value) as AuditActor })}
          />
          <SelectField
            aria-label={t`Source`}
            options={surfaces}
            value={String(filters.surface)}
            onChange={(event) => set({ surface: Number(event.target.value) as Surface })}
          />
        </search>
        {/* A phone has no room for the page's actions in its header, so export sits under the filters. */}
        {phone ? <Export asked={asked} /> : null}
        {log.error && !log.data ? (
          <Callout
            tone="critical"
            title={t`Dockwell can't read the audit log`}
            actions={
              <Button size="sm" icon="refresh" loading={log.isFetching} onClick={() => void log.refetch()}>
                <Trans>Try again</Trans>
              </Button>
            }
          >
            {sentence(log.error)}
          </Callout>
        ) : !log.data ? (
          <div className="audit-skeleton" aria-busy="true">
            <span className="dw-sr">{t`Loading the audit log`}</span>
          </div>
        ) : events.length === 0 ? (
          <div className="dw-panel tokens-empty">
            <span className="tokens-empty-icon">
              <Icon name="file" size={20} />
            </span>
            <p>
              {narrowed ? (
                <Trans>Nothing matches. Pick a longer range, or clear the filters.</Trans>
              ) : (
                <Trans>Nothing happened in this range. Pick a longer one to see more.</Trans>
              )}
            </p>
          </div>
        ) : phone ? (
          <PhoneLog events={events} />
        ) : (
          <LogTable events={events} />
        )}
        {log.data ? (
          <div className="audit-foot">
            <p className="settings-meta">
              <Count count={events.length} more={log.hasNextPage} range={filters.range} />
            </p>
            {log.hasNextPage ? (
              <Button size="sm" loading={log.isFetchingNextPage} onClick={() => void log.fetchNextPage()}>
                <Trans>Show more</Trans>
              </Button>
            ) : null}
          </div>
        ) : null}
      </section>
    </Page>
  );
}

/** How many events show, and what to do for more. */
function Count({ count, more, range }: { count: number; more: boolean; range: Range }) {
  const { t } = useLingui();
  if (more) return <>{t`The newest ${count} events. Show more for older ones.`}</>;
  const words = {
    day: count === 1 ? t`1 event in the last 24 hours.` : t`${count} events in the last 24 hours.`,
    week: count === 1 ? t`1 event in the last 7 days.` : t`${count} events in the last 7 days.`,
    month: count === 1 ? t`1 event in the last 30 days.` : t`${count} events in the last 30 days.`,
    all: count === 1 ? t`1 event, all there is.` : t`${count} events, all there are.`,
  }[range];
  return <>{range === "all" ? words : `${words} ${t`Pick a longer range to see more.`}`}</>;
}

/** Events by the day they happened on, newest first, as the screen groups them. */
interface Day {
  key: string;
  date: Date;
  events: AuditEvent[];
}

function byDay(events: readonly AuditEvent[]): Day[] {
  const out: Day[] = [];
  for (const event of events) {
    const date = event.time ? timestampDate(event.time) : new Date(0);
    const key = `${date.getFullYear()}-${date.getMonth()}-${date.getDate()}`;
    const last = out[out.length - 1];
    if (last?.key === key) last.events.push(event);
    else out.push({ key, date, events: [event] });
  }
  return out;
}

/** A day's heading: "Today · Saturday 7 Nov", "Yesterday · …", or the day alone. */
function useDayHeading(): (date: Date) => string {
  const { t, i18n } = useLingui();
  const now = useControlPlaneNow(60_000);
  return (date) => {
    const same = (a: Date, b: Date) =>
      a.getFullYear() === b.getFullYear() && a.getMonth() === b.getMonth() && a.getDate() === b.getDate();
    const day = formatWeekday(date, i18n.locale);
    if (same(date, new Date(now))) return t`Today · ${day}`;
    if (same(date, new Date(now - DAY))) return t`Yesterday · ${day}`;
    return day;
  };
}

const SOURCE_ICONS: Readonly<Partial<Record<Surface, IconName>>> = {
  [Surface.PANEL]: "browser",
  [Surface.CLI]: "terminal",
  [Surface.API]: "link",
  [Surface.AUTOMATIC]: "refresh",
};

/** Where an event came from, in a word with its icon. */
function Source({ surface }: { surface: Surface }) {
  const { t } = useLingui();
  const words: Readonly<Partial<Record<Surface, string>>> = {
    [Surface.PANEL]: t`Panel`,
    [Surface.CLI]: t`CLI`,
    [Surface.API]: t`API`,
    [Surface.AUTOMATIC]: t`Automatic`,
  };
  const icon = SOURCE_ICONS[surface];
  if (!icon) return <span className="settings-muted">–</span>;
  return (
    <span className="audit-source">
      <Icon name={icon} size={14} />
      {words[surface]}
    </span>
  );
}

/** Who did it: a person's initials, or a mark for a token, a server, a shell or Dockwell, and a name. */
function ActorCell({ event }: { event: AuditEvent }) {
  const { t } = useLingui();
  const actor = event.actor;
  if (!actor) {
    return (
      <span className="audit-actor">
        <span className="audit-chip is-mark" aria-hidden="true">
          <Icon name="question" size={12} />
        </span>
        <span className="settings-muted">{t`Not signed in`}</span>
      </span>
    );
  }
  const icon: IconName | undefined = {
    [ActorKind.TOKEN]: "key" as const,
    [ActorKind.SERVER]: "server" as const,
    [ActorKind.SHELL]: "terminal" as const,
    [ActorKind.DOCKWELL]: "settings" as const,
  }[actor.kind as number];
  const name = actor.name || actor.id;
  return (
    <span className="audit-actor">
      {icon ? (
        <span className="audit-chip is-mark" aria-hidden="true">
          <Icon name={icon} size={12} />
        </span>
      ) : (
        <span className="audit-chip" aria-hidden="true">
          {initialsOf(name)}
        </span>
      )}
      <span className={cx(actor.kind === ActorKind.SHELL && "audit-mono")}>{name}</span>
    </span>
  );
}

function initialsOf(name: string): string {
  const words = name.trim().split(/\s+/).filter(Boolean);
  const lead = (w: string | undefined) => (w ? (Array.from(w)[0] ?? "") : "");
  return (lead(words[0]) + (words.length > 1 ? lead(words[words.length - 1]) : "")).toLocaleUpperCase();
}

/**
 * What an event said, for people: as recorded, except a setting's, which records
 * its value and the one it replaced as JSON, worded here. An export keeps it as
 * recorded.
 */
function useSaid(): (event: AuditEvent) => string {
  const { t } = useLingui();
  return (event) => {
    if (!event.action.startsWith("settings.") || !event.detail.startsWith("{")) return event.detail;
    try {
      const { value = "", previous = "" } = JSON.parse(event.detail) as { value?: string; previous?: string };
      if (value && previous) return t`${value}, which was ${previous}`;
      if (value) return value;
      if (previous) return t`Cleared, which was ${previous}`;
    } catch {
      // Not what a setting records, so it's shown as it is.
    }
    return event.detail;
  };
}

/** What it acted on: a name where Dockwell has one, the ID otherwise. */
function TargetCell({ event }: { event: AuditEvent }) {
  const target = event.target;
  if (!target) {
    return event.subject ? (
      <code className="audit-id">{event.subject}</code>
    ) : (
      <span className="settings-muted">–</span>
    );
  }
  return target.name ? <span>{target.name}</span> : <code className="audit-id">{target.id}</code>;
}

/** The detail an event opens to: everything it recorded, and where to go from it. */
function Detail({ event }: { event: AuditEvent }) {
  const { t, i18n } = useLingui();
  const said = useSaid();
  const time = event.time ? timestampDate(event.time) : undefined;
  const change = event.action.startsWith("change.") && event.subject.startsWith("chg_") ? event.subject : "";
  return (
    <div className="audit-detail">
      <dl className="settings-facts">
        <dt>{t`Said`}</dt>
        <dd>{said(event) || <span className="settings-muted">{t`Nothing more`}</span>}</dd>
        <dt>{t`When`}</dt>
        <dd>{time ? formatDateTime(time, i18n.locale) : "–"}</dd>
        {event.address ? (
          <>
            <dt>{t`From`}</dt>
            <dd>
              <code>{event.address}</code>
            </dd>
          </>
        ) : null}
        {event.actor?.id ? (
          <>
            <dt>{t`Actor`}</dt>
            <dd>
              <code>{event.actor.id}</code>
            </dd>
          </>
        ) : null}
        {event.target ? (
          <>
            <dt>{t`Target`}</dt>
            <dd>
              <code>{event.target.id}</code>
            </dd>
          </>
        ) : null}
        {event.subject && event.subject !== event.target?.id ? (
          <>
            <dt>{t`About`}</dt>
            <dd>
              <code>{event.subject}</code>
            </dd>
          </>
        ) : null}
        <dt>{t`Event`}</dt>
        <dd>
          <code>{event.id}</code>
        </dd>
      </dl>
      {change ? (
        <div>
          <Link to="/changes/$changeId" params={{ changeId: change }} className="dw-btn dw-btn-secondary dw-btn-sm">
            <Icon name="activity" size={14} />
            <span>
              <Trans>Open in Activity</Trans>
            </span>
          </Link>
        </div>
      ) : null}
    </div>
  );
}

/** The log as a table, a row each, grouped by day, as the mock draws it. */
function LogTable({ events }: { events: readonly AuditEvent[] }) {
  const { t, i18n } = useLingui();
  const said = useSaid();
  const heading = useDayHeading();
  const [open, setOpen] = useState<string>();
  const detailId = useId();
  return (
    <div className="dw-table-wrap">
      <table className="dw-table dw-table-compact audit-table">
        <caption className="dw-sr">{t`The audit log, newest first`}</caption>
        <thead>
          <tr>
            <th scope="col" className="audit-time">
              {t`Time`}
            </th>
            <th scope="col" className="audit-who">
              {t`Actor`}
            </th>
            <th scope="col" className="audit-action">
              {t`Action`}
            </th>
            <th scope="col" className="audit-what">
              {t`Target`}
            </th>
            <th scope="col" className="audit-from">
              {t`Source`}
            </th>
            <th scope="col">{t`Detail`}</th>
            <th scope="col" className="audit-toggle">
              <span className="dw-sr">{t`Details`}</span>
            </th>
          </tr>
        </thead>
        <tbody>
          {byDay(events).map((day) => (
            <Fragment key={day.key}>
              <tr className="audit-day">
                <th scope="colgroup" colSpan={7}>
                  {heading(day.date)}
                </th>
              </tr>
              {day.events.map((event) => {
                const shown = open === event.id;
                const time = event.time ? formatClock(timestampDate(event.time), i18n.locale) : "";
                return (
                  <Fragment key={event.id}>
                    <tr className={cx(shown && "is-open")}>
                      <td>{time}</td>
                      <td>
                        <ActorCell event={event} />
                      </td>
                      <td className="is-mono">{event.action}</td>
                      <td>
                        <TargetCell event={event} />
                      </td>
                      <td>
                        <Source surface={event.surface} />
                      </td>
                      <td className="audit-said">{event.address ? <code>{event.address}</code> : said(event)}</td>
                      <td className="audit-toggle">
                        <Button
                          size="sm"
                          variant="ghost"
                          icon={shown ? "chevron-down" : "chevron-right"}
                          aria-expanded={shown}
                          aria-controls={shown ? `${detailId}-${event.id}` : undefined}
                          aria-label={
                            shown
                              ? t`Hide details for ${event.action} at ${time}`
                              : t`Show details for ${event.action} at ${time}`
                          }
                          onClick={() => setOpen(shown ? undefined : event.id)}
                        />
                      </td>
                    </tr>
                    {shown ? (
                      <tr className="audit-open" id={`${detailId}-${event.id}`}>
                        <td colSpan={7}>
                          <Detail event={event} />
                        </td>
                      </tr>
                    ) : null}
                  </Fragment>
                );
              })}
            </Fragment>
          ))}
        </tbody>
      </table>
    </div>
  );
}

/** The log on a phone: a stacked row each, by day, each opening to its detail. */
function PhoneLog({ events }: { events: readonly AuditEvent[] }) {
  const { t, i18n } = useLingui();
  const heading = useDayHeading();
  const [open, setOpen] = useState<string>();
  return (
    <div className="audit-phone">
      {byDay(events).map((day) => (
        <section key={day.key} className="audit-phone-day" aria-label={heading(day.date)}>
          <h2 className="settings-list-label">{heading(day.date)}</h2>
          <ul className="dw-panel tokens-list">
            {day.events.map((event) => {
              const shown = open === event.id;
              const time = event.time ? formatClock(timestampDate(event.time), i18n.locale) : "";
              return (
                <li key={event.id} className="audit-item">
                  <button
                    type="button"
                    className="audit-item-head"
                    aria-expanded={shown}
                    aria-label={
                      shown
                        ? t`Hide details for ${event.action} at ${time}`
                        : t`Show details for ${event.action} at ${time}`
                    }
                    onClick={() => setOpen(shown ? undefined : event.id)}
                  >
                    <span className="audit-item-line">
                      <span className="audit-mono">{event.action}</span>
                      <span className="settings-muted">{time}</span>
                    </span>
                    <span className="audit-item-line">
                      <ActorCell event={event} />
                      <Source surface={event.surface} />
                    </span>
                    {event.target ? (
                      <span className="audit-item-line settings-muted">
                        <TargetCell event={event} />
                      </span>
                    ) : null}
                  </button>
                  {shown ? <Detail event={event} /> : null}
                </li>
              );
            })}
          </ul>
        </section>
      ))}
    </div>
  );
}

/** One line of CSV, each field quoted where it must be. */
export function csvLine(fields: readonly string[]): string {
  return fields.map((f) => (/[",\n\r]/.test(f) ? `"${f.replaceAll('"', '""')}"` : f)).join(",");
}

/** The audit log as CSV, a line each, with a header. */
export function auditCSV(events: readonly AuditEvent[]): string {
  const kinds: Readonly<Partial<Record<ActorKind, string>>> = {
    [ActorKind.USER]: "person",
    [ActorKind.TOKEN]: "token",
    [ActorKind.DOCKWELL]: "dockwell",
    [ActorKind.SERVER]: "server",
    [ActorKind.SHELL]: "shell",
  };
  const sources: Readonly<Partial<Record<Surface, string>>> = {
    [Surface.PANEL]: "panel",
    [Surface.API]: "api",
    [Surface.CLI]: "cli",
    [Surface.AUTOMATIC]: "automatic",
  };
  const header = csvLine([
    "time",
    "action",
    "actor_kind",
    "actor_id",
    "actor_name",
    "address",
    "source",
    "target_id",
    "target_name",
    "subject",
    "detail",
    "id",
  ]);
  const lines = events.map((e) =>
    csvLine([
      e.time ? timestampDate(e.time).toISOString() : "",
      e.action,
      e.actor ? (kinds[e.actor.kind] ?? "") : "stranger",
      e.actor?.id ?? "",
      e.actor?.name ?? "",
      e.address,
      sources[e.surface] ?? "",
      e.target?.id ?? "",
      e.target?.name ?? "",
      e.subject,
      e.detail,
      e.id,
    ]),
  );
  return `${[header, ...lines].join("\n")}\n`;
}

function save(text: string, name: string, type: string) {
  const url = URL.createObjectURL(new Blob([text], { type }));
  const a = document.createElement("a");
  a.href = url;
  a.download = name;
  document.body.appendChild(a);
  a.click();
  a.remove();
  setTimeout(() => URL.revokeObjectURL(url), 10_000);
}

/** Saves what the filters keep, read a page at a time, as CSV or as JSON lines in the API's shape. */
function Export({ asked }: { asked: Asked }) {
  const { t } = useLingui();
  const transport = useTransport();
  const [busy, setBusy] = useState<"csv" | "json">();
  const [said, setSaid] = useState<string>();
  const run = async (as: "csv" | "json") => {
    setBusy(as);
    setSaid(undefined);
    try {
      const client = createClient(AuditService, transport);
      const all: AuditEvent[] = [];
      let pageToken = "";
      do {
        const page = await client.listAuditEvents(
          create(ListAuditEventsRequestSchema, { ...asked, pageSize: 500, pageToken }),
        );
        all.push(...page.events);
        pageToken = page.nextPageToken;
      } while (pageToken && all.length < MOST_EXPORTED);
      const events = all.slice(0, MOST_EXPORTED);
      const stamp = new Date().toISOString().slice(0, 10);
      if (as === "csv") save(auditCSV(events), `dockwell-audit-${stamp}.csv`, "text/csv;charset=utf-8");
      else
        save(
          `${events.map((e) => toJsonString(AuditEventSchema, e)).join("\n")}\n`,
          `dockwell-audit-${stamp}.jsonl`,
          "application/x-ndjson",
        );
      setSaid(
        pageToken
          ? t`Saved the newest ${MOST_EXPORTED} events. Narrow the filters to save the rest.`
          : events.length === 1
            ? t`Saved 1 event.`
            : t`Saved ${events.length} events.`,
      );
    } catch (err) {
      setSaid(sentence(err));
    } finally {
      setBusy(undefined);
    }
  };
  return (
    <div className="audit-export">
      {said ? (
        <span className="settings-meta" role="status">
          {said}
        </span>
      ) : null}
      <Button icon="download" loading={busy === "csv"} disabled={busy !== undefined} onClick={() => void run("csv")}>
        <Trans>Export CSV</Trans>
      </Button>
      <Button variant="ghost" loading={busy === "json"} disabled={busy !== undefined} onClick={() => void run("json")}>
        <Trans>JSON lines</Trans>
      </Button>
    </div>
  );
}
