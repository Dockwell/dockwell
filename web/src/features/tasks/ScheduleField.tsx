// SPDX-License-Identifier: AGPL-3.0-only

import { SelectField, TextField } from "@dockwell/ui";
import { useLingui } from "@lingui/react/macro";
import { useId, useMemo, useState } from "react";

import { CRON_WORDS, nextRuns, parseCron } from "../../lib/cron";
import { useNow } from "../../lib/time";

/** How often, as a person picks it: each is one shape of cron, and anything else is cron itself. */
export type Shape =
  | { every: "minutes"; n: number }
  | { every: "hour"; minute: number }
  | { every: "day" | "weekday"; hour: number; minute: number }
  | { every: "week"; day: number; hour: number; minute: number }
  | { every: "month"; date: number; hour: number; minute: number }
  | { every: "cron" };

type Every = Shape["every"];

/** Every N minutes a person picks from: each divides an hour, so the runs fall evenly. */
const MINUTE_STEPS = [5, 10, 15, 20, 30];

const num = (s: string | undefined) => Number(s);
const two = (n: number) => String(n).padStart(2, "0");

/** What a schedule is in the picker's terms, or cron where it's none of them. */
export function shapeOf(cron: string): Shape {
  const text = (CRON_WORDS[cron.trim().toLowerCase()] ?? cron).trim().replace(/\s+/g, " ");
  const minute = "([0-9]|[1-5][0-9])";
  const hour = "([0-9]|1[0-9]|2[0-3])";
  let m = /^\*\/(\d+) \* \* \* \*$/.exec(text);
  if (m && MINUTE_STEPS.includes(num(m[1]))) return { every: "minutes", n: num(m[1]) };
  m = new RegExp(`^${minute} \\* \\* \\* \\*$`).exec(text);
  if (m) return { every: "hour", minute: num(m[1]) };
  m = new RegExp(`^${minute} ${hour} \\* \\* \\*$`).exec(text);
  if (m) return { every: "day", minute: num(m[1]), hour: num(m[2]) };
  m = new RegExp(`^${minute} ${hour} \\* \\* 1-5$`).exec(text);
  if (m) return { every: "weekday", minute: num(m[1]), hour: num(m[2]) };
  m = new RegExp(`^${minute} ${hour} \\* \\* ([0-7])$`).exec(text);
  if (m) return { every: "week", minute: num(m[1]), hour: num(m[2]), day: num(m[3]) % 7 };
  m = new RegExp(`^${minute} ${hour} ([1-9]|[12][0-9]|3[01]) \\* \\*$`).exec(text);
  if (m) return { every: "month", minute: num(m[1]), hour: num(m[2]), date: num(m[3]) };
  return { every: "cron" };
}

/** The cron a shape is. */
export function cronOf(shape: Exclude<Shape, { every: "cron" }>): string {
  switch (shape.every) {
    case "minutes":
      return `*/${shape.n} * * * *`;
    case "hour":
      return `${shape.minute} * * * *`;
    case "day":
      return `${shape.minute} ${shape.hour} * * *`;
    case "weekday":
      return `${shape.minute} ${shape.hour} * * 1-5`;
    case "week":
      return `${shape.minute} ${shape.hour} * * ${shape.day}`;
    case "month":
      return `${shape.minute} ${shape.hour} ${shape.date} * *`;
  }
}

/** The hour and minute a shape runs at, where it has one. */
const timeOf = (s: Shape) => ("hour" in s ? `${two(s.hour)}:${two(s.minute)}` : "04:00");

export interface ScheduleFieldProps {
  /** The schedule, in cron, as the task keeps it. */
  value: string;
  onChange: (cron: string) => void;
  /** The zone its runs read in, an IANA name such as Europe/Berlin. */
  zone: string;
  /** Says what's wrong with the cron typed, once the form has been sent. */
  error?: string;
}

/**
 * A schedule as a person says it: how often, and when, from lists, with the next
 * three runs beside it in the task's zone, read the way the control plane reads
 * cron. Anything the lists can't say is cron, one choice away, with the same
 * preview; the cron of what's picked is always shown, so it can be learned.
 */
export function ScheduleField({ value, onChange, zone, error }: ScheduleFieldProps) {
  const { t, i18n } = useLingui();
  const now = useNow(60_000);
  const id = useId();
  const shape = useMemo(() => shapeOf(value), [value]);
  // Cron, once chosen, stays cron until a person picks another kind, whatever is typed.
  const [custom, setCustom] = useState(shape.every === "cron");
  // In Custom, its one field stands in for the pickers of the shape the cron matches.
  const every: Every = custom ? "cron" : shape.every;

  const days = useMemo(() => {
    const f = new Intl.DateTimeFormat(i18n.locale, { weekday: "long", timeZone: "UTC" });
    // 4 January 1970 was a Sunday.
    return Array.from({ length: 7 }, (_, d) => f.format(new Date(Date.UTC(1970, 0, 4 + d))));
  }, [i18n.locale]);

  const pick = (next: Every) => {
    setCustom(next === "cron");
    const [hour = 4, minute = 0] = timeOf(shape).split(":").map(Number);
    switch (next) {
      case "minutes":
        return onChange(cronOf({ every: "minutes", n: 15 }));
      case "hour":
        return onChange(cronOf({ every: "hour", minute: "minute" in shape ? shape.minute : 0 }));
      case "day":
      case "weekday":
        return onChange(cronOf({ every: next, hour, minute }));
      case "week":
        return onChange(cronOf({ every: "week", day: 1, hour, minute }));
      case "month":
        return onChange(cronOf({ every: "month", date: 1, hour, minute }));
      case "cron":
        // The cron of what was picked, to change from there.
        return onChange(value.trim() || "0 4 * * *");
    }
  };
  const at = (time: string) => {
    const [h, m] = time.split(":").map(Number);
    if (h === undefined || m === undefined || Number.isNaN(h) || Number.isNaN(m)) return;
    if (shape.every === "day" || shape.every === "weekday" || shape.every === "week" || shape.every === "month") {
      onChange(cronOf({ ...shape, hour: h, minute: m }));
    }
  };

  const parsed = parseCron(value);
  // A zone the browser doesn't know, as one set from the CLI may be, leaves out the preview.
  const when = useMemo(() => {
    try {
      return new Intl.DateTimeFormat(i18n.locale, {
        timeZone: zone,
        weekday: "short",
        day: "numeric",
        month: "short",
        // The locale's own clock, as the time picker beside it shows it.
        hour: "2-digit",
        minute: "2-digit",
      });
    } catch {
      return undefined;
    }
  }, [i18n.locale, zone]);
  const runs = useMemo(() => {
    const schedule = parseCron(value);
    return schedule && when ? nextRuns(schedule, now, zone, 3) : [];
  }, [value, now, zone, when]);
  const typedError =
    every === "cron" && value.trim() !== "" && !parsed ? t`That isn't cron Dockwell reads.` : undefined;
  const shownError = error ?? typedError;

  return (
    <div className="schedule-field">
      <div className="schedule-row">
        <SelectField
          label={t`Schedule`}
          value={every}
          aria-describedby={`${id}-next`}
          options={[
            { value: "minutes", label: t`Every few minutes` },
            { value: "hour", label: t`Every hour` },
            { value: "day", label: t`Every day` },
            { value: "weekday", label: t`Every weekday` },
            { value: "week", label: t`Every week` },
            { value: "month", label: t`Every month` },
            { value: "cron", label: t`Custom, in cron` },
          ]}
          onChange={(event) => pick(event.target.value as Every)}
        />
        {!custom && shape.every === "minutes" ? (
          <SelectField
            label={t`Every`}
            value={String(shape.n)}
            options={MINUTE_STEPS.map((n) => ({ value: String(n), label: t`${n} minutes` }))}
            onChange={(event) => onChange(cronOf({ every: "minutes", n: Number(event.target.value) }))}
          />
        ) : null}
        {!custom && shape.every === "hour" ? (
          <SelectField
            label={t`At minute`}
            value={String(shape.minute)}
            options={Array.from({ length: 60 }, (_, m) => ({ value: String(m), label: `:${two(m)}` }))}
            onChange={(event) => onChange(cronOf({ every: "hour", minute: Number(event.target.value) }))}
          />
        ) : null}
        {!custom && shape.every === "week" ? (
          <SelectField
            label={t({ message: "On", context: "a day of the week" })}
            value={String(shape.day)}
            // Monday first, as most of the world's weeks start.
            options={[1, 2, 3, 4, 5, 6, 0].map((d) => ({ value: String(d), label: days[d] ?? "" }))}
            onChange={(event) => onChange(cronOf({ ...shape, day: Number(event.target.value) }))}
          />
        ) : null}
        {!custom && shape.every === "month" ? (
          <SelectField
            label={t`On day`}
            value={String(shape.date)}
            options={Array.from({ length: 31 }, (_, i) => ({ value: String(i + 1), label: String(i + 1) }))}
            onChange={(event) => onChange(cronOf({ ...shape, date: Number(event.target.value) }))}
          />
        ) : null}
        {!custom &&
        (shape.every === "day" || shape.every === "weekday" || shape.every === "week" || shape.every === "month") ? (
          <TextField
            label={t`At`}
            type="time"
            mono
            value={timeOf(shape)}
            onChange={(event) => at(event.target.value)}
          />
        ) : null}
      </div>
      {every === "cron" ? (
        <TextField
          label={t`Cron`}
          value={value}
          mono
          spellCheck={false}
          autoComplete="off"
          placeholder="0 4 * * *"
          error={shownError}
          hint={shownError ? undefined : t`Minute, hour, day of the month, month, day of the week.`}
          onChange={(event) => onChange(event.target.value)}
        />
      ) : null}
      <p className="schedule-next" id={`${id}-next`} aria-live="polite">
        {!when ? null : runs.length > 0 ? (
          <>
            <span>{t`Next, in ${zone}: ${runs.map((ms) => when.format(new Date(ms))).join(" · ")}`}</span>
            {every === "cron" ? null : <code className="schedule-cron">{value.trim()}</code>}
          </>
        ) : parsed && !shownError ? (
          <span>{t`No date matches it, so it never runs.`}</span>
        ) : null}
      </p>
    </div>
  );
}
