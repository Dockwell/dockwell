// SPDX-License-Identifier: AGPL-3.0-only

import { type Backup, BackupMethod, type BackupPolicy, BackupState, BackupPolicy_Every as Every } from "@dockwell/api";
import type { Status } from "@dockwell/ui";
import { useLingui } from "@lingui/react/macro";
import { useCallback } from "react";

import { formatDateTime } from "../../lib/format";
import { useTimeWords } from "../../lib/time";

/** How a backup's state reads. */
export function backupStatus(backup: Backup): Status {
  switch (backup.state) {
    case BackupState.DONE:
      return backup.method === BackupMethod.COPY_RUNNING ? "degraded" : "healthy";
    case BackupState.FAILED:
      return "down";
    case BackupState.RUNNING:
      return "deploying";
  }
  return "unknown";
}

/** Words for how a backup was taken, which say what it's worth. */
export function useMethodWord(): (method: BackupMethod, database: string) => string {
  const { t } = useLingui();
  return useCallback(
    (method, database) => {
      switch (method) {
        case BackupMethod.DUMP:
          return t`${database} dump`;
        case BackupMethod.COPY:
          return t`Exact copy`;
        case BackupMethod.COPY_RUNNING:
          return t`Copy of a running app`;
      }
      return "";
    },
    [t],
  );
}

/** A backup's state, in a word. */
export function useStateWord(): (backup: Backup) => string {
  const { t } = useLingui();
  return useCallback(
    (backup) => {
      switch (backup.state) {
        case BackupState.DONE:
          return t`Done`;
        case BackupState.FAILED:
          return t`Failed`;
        case BackupState.RUNNING:
          return t`Running`;
      }
      return t`Unknown`;
    },
    [t],
  );
}

/** The days of the week from Sunday, as a schedule numbers them. */
export function useWeekdays(): string[] {
  const { i18n } = useLingui();
  const format = new Intl.DateTimeFormat(i18n.locale, { weekday: "long", timeZone: "UTC" });
  // 4 Jan 1970 was a Sunday.
  return Array.from({ length: 7 }, (_, day) => format.format(new Date(Date.UTC(1970, 0, 4 + day))));
}

/** A schedule, for people, such as "Daily at 03:00". */
export function useScheduleWords(): (spec: BackupPolicy) => string {
  const { t } = useLingui();
  const weekdays = useWeekdays();
  return useCallback(
    (spec) => {
      const at = spec.at;
      const minutes = at.slice(3);
      const day = weekdays[spec.weekday] ?? "";
      switch (spec.every) {
        case Every.HOUR:
          return t`Hourly at ${minutes} past`;
        case Every.WEEK:
          return t`Weekly on ${day} at ${at}`;
      }
      return t`Daily at ${at}`;
    },
    [t, weekdays],
  );
}

/** What a schedule of a volume is called: a name that fits in a URL, whatever the volume's has. */
export function policyName(volume: string, how: string): string {
  const name = volume
    .toLowerCase()
    .replace(/[^a-z0-9-]+/g, "-")
    .replace(/^-+|-+$/g, "");
  const every = { hour: "hourly", day: "daily", week: "weekly" }[how] ?? how;
  return /^[a-z]/.test(name) ? `${name.slice(0, 50)}-${every}` : `backup-${name.slice(0, 50)}-${every}`;
}

const MINUTE = 60_000;
const HOUR = 60 * MINUTE;
const DAY = 24 * HOUR;

/**
 * When a run was or will be, as a person reads it: "5 min ago" or "in 7 h" within a
 * day, the date past that, in zone where a schedule keeps one (#793).
 */
export function useWhen(): (date: Date, now: number, zone?: string) => string {
  const { t, i18n } = useLingui();
  const time = useTimeWords();
  return useCallback(
    (date, now, zone) => {
      const ms = date.getTime() - now;
      if (ms < 0 && -ms < DAY) return time.ago(date, now);
      if (ms >= 0 && ms < HOUR) {
        const minutes = Math.max(1, Math.round(ms / MINUTE));
        return t`in ${minutes} min`;
      }
      if (ms >= 0 && ms < DAY) {
        const hours = Math.round(ms / HOUR);
        return t`in ${hours} h`;
      }
      return formatDateTime(date, i18n.locale, zone);
    },
    [t, i18n.locale, time],
  );
}
