// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate } from "@bufbuild/protobuf/wkt";
import { useQuery } from "@connectrpc/connect-query";
import {
  type Actor,
  ActorKind,
  type Change,
  ChangeService,
  ChangeState,
  type Event,
  type EventAlert,
  type EventBackup,
  type EventDesiredSent,
  EventKind,
  type EventTerminal,
  ResourceService,
} from "@dockwell/api";
import {
  Button,
  Callout,
  cx,
  Icon,
  type IconName,
  Panel,
  PHONE_QUERY,
  SegmentedControl,
  StatusBadge,
  Table,
  type TableColumn,
  Tag,
  Timeline,
  type TimelineEvent,
  useMediaQuery,
} from "@dockwell/ui";
import type { I18n } from "@lingui/core";
import { msg, plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { type ReactNode, useCallback, useEffect, useMemo, useRef } from "react";

import { sentence } from "../../lib/errors";
import { formatBytes, formatClock, formatDateTime, formatWeekday } from "../../lib/format";
import { RouterButtonLink } from "../../lib/links";
import { useTimeWords } from "../../lib/time";
import { initials } from "../auth/user";
import { useChangeSheet } from "../changes/ChangeSheet";
import { BecauseNotice, FailureNotice, MachineWords } from "../changes/FailureNotice";
import { useFieldRows } from "../changes/fields";
import { canUndo, failedStep, shortFingerprint, wentOffline } from "../changes/plan";
import { RollbackLine, rollbackSubject, useRollbackWord } from "../changes/RollbackLine";
import { type StepLook, useChangeWords } from "../changes/words";
import { parentOf } from "../files/paths";
import { FailedDeployLogs } from "../logs/DeployLogLinks";
import { Reconnecting } from "../servers/FleetNotes";
import { useControlPlaneNow, useFleetView } from "../servers/useFleet";
import { EmptyState } from "../shell/EmptyState";
import { Page } from "../shell/Page";
import { useEndWords } from "../terminal/words";
import { useActorWords } from "./actor";
import { type Described, happenedAt, useCounts, useDescribe, useHealthEventWords, useProgress } from "./describe";
import { RecordedPlan } from "./RecordedPlan";
import { type EventFilter, useEvents } from "./useEvents";
import "./ActivityPage.css";
import { placesOfChange, useMay } from "../auth/may";
import { RefLink, refKindOf } from "../shell/RefLink";

/** The kinds of event Activity filters by, as its search names them. */
export type ActivityKind = "changes" | "servers" | "access" | "health" | "backups" | "alerts";

/** Activity's place, in its URL: the filters and the event open beside the list. */
export interface ActivitySearch {
  kind?: ActivityKind;
  /** An actor's ID, or "dockwell" for what Dockwell did on its own. */
  actor?: string;
  server?: string;
  resource?: string;
  event?: string;
}

const KINDS: Readonly<Record<ActivityKind, EventKind>> = {
  changes: EventKind.CHANGE,
  servers: EventKind.SERVER,
  access: EventKind.ACCESS,
  health: EventKind.HEALTH,
  backups: EventKind.BACKUP,
  alerts: EventKind.ALERT,
};

/** Dockwell, in the actor filter. */
export const DOCKWELL_ACTOR = "dockwell";

/**
 * The timeline: every change, with who made it and from where, beside
 * servers joining and access granted, newest first and live (Activity).
 * Each change opens its plan, and offers undo while it can be undone. On
 * a phone the list is a timeline, and a change opens its plan sheet.
 */
export function ActivityPage({
  search,
  onSearch,
}: {
  search: ActivitySearch;
  onSearch: (next: ActivitySearch) => void;
}) {
  const { t, i18n } = useLingui();
  const phone = useMediaQuery(PHONE_QUERY);
  const sheet = useChangeSheet();
  const describe = useDescribe();
  const time = useTimeWords();
  const now = useControlPlaneNow(15_000);
  const filter = useMemo<EventFilter>(
    () => ({
      kinds: search.kind ? [KINDS[search.kind]] : [],
      serverId: search.server ?? "",
      resourceId: search.resource ?? "",
      actorId: search.actor && search.actor !== DOCKWELL_ACTOR ? search.actor : "",
      actorKind: search.actor === DOCKWELL_ACTOR ? ActorKind.DOCKWELL : ActorKind.UNSPECIFIED,
    }),
    [search.kind, search.server, search.resource, search.actor],
  );
  const { state, older, loadingOlder, olderError, loadOlder } = useEvents(filter);
  // The top bar speaks for the fleet's watch; the timeline has its own, and says so itself.
  const status = state.status === "reconnecting" ? <Reconnecting /> : undefined;
  const events = state.status === "connecting" ? undefined : state.events;

  // What reverted changes were called, so an undo can say what it undid.
  const summaries = useMemo(() => {
    const out = new Map<string, string>();
    for (const e of events ?? []) if (e.change) out.set(e.id, e.change.summary);
    return out;
  }, [events]);
  // A run of the same thing done again, as reading one file over and over, is one row on a
  // wide screen, which says how many times. A phone's timeline has no room to say so.
  const { shown, repeats } = useMemo(() => collapseRepeats(events ?? []), [events]);
  const days = useMemo(() => byDay(phone ? (events ?? []) : shown), [events, shown, phone]);
  const selected = search.event ? events?.find((e) => e.id === search.event) : undefined;
  const select = (id: string | undefined) => onSearch({ ...search, event: id });
  // Below the list, where a narrow screen puts it, an event chosen comes into view.
  const layout = useRef<HTMLDivElement>(null);
  const filters = useRef<HTMLDivElement>(null);
  // On a phone the kinds scroll sideways: keep the one chosen in sight, as a link
  // may open Activity on any of them.
  const kind = search.kind ?? "all";
  // biome-ignore lint/correctness/useExhaustiveDependencies: it runs when the kind changes, and reads the segment that kind checked from the page
  useEffect(() => {
    const row = filters.current?.querySelector<HTMLElement>(".dw-seg");
    const on = row?.querySelector<HTMLElement>(`[aria-checked="true"]`);
    if (!row || !on) return;
    const edge = row.getBoundingClientRect();
    const box = on.getBoundingClientRect();
    if (box.left < edge.left || box.right > edge.right) {
      row.scrollLeft += box.left - edge.left - (edge.width - box.width) / 2;
    }
  }, [kind]);
  const reveal = (tries = 10) =>
    requestAnimationFrame(() => {
      const detail = layout.current?.querySelector<HTMLElement>(".activity-detail");
      const list = layout.current?.querySelector<HTMLElement>(".activity-list");
      if (!detail || !list) {
        if (tries > 0) reveal(tries - 1);
        return;
      }
      if (detail.offsetTop > list.offsetTop + 8) detail.scrollIntoView?.({ block: "start", behavior: "smooth" });
    });

  const kindOptions = [
    { value: "all", label: t`All` },
    { value: "changes", label: t`Changes` },
    { value: "servers", label: t`Servers` },
    { value: "access", label: t`Access` },
    { value: "health", label: t`Health` },
    { value: "backups", label: t`Backups` },
    { value: "alerts", label: t`Alerts` },
  ];

  let body: ReactNode;
  if (!events) {
    body = state.error ? (
      <Callout tone="critical" title={t`Can't load the activity yet`}>
        {sentence(state.error)} <Trans>Dockwell keeps trying.</Trans>
      </Callout>
    ) : (
      <p className="activity-note" aria-busy="true">
        <Trans>Loading activity</Trans>
      </p>
    );
  } else if (events.length === 0) {
    body = (
      <Panel>
        <EmptyState icon="activity" title={t`Nothing here yet`}>
          {filtered(search) ? (
            <Trans>Nothing matches these filters. Clear them to see everything.</Trans>
          ) : (
            <Trans>Changes, servers joining and sign-ins show up here as they happen.</Trans>
          )}
        </EmptyState>
      </Panel>
    );
  } else {
    body = (
      <>
        {days.map((day) => {
          const label = dayLabel(day.date, now, i18n);
          const count = day.events.reduce((n, e) => n + (phone ? 1 : (repeats.get(e.id)?.length ?? 1)), 0);
          const long = formatWeekday(day.date, i18n.locale);
          const meta = plural(count, { one: `${long} · # event`, other: `${long} · # events` });
          return (
            <section key={day.key} className="activity-day" aria-label={label}>
              <div className="activity-day-head">
                <h2 className="dw-text-heading">{label}</h2>
                <span className="activity-day-meta">{meta}</span>
              </div>
              {phone ? (
                <EventTimeline events={day.events} summaries={summaries} now={now} />
              ) : (
                <DayTable
                  events={day.events}
                  repeats={repeats}
                  summaries={summaries}
                  now={now}
                  caption={label}
                  selected={search.event}
                  onSelect={(e) => {
                    select(e.id === search.event ? undefined : e.id);
                    if (e.id !== search.event) reveal();
                  }}
                />
              )}
            </section>
          );
        })}
        {older ? (
          <div className="activity-more">
            <Button variant="ghost" icon="chevron-down" loading={loadingOlder} onClick={loadOlder}>
              <Trans>Show earlier events</Trans>
            </Button>
            {olderError ? (
              <p className="activity-note" role="alert">
                {sentence(olderError)}
              </p>
            ) : null}
          </div>
        ) : null}
      </>
    );
  }

  return (
    <Page
      title={t`Activity`}
      lead={t`Every change, server event, access grant, change of health, backup and alert, newest first`}
      status={status}
      className="activity"
    >
      <div ref={filters} className="activity-filters">
        <SegmentedControl
          options={kindOptions}
          value={kind}
          label={t`Event type`}
          onChange={(value) =>
            onSearch({ ...search, kind: value === "all" ? undefined : (value as ActivityKind), event: undefined })
          }
        />
        <span className="activity-fill" />
        <ActorFilter
          events={events ?? []}
          value={search.actor ?? ""}
          onChange={(actor) => onSearch({ ...search, actor: actor || undefined, event: undefined })}
        />
      </div>
      <Scopes search={search} events={events ?? []} onSearch={onSearch} />
      <div ref={layout} className="activity-layout">
        <div className="activity-list">{body}</div>
        {selected && !phone ? (
          <Detail
            event={selected}
            repeats={repeats.get(selected.id)}
            described={describe(selected, summaries)}
            now={now}
            time={time}
            onClose={() => select(undefined)}
            onSelect={(id) => select(id)}
            onOpen={(id) => sheet.open({ kind: "change", id })}
            onUndo={(id) => sheet.open({ kind: "undo", id })}
          />
        ) : null}
      </div>
    </Page>
  );
}

/** Activity's search without the filters left unset, for its URL. */
export function searchWithout(search: ActivitySearch): ActivitySearch {
  return Object.fromEntries(Object.entries(search).filter(([, v]) => v !== undefined)) as ActivitySearch;
}

function filtered(search: ActivitySearch): boolean {
  return Boolean(search.kind || search.actor || search.server || search.resource);
}

interface Day {
  key: string;
  date: Date;
  events: Event[];
}

/** How far apart the same thing done again may be, to be one row. */
const REPEAT_MS = 10 * 60_000;

/**
 * Runs of the same access, newest first, as one row each: the same actor doing the
 * same thing to the same subject from the same surface, each within REPEAT_MS of
 * the one after, on the same local day. Changes, health and the rest each say
 * something new. Each run is under its newest.
 */
export function collapseRepeats(events: readonly Event[]): { shown: Event[]; repeats: Map<string, Event[]> } {
  const shown: Event[] = [];
  const repeats = new Map<string, Event[]>();
  const sameness = (e: Event) =>
    e.kind !== EventKind.ACCESS
      ? undefined
      : [e.kind, e.action, e.surface, e.actor?.kind, e.actor?.id, e.subject?.id, e.subject?.path, e.subject?.name].join(
          "|",
        );
  let run: Event[] | undefined;
  let runKey: string | undefined;
  for (const event of events) {
    const key = sameness(event);
    const last = run?.at(-1);
    if (
      run &&
      last &&
      key !== undefined &&
      key === runKey &&
      dayKey(happenedAt(last)) === dayKey(happenedAt(event)) &&
      happenedAt(last).getTime() - happenedAt(event).getTime() <= REPEAT_MS
    ) {
      run.push(event);
      continue;
    }
    run = [event];
    runKey = key;
    shown.push(event);
    repeats.set(event.id, run);
  }
  return { shown, repeats };
}

function dayKey(date: Date): string {
  return `${date.getFullYear()}-${date.getMonth()}-${date.getDate()}`;
}

/** Events by the local day they happened on, newest day first, as they come. */
function byDay(events: readonly Event[]): Day[] {
  const out: Day[] = [];
  for (const event of events) {
    const date = event.time ? timestampDate(event.time) : new Date(0);
    const key = dayKey(date);
    const last = out[out.length - 1];
    if (last?.key === key) last.events.push(event);
    else out.push({ key, date, events: [event] });
  }
  return out;
}

function dayLabel(date: Date, now: number, i18n: I18n): string {
  const today = new Date(now);
  const yesterday = new Date(now - 86_400_000);
  const same = (a: Date, b: Date) =>
    a.getFullYear() === b.getFullYear() && a.getMonth() === b.getMonth() && a.getDate() === b.getDate();
  if (same(date, today)) return i18n._(msg`Today`);
  if (same(date, yesterday)) return i18n._(msg`Yesterday`);
  return formatWeekday(date, i18n.locale);
}

/** An event's mark: its icon, toned only for an outcome worth noticing. */
function Mark({ icon, tone }: { icon: IconName; tone?: Described["tone"] | undefined }) {
  return (
    <span className={cx("dw-tl-mark", "activity-mark", tone && `dw-tone-${tone}`)} aria-hidden="true">
      <Icon name={icon} size={14} />
    </span>
  );
}

/** The line under an event's title: its state, the plan in brief or its progress, and what reverted it. */
function useMeta() {
  const { t } = useLingui();
  const words = useChangeWords();
  const counts = useCounts();
  const progress = useProgress();
  const rollbackWord = useRollbackWord();
  const health = useHealthEventWords();
  return useCallback(
    (event: Event): { look: StepLook | undefined; parts: string[] } => {
      const change = event.change;
      if (event.kind === EventKind.HEALTH) {
        const said = health.flips(event);
        return { look: health.look(event), parts: said ? [said] : [] };
      }
      if (event.kind === EventKind.ALERT && event.alert?.reason) {
        return { look: undefined, parts: [words.via(event.surface), event.alert.reason] };
      }
      if (!change) return { look: undefined, parts: [words.via(event.surface)] };
      const parts: string[] = [];
      const steps = change.state === ChangeState.APPLYING ? progress(change) : "";
      const brief = steps || counts(change);
      if (brief) parts.push(brief);
      if (change.undoneBy) parts.push(t`Undone`);
      // A planned change the reader's role doesn't cover waits for an admin or the
      // owner, and someone else's in a protected environment, an admin's
      // approval, however it was planned: the API says which (ADR 0049).
      if (change.state === ChangeState.PLANNED) {
        if (change.waits) parts.push(t`Waits for an admin`);
        else if (change.approves) parts.push(t`Waits for your approval`);
      }
      const rollback = rollbackWord(change);
      if (rollback) parts.push(rollback);
      return { look: words.state(change.state), parts };
    },
    [t, words, counts, progress, rollbackWord, health],
  );
}

/** The line under an event's title in a table: its state as a badge, then the rest, each apart. */
function MetaLine({ look, parts }: { look: StepLook | undefined; parts: string[] }) {
  return (
    <span className="dw-tl-meta">
      {/* Its word is the state, so a screen reader hears that alone. */}
      {look ? <StatusBadge status={look.status} label={look.word} labelIsState /> : null}
      {parts.map((part) => (
        <span key={part}>{part}</span>
      ))}
    </span>
  );
}

interface DayProps {
  events: Event[];
  summaries: ReadonlyMap<string, string>;
  now: number;
}

function DayTable({
  events,
  repeats,
  summaries,
  now,
  caption,
  selected,
  onSelect,
}: DayProps & {
  repeats: ReadonlyMap<string, Event[]>;
  caption: string;
  selected: string | undefined;
  onSelect: (event: Event) => void;
}) {
  const { t, i18n } = useLingui();
  const describe = useDescribe();
  const time = useTimeWords();
  const meta = useMeta();
  const columns: TableColumn<Event>[] = [
    {
      key: "title",
      header: t`Event`,
      render: (event) => {
        const d = describe(event, summaries);
        const line = meta(event);
        const times = repeats.get(event.id)?.length ?? 1;
        return (
          <div className="activity-event">
            <Mark icon={d.icon} tone={d.tone} />
            <div className="activity-event-text">
              <span className="activity-event-title">{d.title}</span>
              <MetaLine
                look={line.look}
                parts={times > 1 ? [...line.parts, plural(times, { one: "# time", other: "# times" })] : line.parts}
              />
            </div>
          </div>
        );
      },
    },
    { key: "actor", header: t`By`, width: 172, render: (event) => <ActorView actor={event.actor} /> },
    {
      key: "time",
      header: t`When`,
      width: 104,
      align: "right",
      render: (event) => {
        const date = happenedAt(event);
        return (
          <div className="activity-when">
            <time dateTime={date.toISOString()}>{time.ago(date, now)}</time>
            <span>{formatClock(date, i18n.locale)}</span>
          </div>
        );
      },
    },
  ];
  return (
    <Table<Event>
      columns={columns}
      rows={events}
      rowKey="id"
      caption={t`Events: ${caption}`}
      selectedKey={selected ?? null}
      onRowClick={onSelect}
    />
  );
}

/** A day on a phone, and the overview's recent changes: one line of time, each change opening its plan sheet. */
export function EventTimeline({
  events,
  summaries,
  now,
  framed = true,
}: DayProps & {
  /** Drawn in a frame of its own, as a day on a phone is; the overview's sits in its panel. */
  framed?: boolean;
}) {
  const { t } = useLingui();
  const describe = useDescribe();
  const time = useTimeWords();
  const meta = useMeta();
  const sheet = useChangeSheet();
  const may = useMay();
  const mayUndo = may(ChangeService.method.undoChange).allowed;
  const items: TimelineEvent[] = events.map((event) => {
    const d = describe(event, summaries);
    const date = happenedAt(event);
    const change = event.change;
    return {
      id: event.id,
      kind: change ? (change.reverts ? "rollback" : "plan") : event.kind === EventKind.SERVER ? "server" : "config",
      icon: d.icon,
      ...(d.tone ? { tone: d.tone } : {}),
      title: change ? (
        <button type="button" className="activity-open" onClick={() => sheet.open({ kind: "change", id: event.id })}>
          {d.title}
        </button>
      ) : (
        d.title
      ),
      actor: event.actor?.name ?? "",
      // A timeline's line under the title is one run of text, so its parts are joined.
      meta: (({ look, parts }) => (look ? [look.word, ...parts] : parts).join(" · "))(meta(event)),
      time: time.ago(date, now),
      datetime: date.toISOString(),
      // Where each of its items is: undoing plans in every one of them.
      undoable: canUndo(change, now) && may(ChangeService.method.undoChange, placesOfChange(change)).allowed,
      undoLabel: t`Undo`,
    };
  });
  return (
    <Timeline
      className={framed ? "activity-timeline" : undefined}
      events={items}
      // A timeline's undo has no off state, so it isn't drawn for someone who may not undo (ADR 0059).
      {...(mayUndo ? { onUndo: (item) => sheet.open({ kind: "undo", id: String(item.id) }) } : {})}
    />
  );
}

/**
 * Whether someone other than whoever applied a change proposed it, or
 * proposed it from elsewhere, as when the CLI plans what only a person
 * may apply: then the timeline names the one who applied it, and the
 * detail names the proposer too.
 */
function proposedApart(change: Change): boolean {
  const by = change.proposedBy;
  const applied = change.appliedBy;
  if (!applied || !by) return false;
  return by.kind !== applied.kind || by.id !== applied.id || change.proposedVia !== change.appliedVia;
}

/** Who did it: a person's initials, or a mark for a token, a server, a shell account or Dockwell. */
function ActorView({ actor }: { actor: Actor | undefined }) {
  const who = useActorWords()(actor);
  return (
    <span className="activity-actor" title={who.full === who.name ? undefined : who.full}>
      {who.icon ? (
        <span className="activity-badge" aria-hidden="true">
          <Icon name={who.icon} size={12} />
        </span>
      ) : (
        <span className="activity-avatar" aria-hidden="true">
          {initials(who.name)}
        </span>
      )}
      <span className={cx(who.machine ? "is-machine" : undefined)}>{who.name}</span>
    </span>
  );
}

/** The actor filter: everyone the loaded events name, and Dockwell, as a native select styled as a button. */
function ActorFilter({ events, value, onChange }: { events: Event[]; value: string; onChange: (v: string) => void }) {
  const { t } = useLingui();
  const actors = useMemo(() => {
    const seen = new Map<string, string>();
    for (const e of events) {
      const a = e.actor;
      if (a?.id && a.kind !== ActorKind.DOCKWELL) seen.set(a.id, a.name || a.id);
    }
    return [...seen.entries()].sort((a, b) => a[1].localeCompare(b[1]));
  }, [events]);
  const options: [string, string][] = [["", t`All actors`], [DOCKWELL_ACTOR, "Dockwell"], ...actors];
  if (value && !options.some(([id]) => id === value)) options.push([value, value]);
  const current = options.find(([id]) => id === value)?.[1] ?? t`All actors`;
  return (
    <label className="dw-btn dw-btn-secondary dw-btn-md activity-select">
      <Icon name="users" size={16} />
      <span>{current}</span>
      <Icon name="chevron-down" size={14} />
      <select aria-label={t`Actor`} value={value} onChange={(event) => onChange(event.target.value)}>
        {options.map(([id, label]) => (
          <option key={id} value={id}>
            {label}
          </option>
        ))}
      </select>
    </label>
  );
}

/** The server or resource the list is narrowed to, each with a way to clear it. */
function Scopes({
  search,
  events,
  onSearch,
}: {
  search: ActivitySearch;
  events: readonly Event[];
  onSearch: (next: ActivitySearch) => void;
}) {
  const { t } = useLingui();
  const { servers } = useFleetView();
  // Only the kind of resource the ID names: its prefix says which.
  const kind = search.resource ? kindOf(search.resource) : "";
  const resources = useQuery(ResourceService.method.listResources, { kind }, { enabled: kind !== "" });
  if (!search.server && !search.resource) return null;
  // A removed server is in no fleet, but its events still name it.
  const server =
    servers?.find((s) => s.id === search.server)?.name ||
    events.find((e) => e.subject?.id === search.server && e.subject?.name)?.subject?.name ||
    search.server ||
    "";
  const resource = resources.data?.resources.find((r) => r.id === search.resource);
  const resourceName = resource?.resource?.name ?? search.resource ?? "";
  return (
    <div className="activity-scopes">
      {search.server ? (
        <Tag
          icon="server"
          label={t`server`}
          value={server}
          removeLabel={t`the server filter`}
          onRemove={() => onSearch({ ...search, server: undefined, event: undefined })}
        />
      ) : null}
      {search.resource ? (
        <Tag
          icon={resource?.resource?.kind === "project" ? "folder" : "layers"}
          label={resource?.resource?.kind ?? t`resource`}
          value={resourceName}
          removeLabel={t`the resource filter`}
          onRemove={() => onSearch({ ...search, resource: undefined, event: undefined })}
        />
      ) : null}
    </div>
  );
}

/** The kinds whose IDs say what they are, by their prefix. */
const KIND_OF_PREFIX: Readonly<Record<string, string>> = {
  prj: "project",
  env: "environment",
  svc: "service",
  var: "variable",
  sec: "secret",
};

/**
 * The kind of resource an ID belongs to, from its prefix, or "" for an ID
 * that doesn't say, which Activity shows as it is.
 */
export function kindOf(resourceId: string): string {
  return KIND_OF_PREFIX[resourceId.slice(0, resourceId.indexOf("_"))] ?? "";
}

interface DetailProps {
  event: Event;
  /** The run it heads, when the same was done again, newest first. */
  repeats?: readonly Event[] | undefined;
  described: Described;
  now: number;
  time: ReturnType<typeof useTimeWords>;
  onClose: () => void;
  onSelect: (id: string) => void;
  onOpen: (id: string) => void;
  onUndo: (id: string) => void;
}

/** The selected event beside the list: who, when, from where, and for a change what it changed. */
function Detail({ event, repeats, described, now, time, onClose, onSelect, onOpen, onUndo }: DetailProps) {
  // Undoing plans a change, which a viewer can't: off, with why (ADR 0059).
  const mayUndo = useMay()(ChangeService.method.undoChange, placesOfChange(event.change));
  const { t, i18n } = useLingui();
  const words = useChangeWords();
  const rows = useFieldRows();
  const change = event.change;
  const full = useQuery(ChangeService.method.getChange, { id: event.id }, { enabled: change !== undefined });
  const date = happenedAt(event);
  const ago = time.ago(date, now);
  const kindWord = event.actor ? words.actorKind(event.actor.kind) : "";
  const look = change ? words.state(change.state) : undefined;
  // A change to a variable or secret, or a reveal, leads to its environment's list of them.
  const valueEnv = change
    ? full.data?.change?.plan?.resources.find((r) => r.resource?.kind === "variable" || r.resource?.kind === "secret")
        ?.resource?.scope
    : event.action === "secret.reveal"
      ? event.subject?.scope || undefined
      : undefined;
  const toVariables =
    valueEnv !== undefined ? (
      <RouterButtonLink to="/variables" search={{ environment: valueEnv }} variant="ghost" icon="key">
        <Trans>Open the variables and secrets</Trans>
      </RouterButtonLink>
    ) : null;
  const fileOn =
    event.action === "file.save_way_back_in" || event.action === "file.read" || event.action === "file.save"
      ? event.subject
      : undefined;
  const toFile = fileOn?.path ? (
    <RouterButtonLink
      to="/servers/$serverId/files"
      params={{ serverId: fileOn.id }}
      search={{ file: fileOn.path }}
      variant="ghost"
      icon="file"
    >
      <Trans>Open the file</Trans>
    </RouterButtonLink>
  ) : null;
  // What was made, moved or put back is in a folder, which a file link might not open.
  const placed =
    event.action === "file.make" ||
    event.action === "file.move" ||
    event.action === "file.restore" ||
    event.action === "file.upload"
      ? event.subject
      : undefined;
  const placedAt = placed ? placed.toPath || placed.path : "";
  const toFolder =
    placed && placedAt ? (
      <RouterButtonLink
        to="/servers/$serverId/files"
        params={{ serverId: placed.id }}
        search={{ path: parentOf(placedAt) }}
        variant="ghost"
        icon="folder"
      >
        <Trans>Open the folder</Trans>
      </RouterButtonLink>
    ) : null;
  return (
    <div className="activity-detail">
      <Panel
        title={
          <span className="activity-detail-title">
            <Mark icon={described.icon} tone={described.tone} />
            <span>{described.title}</span>
          </span>
        }
        actions={<Button variant="ghost" size="sm" icon="x" aria-label={t`Close details`} onClick={onClose} />}
      >
        <div className="activity-detail-body">
          <dl className="activity-facts">
            <dt>{t`Who`}</dt>
            <dd>
              <ActorView actor={event.actor} />
              {kindWord ? <span className="activity-muted">{kindWord}</span> : null}
            </dd>
            <dt>{t`When`}</dt>
            <dd>
              {formatDateTime(date, i18n.locale)} <span className="activity-muted">· {ago}</span>
            </dd>
            {repeats && repeats.length > 1 ? <RepeatFacts run={repeats} /> : null}
            <dt>{t`From`}</dt>
            <dd>{words.surface(event.surface)}</dd>
            {change && proposedApart(change) ? (
              <>
                <dt>{t`Proposed by`}</dt>
                <dd>
                  <ActorView actor={change.proposedBy} />
                  <span className="activity-muted">
                    {[change.proposedBy ? words.actorKind(change.proposedBy.kind) : "", words.via(change.proposedVia)]
                      .filter(Boolean)
                      .join(" · ")}
                  </span>
                </dd>
              </>
            ) : null}
            {change ? (
              <>
                <dt>{t`State`}</dt>
                <dd>{look ? <StatusBadge status={look.status} label={look.word} /> : t`Applied`}</dd>
                <dt>{t`Plan`}</dt>
                <dd>
                  <code className="activity-code">{shortFingerprint(change.plan)}</code>
                </dd>
                {change.reverts ? (
                  <>
                    <dt>{change.rollback ? t`Rolls back` : t`Undoes`}</dt>
                    <dd>
                      <button type="button" className="activity-link" onClick={() => onSelect(change.reverts)}>
                        <code className="activity-code">{change.reverts}</code>
                      </button>
                    </dd>
                  </>
                ) : null}
                {change.undoneBy || change.rolledBackBy ? (
                  <>
                    <dt>{change.undoneBy ? t`Undone by` : t`Rollback`}</dt>
                    <dd>
                      <button
                        type="button"
                        className="activity-link"
                        onClick={() => onSelect(change.undoneBy || change.rolledBackBy)}
                      >
                        <code className="activity-code">{change.undoneBy || change.rolledBackBy}</code>
                      </button>
                    </dd>
                  </>
                ) : null}
              </>
            ) : null}
            {/* A mute's subject is its rule, which the Rule row names. */}
            {!change && event.subject && (event.subject.name || !event.alert) ? (
              <>
                <dt>{t`About`}</dt>
                <dd>
                  <Subject id={event.subject.id} name={event.subject.name} />
                </dd>
              </>
            ) : null}
            {event.health ? <HealthFacts event={event} /> : null}
            {event.backup ? <BackupFacts backup={event.backup} /> : null}
            {event.alert ? <AlertFacts alert={event.alert} /> : null}
            {event.terminal ? <TerminalFacts terminal={event.terminal} /> : null}
            {event.desiredSent ? <DesiredSentFacts sent={event.desiredSent} /> : null}
            {event.reconciled?.reason ? (
              <>
                <dt>{t`Why`}</dt>
                <dd>{event.reconciled.reason}</dd>
              </>
            ) : null}
          </dl>
          {change?.because ? <BecauseNotice because={change.because} /> : null}
          {change?.state === ChangeState.FAILED ? (
            <RollbackLine change={change} subject={rollbackSubject(full.data?.change?.plan ?? change.plan)} />
          ) : null}
          {change?.state === ChangeState.FAILED && change.error && !wentOffline(change) ? (
            <FailureNotice message={change.error} detail={failedStep(change)?.detail} />
          ) : null}
          {change &&
          ((change.state === ChangeState.FAILED && !wentOffline(change)) ||
            change.state === ChangeState.APPLIED ||
            change.state === ChangeState.APPLYING) ? (
            <FailedDeployLogs key={`${change.id}:${change.state}`} change={full.data?.change ?? change} />
          ) : null}
          {change ? (
            <div className="activity-detail-section">
              <h3 className="dw-text-label">
                <Trans>What changed</Trans>
              </h3>
              {full.data?.change ? (
                <RecordedPlan plan={full.data.change.plan} applied={change.state === ChangeState.APPLIED} rows={rows} />
              ) : full.error ? (
                <p className="activity-note" role="alert">
                  {sentence(full.error)}
                </p>
              ) : (
                <p className="activity-note" aria-busy="true">
                  <Trans>Loading the plan</Trans>
                </p>
              )}
            </div>
          ) : null}
          {change ? (
            <div className="activity-detail-actions">
              {toVariables}
              {canUndo(change, now) ? (
                <Button icon="undo" onClick={() => onUndo(change.id)} {...mayUndo.off}>
                  <Trans>Undo this change</Trans>
                </Button>
              ) : null}
              <Button
                variant={change.state === ChangeState.PLANNED ? "primary" : "ghost"}
                icon="commit"
                onClick={() => onOpen(change.id)}
              >
                {change.state === ChangeState.PLANNED ? <Trans>Review plan</Trans> : <Trans>Open plan</Trans>}
              </Button>
            </div>
          ) : toVariables || toFile || toFolder ? (
            <div className="activity-detail-actions">
              {toVariables}
              {toFile}
              {toFolder}
            </div>
          ) : event.health && event.subject?.id ? (
            <div className="activity-detail-actions">
              <RouterButtonLink
                to="/apps/$serviceId"
                params={{ serviceId: event.subject.id }}
                variant="ghost"
                icon="service"
              >
                <Trans>Open the app</Trans>
              </RouterButtonLink>
            </div>
          ) : null}
        </div>
      </Panel>
    </div>
  );
}

/** An alert, or a mute, in the facts of an event: its rule, where, why it was muted, and what it said. */
function AlertFacts({ alert }: { alert: EventAlert }) {
  const { t } = useLingui();
  return (
    <>
      {alert.rule ? (
        <>
          <dt>{t`Rule`}</dt>
          <dd>
            <code className="activity-code">{alert.rule}</code>
          </dd>
        </>
      ) : null}
      {alert.serverName ? (
        <>
          <dt>{t`Server`}</dt>
          <dd>
            <code className="activity-code">
              {alert.serverId ? (
                <RefLink kind="server" id={alert.serverId} look="plain">
                  {alert.serverName}
                </RefLink>
              ) : (
                alert.serverName
              )}
            </code>
          </dd>
        </>
      ) : null}
      {alert.reason ? (
        <>
          <dt>{t`Why`}</dt>
          <dd>{alert.reason}</dd>
        </>
      ) : null}
      {alert.detail ? (
        <>
          <dt>{t`What it said`}</dt>
          <dd>{alert.detail}</dd>
        </>
      ) : null}
    </>
  );
}

/** How often the same was done again, and over what time, for a row that stands for a run. */
function RepeatFacts({ run }: { run: readonly Event[] }) {
  const { t, i18n } = useLingui();
  const count = run.length;
  const first = formatClock(happenedAt(run[run.length - 1] as Event), i18n.locale);
  const last = formatClock(happenedAt(run[0] as Event), i18n.locale);
  return (
    <>
      <dt>{t`Times`}</dt>
      <dd>{first === last ? t`${count}, at ${first}` : t`${count}, from ${first} to ${last}`}</dd>
    </>
  );
}

/** What an event is about, as a link to its page where it has one, by the kind its ID says. */
function Subject({ id, name }: { id: string; name: string }) {
  const kind = refKindOf(id);
  const words = name || <code className="activity-code">{id}</code>;
  return kind ? (
    <RefLink kind={kind} id={id} look="plain">
      {words}
    </RefLink>
  ) : (
    words
  );
}

/** What became of a backup, in the facts of an event: of what, where, and why it failed. */
function BackupFacts({ backup }: { backup: EventBackup }) {
  const { t, i18n } = useLingui();
  return (
    <>
      <dt>{t`Volume`}</dt>
      <dd>
        <code className="activity-code">{backup.volume}</code>
      </dd>
      <dt>{t`Server`}</dt>
      <dd>
        <code className="activity-code">
          {backup.serverId ? (
            <RefLink kind="server" id={backup.serverId} look="plain">
              {backup.serverName}
            </RefLink>
          ) : (
            backup.serverName
          )}
        </code>
      </dd>
      {backup.sizeBytes > 0n ? (
        <>
          <dt>{t`Size`}</dt>
          <dd>{formatBytes(backup.sizeBytes, i18n.locale)}</dd>
        </>
      ) : null}
      {backup.error ? (
        <>
          <dt>{t`Why it failed`}</dt>
          <dd>{backup.error}</dd>
        </>
      ) : null}
    </>
  );
}

/** A terminal, in the facts of its opening or closing: who it ran as, when, and how it ended. Never what was typed. */
function TerminalFacts({ terminal }: { terminal: EventTerminal }) {
  const { t, i18n } = useLingui();
  const ended = useEndWords();
  return (
    <>
      {terminal.id ? (
        <>
          <dt>{t`Terminal`}</dt>
          <dd>
            <code className="activity-code">{terminal.id}</code>
          </dd>
        </>
      ) : null}
      {terminal.title ? (
        <>
          <dt>{t`Ran as`}</dt>
          <dd>
            <code className="activity-code">{terminal.title}</code>
          </dd>
        </>
      ) : null}
      {terminal.openTime && terminal.end ? (
        <>
          <dt>{t({ message: "Opened", context: "terminal" })}</dt>
          <dd>{formatDateTime(timestampDate(terminal.openTime), i18n.locale)}</dd>
        </>
      ) : null}
      {terminal.end ? (
        <>
          <dt>{t`How it ended`}</dt>
          <dd>{ended(terminal.end).text}</dd>
        </>
      ) : null}
      {terminal.error ? (
        <>
          <dt>{t`Why`}</dt>
          <dd>{terminal.error}</dd>
        </>
      ) : null}
    </>
  );
}

/**
 * What a server's agent was sent to keep, in the facts of its event: how many
 * resources, and, for a cache new to the control plane, what that can mean and
 * what to do. Never a value.
 */
function DesiredSentFacts({ sent }: { sent: EventDesiredSent }) {
  const { t } = useLingui();
  const server = sent.serverName;
  return (
    <>
      {sent.unknown > 0 ? (
        <>
          <dt>{t`Not on record`}</dt>
          <dd>
            {t`The control plane's data was restored or rebuilt from before these were made or changed, so what ${server}'s agent keeps of them, values included, may be the only copy of what runs there. Apply them again from the control plane to bring its record up to date.`}
          </dd>
        </>
      ) : (
        <>
          <dt>{t`Resources sent`}</dt>
          <dd>{sent.resources}</dd>
          <dt>{t`With variables and secrets`}</dt>
          <dd>{sent.withValues}</dd>
        </>
      )}
      {sent.newCache ? (
        <>
          <dt>{t`New cache`}</dt>
          <dd>
            {t`${server}'s agent keeps its cache under a key it made since the control plane last sent it anything, as after a reinstall, or a data folder that was lost. If neither happened, something else may hold this server's identity: remove ${server} from the fleet and join it again, which gives it a new one.`}
          </dd>
        </>
      ) : null}
    </>
  );
}

/** A change of health, in the facts of an event: how it stood, where, and what its check said. */
function HealthFacts({ event }: { event: Event }) {
  const { t } = useLingui();
  const words = useHealthEventWords();
  const look = words.look(event);
  const flips = words.flips(event);
  return (
    <>
      <dt>{t`Server`}</dt>
      <dd>
        <code className="activity-code">
          {event.health?.serverId ? (
            <RefLink kind="server" id={event.health.serverId} look="plain">
              {event.health.serverName}
            </RefLink>
          ) : (
            event.health?.serverName
          )}
        </code>
      </dd>
      {look ? (
        <>
          <dt>{t`State`}</dt>
          <dd>
            <StatusBadge status={look.status} label={look.word} labelIsState />
          </dd>
        </>
      ) : null}
      {event.health?.output ? (
        <>
          <dt>{t`Its check said`}</dt>
          <dd>
            <MachineWords>{event.health.output}</MachineWords>
          </dd>
        </>
      ) : null}
      {flips ? (
        <>
          <dt>{t`Changes`}</dt>
          <dd>{flips}</dd>
        </>
      ) : null}
    </>
  );
}
