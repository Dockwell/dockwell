// SPDX-License-Identifier: AGPL-3.0-only

import { type Timestamp, timestampDate, timestampMs } from "@bufbuild/protobuf/wkt";
import { useQuery } from "@connectrpc/connect-query";
import {
  Connection,
  type TaskInfo,
  TaskKind,
  type TaskOnServer,
  type TaskRun,
  TaskRunState,
  TaskService,
} from "@dockwell/api";
import type { Status } from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { useLingui } from "@lingui/react/macro";
import { useMemo } from "react";

import { READ_EVERY_MS } from "../../lib/standing";

/** Every task, read again while a screen shows them. */
export function useTasks() {
  return useQuery(TaskService.method.listTasks, {}, { refetchInterval: READ_EVERY_MS });
}

/** A task's newest runs, with what each printed. */
export function useTaskRuns(id: string, size = 14) {
  return useQuery(
    TaskService.method.listTaskRuns,
    { id, pageSize: size },
    { enabled: id !== "", refetchInterval: READ_EVERY_MS, retry: false },
  );
}

/** What a task runs, in a word: its app's name, a server's, or a URL's host. */
export function targetOf(task: TaskInfo): string {
  const runs = task.spec?.runs;
  switch (runs?.case) {
    case "app":
      return runs.value.service;
    case "request":
      try {
        return new URL(runs.value.url).host;
      } catch {
        return runs.value.url;
      }
    default:
      return task.servers.map((s) => s.serverName).join(", ");
  }
}

/** What people call a task: what it says, or its name. */
export function nameOf(task: TaskInfo): string {
  return task.spec?.displayName || task.resource?.name || task.id;
}

/** The command or URL a task runs, as it's written. */
export function commandOf(task: TaskInfo): string {
  const runs = task.spec?.runs;
  switch (runs?.case) {
    case "shell":
      return runs.value.command;
    case "app":
      return runs.value.command;
    case "request":
      return `${runs.value.method || "GET"} ${runs.value.url}`;
  }
  return "";
}

/** The newest run of a task on any of its servers. */
export function lastRunOf(task: TaskInfo) {
  return task.servers
    .map((s) => s.lastRun)
    .filter((r) => r !== undefined)
    .sort((a, b) => Number(b.startTime?.seconds ?? 0n) - Number(a.startTime?.seconds ?? 0n))[0];
}

/** How long a run took, in milliseconds, once it has ended. */
export function tookOf(run: { startTime?: Timestamp; endTime?: Timestamp }): number | undefined {
  return run.startTime && run.endTime ? Math.max(0, timestampMs(run.endTime) - timestampMs(run.startTime)) : undefined;
}

/** When a task next runs, on any of its servers. */
export function nextRunOf(task: TaskInfo): Date | undefined {
  const times = task.servers.flatMap((s) => (s.nextRun ? [Number(s.nextRun.seconds) * 1000] : []));
  return times.length > 0 ? new Date(Math.min(...times)) : undefined;
}

/** A server that isn't connected now, whose runs show once it's back. */
export function offline(on: TaskOnServer): boolean {
  return on.connection !== Connection.CONNECTED;
}

/** A run's state as a badge shows it. */
export function runStatus(state: TaskRunState): Status {
  switch (state) {
    case TaskRunState.SUCCEEDED:
      return "healthy";
    case TaskRunState.RUNNING:
      return "deploying";
    case TaskRunState.FAILED:
    case TaskRunState.TIMED_OUT:
    case TaskRunState.NOT_STARTED:
      return "down";
    case TaskRunState.INTERRUPTED:
      return "stopped";
  }
  return "unknown";
}

/** The zone the viewer's clock reads in. */
export function viewerZone(): string {
  return Intl.DateTimeFormat().resolvedOptions().timeZone;
}

/** A line of a run's transcript: what it printed, or what happened to it, said here. */
export interface TranscriptLine {
  time?: Date;
  text: string;
  /** Dockwell's own, not the run's. */
  own: boolean;
}

/** How long a run's attempt took, in milliseconds. */
function attemptMs(a: TaskRun["attempts"][number]): number {
  return a.startTime && a.endTime ? Math.max(0, timestampMs(a.endTime) - timestampMs(a.startTime)) : 0;
}

/** Words for what tasks run, how runs went, and when. */
export function useTaskWords() {
  const { t, i18n } = useLingui();
  return useMemo(() => {
    const two = (n: number) => String(n).padStart(2, "0");
    const weekday = (d: number) =>
      // 1 November 2026 is a Sunday.
      new Intl.DateTimeFormat(i18n.locale, { weekday: "long", timeZone: "UTC" }).format(
        new Date(Date.UTC(2026, 10, 1 + (d % 7))),
      );
    const single = (field: string) => (/^\d+$/.test(field) ? Number(field) : undefined);
    return {
      /** What a task runs: "Shell command", "Command in api", "HTTP request". */
      kind(task: TaskInfo): string {
        switch (task.kind) {
          case TaskKind.SHELL:
            return t`Shell command`;
          case TaskKind.APP: {
            const app = task.spec?.runs.case === "app" ? task.spec.runs.value.service : "";
            return t`Command in ${app}`;
          }
          case TaskKind.REQUEST:
            return t`HTTP request`;
        }
        return "";
      },
      /** What kind of place it runs in: "Server", "App", "URL". */
      place(task: TaskInfo): string {
        switch (task.kind) {
          case TaskKind.APP:
            return t`App`;
          case TaskKind.REQUEST:
            return t`URL`;
        }
        return task.servers.length > 1 ? t`Servers` : t`Server`;
      },
      /**
       * A task's schedule in words, with its zone where that isn't the viewer's, as
       * the schedule reads in the task's own: "Every day at 04:00, Europe/Amsterdam time".
       */
      scheduleIn(task: TaskInfo): string {
        const said = this.schedule(task.spec?.schedule ?? "");
        const zone = task.spec?.timeZone || "UTC";
        if (!said || zone === viewerZone()) return said;
        return t`${said}, ${zone} time`;
      },
      /**
       * Why a run didn't succeed, as a clause that follows a colon, from its attempts
       * where they say, and the agent's own words otherwise, such as why it couldn't start.
       */
      why(run: TaskRun, task: TaskInfo | undefined, span: (ms: number) => string): string {
        const last = run.attempts.at(-1);
        switch (run.state) {
          case TaskRunState.INTERRUPTED:
            return t`the agent stopped while it ran`;
          case TaskRunState.TIMED_OUT: {
            const limit = span((task?.spec?.timeoutSeconds || 600) * 1000);
            return t`it took longer than its timeout, ${limit}`;
          }
          case TaskRunState.FAILED: {
            const runs = task?.spec?.runs;
            if (last?.httpStatus) {
              const status = last.httpStatus;
              const expected = runs?.case === "request" ? runs.value.expectStatus : 0;
              return expected
                ? t`it answered ${status}, and the task expects ${expected}`
                : t`it answered ${status}, and the task expects a 2xx`;
            }
            if (last && last.exitCode > 0) {
              const code = last.exitCode;
              return t`it exited with ${code}`;
            }
            break;
          }
        }
        return run.error;
      },
      /**
       * What a run printed, with what happened to it said here in place of the agent's
       * own lines: each try again, a timeout, what a request answered, and what it left.
       */
      transcript(run: TaskRun, task: TaskInfo | undefined, span: (ms: number) => string): TranscriptLine[] {
        const lines: TranscriptLine[] = run.output
          .filter((line) => !line.text.startsWith("Dockwell: "))
          .map((line) => ({ time: line.time ? timestampDate(line.time) : undefined, text: line.text, own: false }));
        const retries = task?.spec?.retries ?? 0;
        const runs = task?.spec?.runs;
        run.attempts.forEach((a, i) => {
          const start = a.startTime ? timestampDate(a.startTime) : undefined;
          const end = a.endTime ? timestampDate(a.endTime) : undefined;
          if (i > 0) lines.push({ time: start, text: t`Trying again, ${i} of ${retries}.`, own: true });
          if (runs?.case === "request") {
            const method = runs.value.method || "GET";
            const url = runs.value.url;
            const took = this.took(attemptMs(a), span);
            const status = a.httpStatus;
            const bytes = Number(a.responseBytes);
            lines.push({
              time: end,
              text: status
                ? t`${method} ${url} answered ${status} in ${took}, ${bytes} bytes.`
                : t`${method} ${url} didn't answer.`,
              own: true,
            });
          }
          if (a.timedOut) {
            const limit = span((task?.spec?.timeoutSeconds || 600) * 1000);
            lines.push({ time: end, text: t`It took longer than its timeout, ${limit}, and was stopped.`, own: true });
          }
          if (a.outputHeld) {
            lines.push({
              time: end,
              text: t`Something it started still holds its output, and runs on after it.`,
              own: true,
            });
          }
        });
        if (run.state === TaskRunState.INTERRUPTED) {
          lines.push({
            time: run.endTime ? timestampDate(run.endTime) : undefined,
            text: t`The agent stopped while it ran.`,
            own: true,
          });
        }
        if (run.skippedSlots > 0) {
          const count = run.skippedSlots;
          lines.push({
            time: run.endTime ? timestampDate(run.endTime) : undefined,
            text: plural(count, {
              one: "Its schedule came round once while it ran, which ran nothing, as its runs never overlap.",
              other: "Its schedule came round # times while it ran, which ran nothing, as its runs never overlap.",
            }),
            own: true,
          });
        }
        // In the order they came; what has no time stays where it is.
        return lines
          .map((line, i) => ({ line, i }))
          .sort((a, b) => (a.line.time?.getTime() ?? 0) - (b.line.time?.getTime() ?? 0) || a.i - b.i)
          .map(({ line }) => line);
      },
      /** A schedule in words, such as "Every day at 04:00", or "" where it isn't one of the common ones. */
      schedule(cron: string): string {
        const named: Record<string, string> = {
          "@hourly": "0 * * * *",
          "@daily": "0 0 * * *",
          "@midnight": "0 0 * * *",
          "@weekly": "0 0 * * 0",
          "@monthly": "0 0 1 * *",
          "@yearly": "0 0 1 1 *",
          "@annually": "0 0 1 1 *",
        };
        const fields = (named[cron.trim().toLowerCase()] ?? cron).trim().split(/\s+/);
        if (fields.length !== 5) return "";
        const [minute = "", hour = "", dom = "", month = "", dow = ""] = fields;
        const m = single(minute);
        const h = single(hour);
        const every = /^\*\/(\d+)$/.exec(minute);
        const anyDay = dom === "*" && month === "*" && dow === "*";
        if (minute === "*" && hour === "*" && anyDay) return t`Every minute`;
        if (every && hour === "*" && anyDay) {
          const count = Number(every[1]);
          return t`Every ${count} minutes`;
        }
        if (m === 0 && hour === "*" && anyDay) return t`Every hour`;
        if (m !== undefined && hour === "*" && anyDay) {
          const at = `:${two(m)}`;
          return t`Every hour at ${at}`;
        }
        const hours = /^\*\/(\d+)$/.exec(hour);
        if (m !== undefined && hours && anyDay) {
          const count = Number(hours[1]);
          if (m === 0) return t`Every ${count} hours`;
          const at = `:${two(m)}`;
          return t`Every ${count} hours at ${at}`;
        }
        if (m === undefined || h === undefined) return "";
        const at = `${two(h)}:${two(m)}`;
        if (anyDay) return t`Every day at ${at}`;
        if (dom === "*" && month === "*" && dow === "1-5") return t`Every weekday at ${at}`;
        const d = single(dow);
        if (dom === "*" && month === "*" && d !== undefined && d <= 7) {
          const day = weekday(d);
          return t`Every ${day} at ${at}`;
        }
        const date = single(dom);
        if (date !== undefined && month === "*" && dow === "*") return t`On day ${date} of every month at ${at}`;
        if (date !== undefined && single(month) !== undefined && dow === "*") {
          const on = new Intl.DateTimeFormat(i18n.locale, { day: "numeric", month: "long", timeZone: "UTC" }).format(
            new Date(Date.UTC(2026, Number(month) - 1, date)),
          );
          return t`Every year on ${on} at ${at}`;
        }
        return "";
      },
      /** A time to come, from now: "in 5 min", "in 12 h", "in 3 days". */
      until(then: Date, now: number): string {
        const ms = Math.max(0, then.getTime() - now);
        if (ms < 60_000) return t`in under a minute`;
        if (ms < 3_600_000) {
          const count = Math.round(ms / 60_000);
          return t`in ${count} min`;
        }
        if (ms < 86_400_000) {
          const count = Math.round(ms / 3_600_000);
          return t`in ${count} h`;
        }
        const count = Math.round(ms / 86_400_000);
        return t`in ${count} days`;
      },
      /** A day and time: "Today 15:00", "Tomorrow 02:30", or the date. */
      when(then: Date, now: number): string {
        const time = new Intl.DateTimeFormat(i18n.locale, {
          hour: "2-digit",
          minute: "2-digit",
          hourCycle: "h23",
        }).format(then);
        const day = (d: Date) => new Date(d.getFullYear(), d.getMonth(), d.getDate()).getTime();
        const days = Math.round((day(then) - day(new Date(now))) / 86_400_000);
        if (days === 0) return t`Today ${time}`;
        if (days === 1) return t`Tomorrow ${time}`;
        if (days === -1) return t`Yesterday ${time}`;
        const date = new Intl.DateTimeFormat(i18n.locale, { day: "numeric", month: "short" }).format(then);
        return `${date} ${time}`;
      },
      /** How long a run took: "180 ms" under a second, then as spans read. */
      took(ms: number, span: (ms: number) => string): string {
        const count = Math.round(ms);
        return ms < 1000 ? t`${count} ms` : span(ms);
      },
      /** How a run went, in a word. */
      state(state: TaskRunState): string {
        switch (state) {
          case TaskRunState.RUNNING:
            return t`Running`;
          case TaskRunState.SUCCEEDED:
            return t`Succeeded`;
          case TaskRunState.FAILED:
            return t`Failed`;
          case TaskRunState.TIMED_OUT:
            return t`Timed out`;
          case TaskRunState.INTERRUPTED:
            return t`Interrupted`;
          case TaskRunState.NOT_STARTED:
            return t`Didn't start`;
        }
        return "";
      },
    };
  }, [t, i18n]);
}
