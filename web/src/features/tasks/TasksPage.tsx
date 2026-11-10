// SPDX-License-Identifier: AGPL-3.0-only

import { type TaskInfo, TaskKind } from "@dockwell/api";
import {
  Button,
  Callout,
  cx,
  Icon,
  type IconName,
  Panel,
  PHONE_QUERY,
  StatusBadge,
  Table,
  type TableColumn,
  useMediaQuery,
} from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { useNavigate, useSearch } from "@tanstack/react-router";
import { useState } from "react";

import { sentence } from "../../lib/errors";
import { useNow, useTimeWords } from "../../lib/time";
import { EmptyState } from "../shell/EmptyState";
import { Page } from "../shell/Page";
import { TaskPanel } from "./TaskPanel";
import { TaskSheet } from "./TaskSheet";
import { lastRunOf, nameOf, nextRunOf, offline, runStatus, targetOf, tookOf, useTasks, useTaskWords } from "./useTasks";
import "./Tasks.css";

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
export function TasksPage() {
  const { t } = useLingui();
  const phone = useMediaQuery(PHONE_QUERY);
  const query = useTasks();
  const words = useTaskWords();
  const time = useTimeWords();
  const now = useNow(30_000);
  const search = useSearch({ strict: false }) as { task?: string };
  const navigate = useNavigate();
  const [making, setMaking] = useState(false);
  const tasks = query.data?.tasks ?? [];
  const chosen = tasks.find((task) => task.id === search.task) ?? tasks[0];
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
      header: t`Runs in`,
      width: 176,
      render: (task) => (
        <span className="tasks-target">
          <Icon name={ICONS[task.kind] ?? "server"} size={16} />
          <span className="dw-cell">
            <span className={task.kind === TaskKind.APP ? undefined : "tasks-mono"}>{targetOf(task)}</span>
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
  ];

  const choose = (task: TaskInfo) =>
    void navigate({ to: "/scheduled-tasks", search: { task: task.id }, replace: true });
  const action = (
    <Button variant="primary" icon="plus" onClick={() => setMaking(true)}>
      <Trans>New task</Trans>
    </Button>
  );
  return (
    <Page title={t`Scheduled tasks`} lead={lead} phoneSub={lead} back="/more" actions={action}>
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
      {query.isPending ? null : count === 0 ? (
        // It draws its own card, so it stands alone, as on every other page.
        <EmptyState icon="calendar" title={t`No scheduled tasks yet`} action={action}>
          <Trans>
            A task runs on its server's own clock, so it runs while the control plane is away. Each run keeps what it
            printed, and one that fails is on Activity.
          </Trans>
        </EmptyState>
      ) : (
        <div className="tasks-columns">
          <div className="tasks-main">
            <section className="tasks-section" aria-labelledby="tasks-all">
              <div className="tasks-section-head">
                <h2 className="dw-text-heading" id="tasks-all">
                  <Trans>All tasks</Trans>
                </h2>
                {phone ? <span className="tasks-fill" /> : null}
                {phone ? action : null}
                {next ? (
                  <span className="tasks-sub">{t`Next: ${nameOf(next.task)} ${words.until(next.at, now)}`}</span>
                ) : null}
              </div>
              {phone ? (
                <ul className="tasks-phone">
                  {tasks.map((task) => {
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
                      </li>
                    );
                  })}
                </ul>
              ) : (
                <Table<TaskInfo>
                  columns={columns}
                  rows={tasks}
                  rowKey="id"
                  selectedKey={chosen?.id}
                  onRowClick={choose}
                  caption={t`Scheduled tasks`}
                />
              )}
            </section>
            <NextDay tasks={tasks} chosen={chosen} now={now} />
          </div>
          {chosen ? <TaskPanel task={chosen} /> : null}
        </div>
      )}
      {making ? <TaskSheet onClose={() => setMaking(false)} /> : null}
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
  const time = new Intl.DateTimeFormat(i18n.locale, { hour: "2-digit", minute: "2-digit", hourCycle: "h23" });
  // A phone's track has no room for the weekday, which the panel's head says anyway.
  const date = new Intl.DateTimeFormat(i18n.locale, {
    ...(phone ? {} : { weekday: "short" }),
    day: "numeric",
    month: "short",
  });
  const long = new Intl.DateTimeFormat(i18n.locale, {
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
