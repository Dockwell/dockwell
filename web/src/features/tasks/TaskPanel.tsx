// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate } from "@bufbuild/protobuf/wkt";
import { createClient } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import { ChangeService, type TaskInfo, TaskKind, type TaskRun, TaskRunState, TaskService } from "@dockwell/api";
import { Button, Callout, Glyph, type GlyphShape, SelectField, Switch } from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { useQueryClient } from "@tanstack/react-query";
import { useState } from "react";

import { sentence } from "../../lib/errors";
import { dayDiff, formatClockSeconds, formatDateTime, formatDay } from "../../lib/format";
import { RouterButtonLink } from "../../lib/links";
import { useNow, useTimeWords } from "../../lib/time";
import { useMay } from "../auth/may";
import { TaskSheet } from "./TaskSheet";
import { commandOf, nameOf, offline, runStatus, tookOf, useTaskRuns, useTaskWords } from "./useTasks";

/** How a run looks in the strip of the last 14: a shape and a tone, never colour alone. */
function mark(state: TaskRunState): { shape: GlyphShape; tone: string } {
  switch (state) {
    case TaskRunState.SUCCEEDED:
      return { shape: "circle", tone: "is-good" };
    case TaskRunState.RUNNING:
      return { shape: "spinner", tone: "is-running" };
    case TaskRunState.INTERRUPTED:
      return { shape: "ring", tone: "is-skipped" };
  }
  return { shape: "square", tone: "is-bad" };
}

/** One task, as Scheduled-Tasks has it beside the list. */
export function TaskPanel({
  task,
  onReview,
}: {
  task: TaskInfo;
  onReview: (action: "pause" | "resume" | "delete") => void;
}) {
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
  const [runOn, setRunOn] = useState(task.servers[0]?.serverId ?? "");
  const [runId, setRunId] = useState<string>();
  const runs = runsQuery.data?.runs ?? [];
  const oldestFirst = [...runs].reverse();
  const good = runs.filter((r) => r.state === TaskRunState.SUCCEEDED).length;
  const bad = runs.filter((r) => runStatus(r.state) === "down").length;
  const selected = runs.find((run) => run.id === runId) ?? runs[0];
  const transcript = selected ? words.transcript(selected, task, time.span) : [];
  const lastBad = runs.find((r) => runStatus(r.state) === "down");
  const spec = task.spec;
  const saved = !!task.resource && task.revision !== 0n;
  const name = nameOf(task);
  const schedule = words.schedule(spec?.schedule ?? "");
  const timeout = time.span((spec?.timeoutSeconds ?? 600) * 1000);
  const retries = spec?.retries ?? 0;
  const where =
    task.kind === TaskKind.APP && spec?.runs.case === "app"
      ? t`App ${spec.runs.value.service}`
      : task.servers.map((s) => s.serverName).join(", ");
  const day = (r: TaskRun) => (r.startTime ? formatDateTime(timestampDate(r.startTime), i18n.locale) : "");
  /** A run's day at an end of the strip, as Scheduled-Tasks labels it: "Today", "Yesterday", "13 Nov". */
  const end = (r: TaskRun | undefined) => {
    if (!r?.startTime) return "";
    const at = timestampDate(r.startTime);
    const days = dayDiff(new Date(now), at);
    return days === 0 ? t`Today` : days === 1 ? t`Yesterday` : formatDay(at, i18n.locale, now);
  };
  /** How a run's last attempt ended: "exit 0", "HTTP 200". */
  const exited = (run: TaskRun) => {
    const last = run.attempts.at(-1);
    if (!last || run.state === TaskRunState.RUNNING) return "";
    if (last.httpStatus) return t`HTTP ${last.httpStatus}`;
    return last.exitCode === -1 ? t`stopped` : t`exit ${last.exitCode}`;
  };

  const may = useMay();
  const scope = { environment: task.resource?.scope ?? "" };
  const mayRun = may(TaskService.method.runTask, scope);
  const mayEdit = may(ChangeService.method.proposeChange, scope);
  const runNow = async () => {
    setRunning(true);
    setRunFailure(undefined);
    try {
      await createClient(TaskService, transport).runTask({
        id: task.id,
        server: runOn || (task.servers[0]?.serverId ?? ""),
      });
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
        {spec ? (
          <Switch
            checked={!spec.paused}
            label={t`Enabled`}
            disabled={!mayEdit.allowed || !saved}
            description={mayEdit.reason || undefined}
            onChange={(enabled) => onReview(enabled ? "resume" : "pause")}
          />
        ) : null}
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
            {/* In its app, or on the servers it runs on: never a URL, which is the table's target (#796). */}
            <dt>{task.kind === TaskKind.APP ? t`Runs in` : t`Runs on`}</dt>
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
            <span className="tasks-label">
              {/* Before a first run there are none to count: "Last 0 runs" says less than the table's own head. */}
              {runs.length === 0 ? t`Last run` : plural(runs.length, { one: "Last run", other: "Last # runs" })}
            </span>
            <span className="tasks-label">
              {runs.length === 0
                ? t`None yet`
                : `${t`${good} succeeded`} · ${plural(bad, { one: "# failed", other: "# failed" })}`}
            </span>
            <RouterButtonLink to="/activity" search={{ resource: task.id }} variant="ghost" size="sm">
              <Trans>All runs</Trans>
            </RouterButtonLink>
          </div>
          {runs.length > 0 ? (
            <>
              <ol className="tasks-strip" aria-label={t`Last runs, oldest first`}>
                {oldestFirst.map((r) => {
                  const m = mark(r.state);
                  const why = r.state === TaskRunState.SUCCEEDED ? "" : words.why(r, task, time.span);
                  const state = words.state(r.state).toLowerCase();
                  const what = [day(r), task.servers.length > 1 ? r.serverName : "", why ? `${state}: ${why}` : state]
                    .filter(Boolean)
                    .join(" · ");
                  return (
                    <li key={r.id}>
                      <button
                        type="button"
                        className={`tasks-strip-run ${m.tone}`}
                        title={what}
                        aria-label={t`Show output: ${what}`}
                        aria-pressed={selected?.id === r.id}
                        onClick={() => setRunId(r.id)}
                      >
                        <Glyph shape={m.shape} size={10} />
                      </button>
                    </li>
                  );
                })}
              </ol>
              {/* The span of time the strip covers, its ends by their day, and the last failure between them. */}
              <div className="tasks-row tasks-label tasks-strip-ends">
                <span>{end(oldestFirst[0])}</span>
                {selected === runs[0] && lastBad?.startTime ? (
                  <span className="tasks-bad">{t`${words.when(timestampDate(lastBad.startTime), now)} ${words.state(lastBad.state).toLowerCase()}: ${words.why(lastBad, task, time.span)}`}</span>
                ) : null}
                {oldestFirst.length > 1 ? <span>{end(oldestFirst.at(-1))}</span> : null}
              </div>
            </>
          ) : (
            <p className="tasks-quiet">
              <Trans>It hasn't run yet. Run it now to see what it does.</Trans>
            </p>
          )}
        </div>
        {selected ? (
          <div className="tasks-block">
            <div className="tasks-row">
              <span className="tasks-label">{selected === runs[0] ? t`Last output` : t`Run output`}</span>
              <span className="tasks-label" aria-live="polite">
                {[
                  selected.startTime ? words.when(timestampDate(selected.startTime), now) : "",
                  tookOf(selected) === undefined
                    ? words.state(selected.state)
                    : words.took(tookOf(selected) ?? 0, time.span),
                  exited(selected),
                ]
                  .filter(Boolean)
                  .join(" · ")}
              </span>
            </div>
            <pre className="tasks-output">
              {transcript.length === 0 ? (
                <span className="tasks-quiet">{t`It printed nothing.`}</span>
              ) : (
                transcript.map((line, i) => (
                  // biome-ignore lint/suspicious/noArrayIndexKey: lines have no identity but their place
                  <span key={i} className={line.own ? "tasks-output-line is-own" : "tasks-output-line"}>
                    <span className="tasks-output-time">
                      {line.time ? formatClockSeconds(line.time, i18n.locale) : ""}
                    </span>
                    <span>{line.text}</span>
                  </span>
                ))
              )}
            </pre>
            {selected.outputCutBytes > 0n ? (
              <p className="tasks-quiet">{t`What it printed before this, ${selected.outputCutBytes} bytes, wasn't kept.`}</p>
            ) : null}
          </div>
        ) : null}
        {runFailure ? (
          <Callout tone="critical" title={t`It didn't run`}>
            {runFailure}
          </Callout>
        ) : null}
        {task.servers.length > 1 ? (
          <SelectField
            label={t`Run it now on`}
            value={runOn}
            options={task.servers.map((s) => ({ value: s.serverId, label: s.serverName }))}
            onChange={(event) => setRunOn(event.target.value)}
          />
        ) : null}
        <div className="tasks-actions">
          {/* Running one by hand can run a command as root, so it's an admin's (ADR 0049); off, with why, for anyone else. */}
          <Button icon="play" loading={running} onClick={() => void runNow()} {...mayRun.off}>
            <Trans>Run now</Trans>
          </Button>
          <Button icon="edit" onClick={() => setEditing(true)} {...mayEdit.off}>
            <Trans>Edit</Trans>
          </Button>
          {saved ? (
            <Button variant="danger" icon="trash" onClick={() => onReview("delete")} {...mayEdit.off}>
              <Trans>Delete task</Trans>
            </Button>
          ) : null}
        </div>
      </div>
      {editing ? <TaskSheet task={task} onClose={() => setEditing(false)} /> : null}
    </section>
  );
}
