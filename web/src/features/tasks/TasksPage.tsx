// SPDX-License-Identifier: AGPL-3.0-only

import { ChangeService, type TaskInfo, TaskKind } from "@dockwell/api";
import {
  Button,
  Callout,
  cx,
  Icon,
  type IconName,
  Loading,
  Panel,
  PHONE_QUERY,
  SelectField,
  SkeletonRows,
  StatusBadge,
  Table,
  type TableColumn,
  useMediaQuery,
} from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";

import { useEffect, useState } from "react";
import { sentence } from "../../lib/errors";
import { dateLocale } from "../../lib/format";
import { useNow, useTimeWords } from "../../lib/time";
import { ActionMenu, type MenuAction } from "../shell/ActionMenu";
import { EmptyState } from "../shell/EmptyState";
import { ListPager, ListToolbar, sortableColumns, useListPlace, useRowMenu } from "../shell/ListTools";
import { type ListSearch, pagedRows, sortedRows } from "../shell/list";
import { Page } from "../shell/Page";
import { TaskPanel } from "./TaskPanel";
import { TaskSheet } from "./TaskSheet";
import { lastRunOf, nameOf, nextRunOf, offline, runStatus, targetOf, tookOf, useTasks, useTaskWords } from "./useTasks";
import "./Tasks.css";
import { useServicesNamed } from "../apps/useServices";
import { useMay } from "../auth/may";
import { RefLink } from "../shell/RefLink";

export interface TasksSearch extends ListSearch {
  task?: string;
  kind?: TaskKind;
}

const SORT_KEYS = ["name", "schedule", "target", "last", "next"] as const;

const ICONS: Readonly<Record<number, IconName>> = {
  [TaskKind.SHELL]: "server",
  [TaskKind.APP]: "service",
  [TaskKind.REQUEST]: "globe",
};

/**
 * Scheduled tasks, to Scheduled-Tasks: every task with its schedule in words and as
 * cron, where it runs, and how its last run went; the next 24 hours as a timeline;
 * and the chosen task's command, last runs and what it printed (ADR 0053).
 */
export function TasksPage({ search, onSearch }: { search: TasksSearch; onSearch: (next: TasksSearch) => void }) {
  const { t } = useLingui();
  const phone = useMediaQuery(PHONE_QUERY);
  const query = useTasks();
  const words = useTaskWords();
  const time = useTimeWords();
  const now = useNow(30_000);
  const place = useListPlace(search, onSearch, SORT_KEYS);
  const context = useRowMenu<TaskInfo>();
  const [editing, setEditing] = useState<TaskInfo>();
  const choose = (task: TaskInfo) => onSearch({ ...search, task: task.id });
  const actionsFor = (task: TaskInfo): MenuAction[] => [
    { id: "open", label: t`Open`, onSelect: () => choose(task) },
    {
      id: "edit",
      label: t`Edit`,
      icon: "edit",
      ...(!mayPlan.allowed && { disabled: true, off: mayPlan.reason }),
      onSelect: () => setEditing(task),
    },
  ];
  const [making, setMaking] = useState(false);
  // A task is a plan, which is a developer's: off, with why, for a viewer (ADR 0059).
  const mayPlan = useMay()(ChangeService.method.proposeChange);
  // A task just made, to open once the list has it, where Run now is.
  const [made, setMade] = useState<{ name: string; scope: string }>();
  const tasks = query.data?.tasks ?? [];
  const listed = sortedRows(
    tasks.filter(
      (task) =>
        (!search.kind || task.kind === search.kind) &&
        `${nameOf(task)} ${targetOf(task)} ${words.kind(task)}`.toLowerCase().includes(place.query),
    ),
    place.sort,
    (task, key) => {
      switch (key) {
        case "schedule":
          return task.spec?.schedule;
        case "target":
          return targetOf(task);
        case "last":
          return lastRunOf(task)?.startTime ? Number(lastRunOf(task)?.startTime?.seconds) : undefined;
        case "next":
          return nextRunOf(task)?.getTime();
        default:
          return nameOf(task);
      }
    },
  );
  const page = pagedRows(listed, place.page);
  const chosen = tasks.find((task) => task.id === search.task) ?? page.rows[0];
  const kinds = [...new Map(tasks.map((task) => [task.kind, words.kind(task)])).entries()];
  const servers = [...new Set(tasks.flatMap((task) => task.servers.map((s) => s.serverName)))];
  const failedLately = tasks.filter((task) => {
    const last = lastRunOf(task);
    return last && runStatus(last.state) === "down" && Number(last.startTime?.seconds ?? 0n) * 1000 > now - 86_400_000;
  }).length;
  const away = [...new Set(tasks.flatMap((task) => task.servers.filter(offline).map((s) => s.serverName)))];
  const next = tasks
    .map((task) => ({ task, at: nextRunOf(task) }))
    .filter((n): n is { task: TaskInfo; at: Date } => n.at !== undefined)
    .sort((a, b) => a.at.getTime() - b.at.getTime())[0];

  const count = tasks.length;
  const where =
    servers.length === 1 ? (servers[0] ?? "") : plural(servers.length, { one: "# server", other: "# servers" });
  const lead =
    count === 0
      ? t`Run a shell command, a command in an app or an HTTP request on a schedule`
      : [
          plural(count, { one: `# task on ${where}`, other: `# tasks on ${where}` }),
          failedLately === 0
            ? t`no failures in the last 24 h`
            : plural(failedLately, { one: "# failed in the last 24 h", other: "# failed in the last 24 h" }),
        ].join(" · ");

  const columns: TableColumn<TaskInfo>[] = [
    {
      key: "name",
      header: t`Task`,
      render: (task) => (
        <span className="dw-cell">
          <span className="tasks-name">{nameOf(task)}</span>
          <span className="tasks-sub">{words.kind(task)}</span>
        </span>
      ),
    },
    {
      key: "schedule",
      header: t`Schedule`,
      width: 168,
      render: (task) => (
        <span className="dw-cell">
          <span>{task.spec?.paused ? t`Paused` : words.scheduleIn(task) || t`On its own schedule`}</span>
          <code className="tasks-sub">{task.spec?.schedule}</code>
        </span>
      ),
    },
    {
      key: "target",
      // What it runs or asks: an app, a command's server, or a URL, each a target (#796).
      header: t`Target`,
      width: 176,
      render: (task) => (
        <span className="tasks-target">
          <Icon name={ICONS[task.kind] ?? "server"} size={16} />
          <span className="dw-cell">
            <span className={task.kind === TaskKind.APP ? undefined : "tasks-mono"}>
              <Target task={task} />
            </span>
            <span className="tasks-sub">{words.place(task)}</span>
          </span>
        </span>
      ),
    },
    {
      key: "last",
      header: t`Last run`,
      width: 132,
      render: (task) => {
        const last = lastRunOf(task);
        if (!last) return <span className="tasks-sub">{t`Not yet`}</span>;
        const ms = tookOf(last);
        const took = ms === undefined ? "" : words.took(ms, time.span);
        return (
          <span className="dw-cell">
            <StatusBadge status={runStatus(last.state)} label={words.state(last.state)} />
            {took ? <span className="tasks-sub tasks-indent">{took}</span> : null}
          </span>
        );
      },
    },
    {
      key: "next",
      header: t`Next run`,
      width: 132,
      render: (task) => {
        const at = nextRunOf(task);
        if (!at) return <span className="tasks-sub">{task.spec?.paused ? t`Paused` : t`Not scheduled`}</span>;
        return (
          <span className="dw-cell">
            <span>{words.when(at, now)}</span>
            <span className="tasks-sub">{words.until(at, now)}</span>
          </span>
        );
      },
    },
    {
      key: "actions",
      header: <span className="dw-sr">{t`Actions`}</span>,
      width: 48,
      render: (task) => <ActionMenu label={t`Actions for ${nameOf(task)}`} actions={actionsFor(task)} />,
    },
  ];

  const madeId = made
    ? tasks.find((task) => task.resource?.name === made.name && task.resource?.scope === made.scope)?.id
    : undefined;
  useEffect(() => {
    if (!madeId) return;
    setMade(undefined);
    onSearch({ ...search, task: madeId });
  }, [madeId, onSearch, search]);
  const action = (
    <Button variant="primary" icon="plus" onClick={() => setMaking(true)} {...mayPlan.off}>
      <Trans>New task</Trans>
    </Button>
  );
  return (
    <Page
      title={t`Scheduled tasks`}
      lead={lead}
      phoneSub={lead}
      back="/more"
      // With none yet the empty state offers it, and a view has one primary button.
      actions={query.isPending || count === 0 ? null : action}
      phoneAction={
        // A phone's header has it as the Servers page has Add server: an icon, for whoever may use it.
        query.isPending || count === 0 || !mayPlan.allowed ? undefined : (
          <button
            type="button"
            className="mobile-header-button"
            aria-label={t`New task`}
            onClick={() => setMaking(true)}
          >
            <Icon name="plus" size={20} />
          </button>
        )
      }
    >
      {query.error ? (
        <Callout tone="critical" title={t`Dockwell can't read its scheduled tasks`}>
          {sentence(query.error)}
        </Callout>
      ) : null}
      {away.length > 0 ? (
        <Callout
          tone="warning"
          title={plural(away.length, { one: "# server isn't connected", other: "# servers aren't connected" })}
        >
          {t`${away.join(", ")} runs its tasks all the same, on its own clock. Its runs show here once it's back.`}
        </Callout>
      ) : null}
      {query.isPending ? (
        <section className="tasks-section">
          <div className="tasks-section-head">
            <h2 className="dw-text-heading">
              <Trans>All tasks</Trans>
            </h2>
          </div>
          {phone ? (
            <Loading label={t`Loading the tasks`}>
              <SkeletonRows rows={3} end="value" className="dw-panel" />
            </Loading>
          ) : (
            <Table<TaskInfo>
              columns={columns}
              rows={[]}
              caption={t`Scheduled tasks`}
              loading
              loadingLabel={t`Loading the tasks`}
            />
          )}
        </section>
      ) : count === 0 ? (
        // It draws its own card, so it stands alone, as on every other page.
        <EmptyState
          icon="calendar"
          title={t`No scheduled tasks yet`}
          action={
            <Button variant="primary" size="lg" icon="plus" onClick={() => setMaking(true)} {...mayPlan.off}>
              <Trans>New task</Trans>
            </Button>
          }
        >
          <Trans>Each runs on its server's own clock, even while the control plane is away.</Trans>
        </EmptyState>
      ) : (
        <div className="tasks-columns">
          <div className="tasks-main">
            <section className="tasks-section" aria-labelledby="tasks-all">
              <div className="tasks-section-head">
                <h2 className="dw-text-heading" id="tasks-all">
                  <Trans>All tasks</Trans>
                </h2>
                {next ? (
                  <span className="tasks-sub">{t`Next: ${nameOf(next.task)} ${words.until(next.at, now)}`}</span>
                ) : null}
              </div>
              <ListToolbar place={place} label={t`Filter tasks`}>
                {kinds.length > 1 || search.kind ? (
                  <SelectField
                    aria-label={t`Task type`}
                    size="sm"
                    value={String(search.kind ?? "")}
                    options={[
                      { value: "", label: t`Every type` },
                      ...kinds.map(([kind, label]) => ({ value: String(kind), label })),
                    ]}
                    onChange={(event) =>
                      onSearch({
                        ...search,
                        page: undefined,
                        kind: (Number(event.target.value) || undefined) as TaskKind | undefined,
                      })
                    }
                  />
                ) : null}
              </ListToolbar>
              {listed.length === 0 ? (
                <EmptyState
                  icon="search"
                  title={t`Nothing matches`}
                  action={
                    <Button
                      onClick={() => {
                        place.clearQuery();
                        onSearch({ ...search, q: undefined, kind: undefined, page: undefined });
                      }}
                    >
                      <Trans>Clear the filter</Trans>
                    </Button>
                  }
                />
              ) : phone ? (
                <ul className="tasks-phone">
                  {page.rows.map((task) => {
                    const last = lastRunOf(task);
                    const at = nextRunOf(task);
                    return (
                      <li key={task.id}>
                        <button
                          type="button"
                          className="tasks-phone-row"
                          aria-current={task === chosen}
                          onClick={() => choose(task)}
                        >
                          <span className="tasks-name">{nameOf(task)}</span>
                          <span className="tasks-sub">
                            {[words.scheduleIn(task) || task.spec?.schedule, at ? words.until(at, now) : ""]
                              .filter(Boolean)
                              .join(" · ")}
                          </span>
                          {last ? <StatusBadge status={runStatus(last.state)} label={words.state(last.state)} /> : null}
                        </button>
                        <ActionMenu label={t`Actions for ${nameOf(task)}`} actions={actionsFor(task)} />
                      </li>
                    );
                  })}
                </ul>
              ) : (
                <Table<TaskInfo>
                  columns={sortableColumns(columns, place, SORT_KEYS)}
                  rows={page.rows}
                  rowKey="id"
                  selectedKey={chosen?.id}
                  onRowClick={choose}
                  onRowContextMenu={(task, at, row) =>
                    context.open(task, at, row, {
                      label: t`Actions for ${nameOf(task)}`,
                      actions: actionsFor(task),
                    })
                  }
                  caption={t`Scheduled tasks`}
                />
              )}
              <ListPager page={page} onPage={place.setPage} />
              {context.element}
            </section>
            <NextDay tasks={page.rows} chosen={chosen} now={now} />
          </div>
          {chosen ? <TaskPanel task={chosen} /> : null}
        </div>
      )}
      {editing ? <TaskSheet task={editing} onClose={() => setEditing(undefined)} /> : null}
      {making ? (
        <TaskSheet onClose={() => setMaking(false)} onMade={(name, scope) => setMade({ name, scope })} />
      ) : null}
    </Page>
  );
}

/** How far along the track, in percent, an axis label has to be to clear the word Now at its start. */
const NOW_ROOM = 12;

/** The next 24 hours, a lane for each task with a mark at each run. */
function NextDay({ tasks, chosen, now }: { tasks: readonly TaskInfo[]; chosen: TaskInfo | undefined; now: number }) {
  const { t, i18n } = useLingui();
  const phone = useMediaQuery(PHONE_QUERY);
  const day = 86_400_000;
  const at = (ms: number) => Math.min(100, Math.max(0, ((ms - now) / day) * 100));
  const hours = (h: number) => {
    const d = new Date(now);
    d.setHours(h, 0, 0, 0);
    if (d.getTime() <= now) d.setDate(d.getDate() + 1);
    return d;
  };
  const time = new Intl.DateTimeFormat(dateLocale(i18n.locale), {
    hour: "2-digit",
    minute: "2-digit",
    hourCycle: "h23",
  });
  // A phone's track has no room for the weekday, which the panel's head says anyway.
  const date = new Intl.DateTimeFormat(dateLocale(i18n.locale), {
    ...(phone ? {} : { weekday: "short" }),
    day: "numeric",
    month: "short",
  });
  const long = new Intl.DateTimeFormat(dateLocale(i18n.locale), {
    weekday: "long",
    hour: "2-digit",
    minute: "2-digit",
    hourCycle: "h23",
  });
  const from = long.format(now);
  const to = long.format(now + day);
  const span = t`${from} to ${to}`;
  const axis = [6, 12, 18, 0].map((h) => {
    const d = hours(h);
    const where = at(d.getTime());
    // One that would stand on Now gives way to it: the hour is minutes off, and Now says so.
    return { at: where, label: h === 0 ? date.format(d) : time.format(d), strong: h === 0, hidden: where < NOW_ROOM };
  });
  return (
    <Panel title={t`Next 24 hours`} actions={<span className="tasks-sub">{span}</span>}>
      <div className="tasks-lanes">
        {tasks.map((task) => {
          const marks = task.servers.flatMap((s) => s.upcoming.map((u) => Number(u.seconds) * 1000));
          const dense = marks.length > 48;
          const first = marks[0];
          const runs = marks.length;
          return (
            <div key={task.id} className={task === chosen ? "tasks-lane is-chosen" : "tasks-lane"}>
              <span className="tasks-lane-name">
                <span className="tasks-lane-title">{nameOf(task)}</span>
                {runs > 1 ? <span className="tasks-sub">{plural(runs, { one: "# run", other: "# runs" })}</span> : null}
              </span>
              <div className="tasks-lane-track">
                {dense ? (
                  <span className="tasks-lane-band" />
                ) : (
                  marks.map((ms) => <span key={ms} className="tasks-lane-mark" style={{ left: `${at(ms)}%` }} />)
                )}
                {runs === 1 && first !== undefined ? (
                  // Near the end of the day, the label goes before its mark, so it stays on the track.
                  <span
                    className={at(first) > 75 ? "tasks-lane-label is-before" : "tasks-lane-label"}
                    style={at(first) > 75 ? { right: `${100 - at(first)}%` } : { left: `${at(first)}%` }}
                  >
                    {time.format(first)}
                  </span>
                ) : null}
              </div>
            </div>
          );
        })}
        <div className="tasks-lane tasks-axis">
          <span />
          <div className="tasks-lane-track is-axis">
            <span className="tasks-axis-now">{t`Now`}</span>
            {axis.map((a) => (
              <span
                key={a.label}
                className={cx("tasks-axis-label", a.strong && "is-strong", a.hidden && "is-hidden")}
                // A label anchors by its position, from left-aligned to right-aligned, to stay on the track.
                style={{ left: `${a.at}%`, transform: `translateX(-${a.at}%)` }}
              >
                {a.label}
              </span>
            ))}
          </div>
        </div>
      </div>
    </Panel>
  );
}

/**
 * What a task runs in, each name a link to its page: its app, or the servers its
 * command runs on. A request's host is somewhere else, and stays words.
 */
function Target({ task }: { task: TaskInfo }) {
  const named = useServicesNamed(task.resource?.scope);
  const runs = task.spec?.runs;
  if (runs?.case === "app") {
    const app = named.get(runs.value.service);
    if (!app) return <>{runs.value.service}</>;
    return (
      <RefLink kind="app" id={app.id} look="plain">
        {runs.value.service}
      </RefLink>
    );
  }
  if (runs?.case === "request" || task.servers.length === 0) return <>{targetOf(task)}</>;
  return (
    <>
      {task.servers.map((s, i) => (
        <span key={s.serverId}>
          {i > 0 ? ", " : null}
          <RefLink kind="server" id={s.serverId} look="plain">
            {s.serverName}
          </RefLink>
        </span>
      ))}
    </>
  );
}
