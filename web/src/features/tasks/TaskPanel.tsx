// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate } from "@bufbuild/protobuf/wkt";
import { createClient } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import { type TaskInfo, TaskKind, type TaskRun, TaskRunState, TaskService } from "@dockwell/api";
import { Button, Callout, Glyph, type GlyphShape, StatusBadge } from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { useQueryClient } from "@tanstack/react-query";
import { useState } from "react";

import { sentence } from "../../lib/errors";
import { formatDateTime } from "../../lib/format";
import { RouterButtonLink } from "../../lib/links";
import { useNow, useTimeWords } from "../../lib/time";
import { TaskSheet } from "./TaskSheet";
import { commandOf, nameOf, offline, runStatus, tookOf, useTaskRuns, useTaskWords } from "./useTasks";

/** How a run looks in the strip of the last 14: a shape and a tone, never colour alone. */
function mark(state: TaskRunState): { shape: GlyphShape; tone: string } {
  switch (state) {
    case TaskRunState.SUCCEEDED:
      return { shape: "circle", tone: "is-good" };
    case TaskRunState.RUNNING:
      return { shape: "spinner", tone: "is-running" };
    case TaskRunState.SKIPPED:
      return { shape: "ring", tone: "is-skipped" };
  }
  return { shape: "square", tone: "is-bad" };
}

/** One task, as Scheduled-Tasks has it beside the list. */
export function TaskPanel({ task }: { task: TaskInfo }) {
  const { t, i18n } = useLingui();
  const words = useTaskWords();
  const time = useTimeWords();
  const now = useNow(30_000);
  const transport = useTransport();
  const queries = useQueryClient();
  const runsQuery = useTaskRuns(task.id);
  const [editing, setEditing] = useState(false);
  const [running, setRunning] = useState(false);
  const [runFailure, setRunFailure] = useState<string>();
  const runs = runsQuery.data?.runs ?? [];
  const oldestFirst = [...runs].reverse();
  const good = runs.filter((r) => r.state === TaskRunState.SUCCEEDED).length;
  const bad = runs.filter((r) => runStatus(r.state) === "down").length;
  const newest = runs[0];
  const lastBad = runs.find((r) => runStatus(r.state) === "down");
  const spec = task.spec;
  const name = nameOf(task);
  const schedule = words.schedule(spec?.schedule ?? "");
  const timeout = time.span((spec?.timeoutSeconds ?? 600) * 1000);
  const retries = spec?.retries ?? 0;
  const where =
    task.kind === TaskKind.APP && spec?.runs.case === "app"
      ? t`App ${spec.runs.value.service}`
      : task.servers.map((s) => s.serverName).join(", ");
  const day = (r: TaskRun) => (r.startTime ? formatDateTime(timestampDate(r.startTime), i18n.locale) : "");
  /** How a run's last attempt ended: "exit 0", "HTTP 200". */
  const exited = (run: TaskRun) => {
    const last = run.attempts.at(-1);
    if (!last || run.state === TaskRunState.RUNNING) return "";
    if (last.httpStatus) return t`HTTP ${last.httpStatus}`;
    return last.exitCode === -1 ? t`stopped` : t`exit ${last.exitCode}`;
  };

  const runNow = async () => {
    setRunning(true);
    setRunFailure(undefined);
    try {
      await createClient(TaskService, transport).runTask({ id: task.id, server: task.servers[0]?.serverId ?? "" });
      await queries.invalidateQueries();
    } catch (err) {
      setRunFailure(sentence(err));
    } finally {
      setRunning(false);
    }
  };

  return (
    <section className="dw-panel tasks-panel" aria-label={name}>
      <div className="dw-panel-head">
        <div className="tasks-panel-title">
          <h2 className="dw-panel-title">{name}</h2>
          <span className="tasks-sub">
            {[words.kind(task), schedule ? schedule.charAt(0).toLowerCase() + schedule.slice(1) : spec?.schedule].join(
              " · ",
            )}
          </span>
        </div>
        {spec?.paused ? (
          <StatusBadge status="stopped" label={t`Paused`} />
        ) : (
          <StatusBadge status="healthy" label={t`Enabled`} />
        )}
      </div>
      <div className="dw-panel-body tasks-panel-body">
        <div className="tasks-block">
          <span className="tasks-label">{task.kind === TaskKind.REQUEST ? t`Request` : t`Command`}</span>
          <div className="tasks-command">
            <code>{commandOf(task)}</code>
            <Button
              variant="ghost"
              size="sm"
              icon="copy"
              aria-label={t`Copy command`}
              onClick={() => void navigator.clipboard?.writeText(commandOf(task))}
            />
          </div>
          <dl className="tasks-facts">
            <dt>{t`Schedule`}</dt>
            <dd>
              <code>{spec?.schedule}</code> · {spec?.timeZone || "UTC"}
            </dd>
            <dt>{t`Runs in`}</dt>
            <dd>{where}</dd>
            <dt>{t`Timeout`}</dt>
            <dd>
              {retries === 0
                ? timeout
                : plural(retries, { one: `${timeout}, then retry once`, other: `${timeout}, then retry # times` })}
            </dd>
          </dl>
        </div>
        {task.servers.some(offline) ? (
          <Callout tone="warning" title={t`Its server isn't connected`}>
            {t`It runs on its schedule all the same, and its runs show here once the server is back.`}
          </Callout>
        ) : null}
        <div className="tasks-block">
          <div className="tasks-row">
            <span className="tasks-label">{plural(runs.length, { one: "Last run", other: "Last # runs" })}</span>
            <span className="tasks-label">
              {runs.length === 0
                ? t`None yet`
                : `${t`${good} succeeded`} · ${plural(bad, { one: "# failed", other: "# failed" })}`}
            </span>
          </div>
          {runs.length > 0 ? (
            <>
              <ol className="tasks-strip" aria-label={t`Last runs, oldest first`}>
                {oldestFirst.map((r) => {
                  const m = mark(r.state);
                  const what = `${day(r)}, ${words.state(r.state).toLowerCase()}${r.error ? `: ${r.error}` : ""}`;
                  return (
                    <li key={r.id} title={what} className={`tasks-strip-run ${m.tone}`}>
                      <Glyph shape={m.shape} size={10} />
                      <span className="dw-sr">{what}</span>
                    </li>
                  );
                })}
              </ol>
              <div className="tasks-row tasks-label">
                <span>{oldestFirst[0] ? day(oldestFirst[0]) : ""}</span>
                {lastBad ? (
                  <span className="tasks-bad">{t`${day(lastBad)} ${words.state(lastBad.state).toLowerCase()}: ${lastBad.error}`}</span>
                ) : null}
              </div>
            </>
          ) : (
            <p className="tasks-quiet">
              <Trans>It hasn't run yet. Run it now to see what it does.</Trans>
            </p>
          )}
        </div>
        {newest ? (
          <div className="tasks-block">
            <div className="tasks-row">
              <span className="tasks-label">{t`Last output`}</span>
              <span className="tasks-label">
                {[
                  newest.startTime ? words.when(timestampDate(newest.startTime), now) : "",
                  tookOf(newest) === undefined ? words.state(newest.state) : words.took(tookOf(newest) ?? 0, time.span),
                  exited(newest),
                ]
                  .filter(Boolean)
                  .join(" · ")}
              </span>
            </div>
            <pre className="tasks-output">
              {newest.output.length === 0 ? (
                <span className="tasks-quiet">{t`It printed nothing.`}</span>
              ) : (
                newest.output.map((line, i) => (
                  // biome-ignore lint/suspicious/noArrayIndexKey: lines have no identity but their place
                  <span key={i} className="tasks-output-line">
                    <span className="tasks-output-time">
                      {line.time
                        ? new Intl.DateTimeFormat(i18n.locale, {
                            hour: "2-digit",
                            minute: "2-digit",
                            second: "2-digit",
                            hourCycle: "h23",
                          }).format(timestampDate(line.time))
                        : ""}
                    </span>
                    <span>{line.text}</span>
                  </span>
                ))
              )}
            </pre>
            {newest.outputCutBytes > 0n ? (
              <p className="tasks-quiet">{t`What it printed before this, ${newest.outputCutBytes} bytes, wasn't kept.`}</p>
            ) : null}
          </div>
        ) : null}
        {runFailure ? (
          <Callout tone="critical" title={t`It didn't run`}>
            {runFailure}
          </Callout>
        ) : null}
        <div className="tasks-actions">
          <Button icon="play" loading={running} onClick={() => void runNow()}>
            <Trans>Run now</Trans>
          </Button>
          <Button icon="edit" onClick={() => setEditing(true)}>
            <Trans>Edit</Trans>
          </Button>
          <span className="tasks-fill" />
          <RouterButtonLink to="/activity" search={{ resource: task.id }} variant="ghost">
            <Trans>All runs</Trans>
          </RouterButtonLink>
        </div>
      </div>
      {editing ? <TaskSheet task={task} onClose={() => setEditing(false)} /> : null}
    </section>
  );
}
