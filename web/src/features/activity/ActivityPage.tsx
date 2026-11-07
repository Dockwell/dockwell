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
  EventKind,
  type Plan,
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
  PlanDiff,
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
import { type ReactNode, useCallback, useMemo, useRef } from "react";

import { sentence } from "../../lib/errors";
import { formatClock, formatDateTime, formatWeekday } from "../../lib/format";
import { RouterButtonLink } from "../../lib/links";
import { useTimeWords } from "../../lib/time";
import { initials } from "../auth/user";
import { useChangeSheet } from "../changes/ChangeSheet";
import { BecauseNotice, FailureNotice, MachineWords } from "../changes/FailureNotice";
import { useFieldRows } from "../changes/fields";
import { canUndo, diffResources, failedStep, foldPlumbing, shortFingerprint, wentOffline } from "../changes/plan";
import { RollbackLine, rollbackSubject, useRollbackWord } from "../changes/RollbackLine";
import { type StepLook, useAlsoWords, useChangeWords } from "../changes/words";
import { FailedDeployLogs } from "../logs/DeployLogLinks";
import { Reconnecting } from "../servers/FleetNotes";
import { useControlPlaneNow, useFleetView } from "../servers/useFleet";
import { EmptyState } from "../shell/EmptyState";
import { Page } from "../shell/Page";
import { type Described, happenedAt, useCounts, useDescribe, useHealthEventWords, useProgress } from "./describe";
import { type EventFilter, useEvents } from "./useEvents";
import "./ActivityPage.css";

/** The kinds of event Activity filters by, as its search names them. */
export type ActivityKind = "changes" | "servers" | "access" | "health";

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
  const days = useMemo(() => byDay(events ?? []), [events]);
  const selected = search.event ? events?.find((e) => e.id === search.event) : undefined;
  const select = (id: string | undefined) => onSearch({ ...search, event: id });
  // Below the list, where a narrow screen puts it, an event chosen comes into view.
  const layout = useRef<HTMLDivElement>(null);
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
          const count = day.events.length;
          const long = formatWeekday(day.date, i18n.locale);
          const meta = plural(count, { one: `${long} · # event`, other: `${long} · # events` });
          return (
            <section key={day.key} className="activity-day" aria-label={label}>
              <div className="activity-day-head">
                <h2 className="dw-text-heading">{label}</h2>
                <span className="activity-day-meta">{meta}</span>
              </div>
              {phone ? (
                <PhoneDay events={day.events} summaries={summaries} now={now} />
              ) : (
                <DayTable
                  events={day.events}
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
      lead={t`Every change, server event, access grant and change of health, newest first`}
      status={status}
      className="activity"
    >
      <div className="activity-filters">
        <SegmentedControl
          options={kindOptions}
          value={search.kind ?? "all"}
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

/** Events by the local day they happened on, newest day first, as they come. */
function byDay(events: readonly Event[]): Day[] {
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

/** What a change on record did, as the plan sheet reads it: what it was for, and what matters first. */
function RecordedPlan({
  plan,
  applied,
  rows,
}: {
  plan: Plan | undefined;
  applied: boolean;
  rows: ReturnType<typeof useFieldRows>;
}) {
  const words = useChangeWords();
  const alsoWords = useAlsoWords();
  const fold = foldPlumbing(plan, diffResources(plan, rows, { tag: (intent) => words.intentTag(intent, applied) }));
  return (
    <PlanDiff
      resources={fold.shown}
      also={
        fold.folded.length > 0
          ? { label: alsoWords(fold, applied), action: "create", resources: fold.folded }
          : undefined
      }
    />
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
      if (!change) return { look: undefined, parts: [words.via(event.surface)] };
      const parts: string[] = [];
      const steps = change.state === ChangeState.APPLYING ? progress(change) : "";
      const brief = steps || counts(change);
      if (brief) parts.push(brief);
      if (change.undoneBy) parts.push(t`Undone`);
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
  summaries,
  now,
  caption,
  selected,
  onSelect,
}: DayProps & { caption: string; selected: string | undefined; onSelect: (event: Event) => void }) {
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
        return (
          <div className="activity-event">
            <Mark icon={d.icon} tone={d.tone} />
            <div className="activity-event-text">
              <span className="activity-event-title">{d.title}</span>
              <MetaLine {...meta(event)} />
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

/** A day on a phone: one line of time, each change opening its plan sheet. */
function PhoneDay({ events, summaries, now }: DayProps) {
  const { t } = useLingui();
  const describe = useDescribe();
  const time = useTimeWords();
  const meta = useMeta();
  const sheet = useChangeSheet();
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
      undoable: canUndo(change, now),
      undoLabel: t`Undo`,
    };
  });
  return (
    <Timeline
      className="activity-timeline"
      events={items}
      onUndo={(item) => sheet.open({ kind: "undo", id: String(item.id) })}
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
  const { t } = useLingui();
  const marks: Partial<Record<ActorKind, IconName>> = {
    [ActorKind.TOKEN]: "key",
    [ActorKind.DOCKWELL]: "activity",
    [ActorKind.SERVER]: "server",
    [ActorKind.SHELL]: "terminal",
  };
  const mark = actor ? marks[actor.kind] : undefined;
  const name = actor?.name || t`Someone`;
  return (
    <span className="activity-actor">
      {mark ? (
        <span className="activity-badge" aria-hidden="true">
          <Icon name={mark} size={12} />
        </span>
      ) : (
        <span className="activity-avatar" aria-hidden="true">
          {initials(name)}
        </span>
      )}
      <span
        className={cx(actor?.kind === ActorKind.SERVER || actor?.kind === ActorKind.SHELL ? "is-machine" : undefined)}
      >
        {name}
      </span>
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
  described: Described;
  now: number;
  time: ReturnType<typeof useTimeWords>;
  onClose: () => void;
  onSelect: (id: string) => void;
  onOpen: (id: string) => void;
  onUndo: (id: string) => void;
}

/** The selected event beside the list: who, when, from where, and for a change what it changed. */
function Detail({ event, described, now, time, onClose, onSelect, onOpen, onUndo }: DetailProps) {
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
  const fileOn = event.action === "file.read" || event.action === "file.save" ? event.subject : undefined;
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
            {!change && event.subject ? (
              <>
                <dt>{t`About`}</dt>
                <dd>{event.subject.name || <code className="activity-code">{event.subject.id}</code>}</dd>
              </>
            ) : null}
            {event.health ? <HealthFacts event={event} /> : null}
          </dl>
          {change?.because ? <BecauseNotice because={change.because} /> : null}
          {change?.state === ChangeState.FAILED ? (
            <RollbackLine change={change} subject={rollbackSubject(full.data?.change?.plan ?? change.plan)} />
          ) : null}
          {change?.state === ChangeState.FAILED && change.error && !wentOffline(change) ? (
            <FailureNotice message={change.error} detail={failedStep(change)?.detail} />
          ) : null}
          {change?.state === ChangeState.FAILED && !wentOffline(change) ? (
            <FailedDeployLogs change={full.data?.change ?? change} />
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
                <Button icon="undo" onClick={() => onUndo(change.id)}>
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
          ) : toVariables || toFile ? (
            <div className="activity-detail-actions">
              {toVariables}
              {toFile}
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
        <code className="activity-code">{event.health?.serverName}</code>
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
