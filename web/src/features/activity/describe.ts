// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate } from "@bufbuild/protobuf/wkt";
import {
  ActorKind,
  AlertRule_Condition,
  AlertRule_Severity,
  type Change,
  ChangeState,
  type Event,
  EventKind,
  HealthState,
  IntentKind,
  Role,
  StepState,
} from "@dockwell/api";
import type { IconName, Status } from "@dockwell/ui";
import type { I18n } from "@lingui/core";
import { msg, plural } from "@lingui/core/macro";
import { useLingui } from "@lingui/react/macro";
import { useMemo } from "react";

import { formatBytes, formatDateTime } from "../../lib/format";
import { useTimeWords } from "../../lib/time";
import { useRolePhrase } from "../auth/user";

/** How an event shows on the timeline. */
export interface Described {
  title: string;
  icon: IconName;
  /** Only for an outcome worth noticing; routine events stay neutral. */
  tone?: "warning" | "critical" | "cobalt";
}

/**
 * Says each event in the active locale: its title, mark and tone. A change
 * is named by its summary, as its author wrote it; an undo or rollback by
 * the summary of the change it reverts, once that is known.
 */
export function useDescribe(): (event: Event, summaries: ReadonlyMap<string, string>) => Described {
  const { t, i18n } = useLingui();
  const time = useTimeWords();
  const rolePhrase = useRolePhrase();
  return useMemo(
    () => (event: Event, summaries: ReadonlyMap<string, string>) => {
      const change = event.change;
      if (event.kind === EventKind.CHANGE && change) return describeChange(change, summaries, i18n);
      const name = event.subject?.name ?? "";
      if (event.kind === EventKind.HEALTH) return describeHealth(event, i18n);
      if (event.kind === EventKind.ALERT) return describeAlert(event, i18n);
      const volume = event.backup?.volume ?? name;
      if (event.action === "backup.taken") return { title: t`Backed up ${volume}`, icon: "archive" };
      if (event.action === "backup.failed") {
        return { title: t`The backup of ${volume} failed`, icon: "archive", tone: "critical" };
      }
      if (event.action === "backup.deleted") return { title: t`Deleted a backup of ${volume}`, icon: "archive" };
      // An app opened through the panel, on an origin of its own (ADR 0061).
      if (event.action === "service.open") {
        return {
          title: name ? t`Opened ${name} through the panel` : t`Opened an app through the panel`,
          icon: "external",
        };
      }
      const destination = event.backup?.destination ?? "";
      if (event.action === "backup.copy_failed") {
        return { title: t`The backup of ${volume} didn't reach ${destination}`, icon: "archive", tone: "critical" };
      }
      if (event.action === "backup.fetched") {
        return { title: t`Fetched a backup of ${volume} from ${destination}`, icon: "archive" };
      }
      if (event.action === "backup.fetch_failed") {
        return {
          title: t`A backup of ${volume} couldn't be fetched from ${destination}`,
          icon: "archive",
          tone: "critical",
        };
      }
      if (event.action === "backup.server_copy_removed") {
        return {
          title: t`Removed a backup of ${volume} from its server, and kept it on ${destination}`,
          icon: "archive",
        };
      }
      if (event.action === "backup.copy_kept") {
        return { title: t`${destination} kept a backup of ${volume} that was to go`, icon: "archive", tone: "warning" };
      }
      const task = event.taskRun?.displayName || event.taskRun?.task || name;
      const server = name || event.subject?.id || t`a server`;
      switch (event.action) {
        case "task.failed":
          return {
            title: task ? t`${task} failed` : t`A scheduled task failed`,
            icon: "clock",
            tone: "critical",
          };
        case "task.recovered":
          return {
            title: task ? t`${task} succeeded after a failure` : t`A scheduled task succeeded after a failure`,
            icon: "clock",
            tone: "cobalt",
          };
        case "task.run":
          return { title: task ? t`Asked to run ${task}` : t`Asked to run a scheduled task`, icon: "clock" };
        case "server.join":
          return event.actor?.kind === ActorKind.DOCKWELL
            ? { title: t`${server} joined as the control server`, icon: "server" }
            : { title: t`${server} joined the fleet`, icon: "server" };
        case "server.renew":
          return { title: t`${server} renewed its certificate`, icon: "server" };
        case "server.revoke":
          return { title: t`Removed ${server} from the fleet`, icon: "server", tone: "warning" };
        case "server.public_address": {
          const address = event.publicAddress?.address ?? "";
          const previous = event.publicAddress?.previous ?? "";
          return {
            title: previous
              ? t`Set the public address of ${server} to ${address}, which was ${previous}`
              : t`Set the public address of ${server} to ${address}`,
            icon: "server",
          };
        }
        case "server.public_address_clear": {
          const previous = event.publicAddress?.previous ?? "";
          return { title: t`Cleared the public address of ${server}, which was ${previous}`, icon: "server" };
        }
        case "terminal.open": {
          const as = event.terminal?.title ?? "";
          const container = event.terminal?.container ?? "";
          if (container) return { title: t`Opened a shell in ${container} on ${server}`, icon: "terminal" };
          return {
            title: as ? t`Opened a terminal on ${server}, as ${as}` : t`Opened a terminal on ${server}`,
            icon: "terminal",
          };
        }
        case "terminal.close": {
          const opened = event.terminal?.openTime;
          const ended = event.terminal?.end?.time;
          const container = event.terminal?.container ?? "";
          const open =
            opened && ended ? time.span(timestampDate(ended).getTime() - timestampDate(opened).getTime()) : "";
          if (container) {
            return {
              title: open
                ? t`Closed a shell in ${container} on ${server}, open ${open}`
                : t`Closed a shell in ${container} on ${server}`,
              icon: "terminal",
            };
          }
          return {
            title: open ? t`Closed a terminal on ${server}, open ${open}` : t`Closed a terminal on ${server}`,
            icon: "terminal",
          };
        }
        case "terminal.open_failed": {
          const container = event.terminal?.container ?? "";
          return {
            title: container
              ? t`Couldn't open a shell in ${container} on ${server}`
              : t`Couldn't open a terminal on ${server}`,
            icon: "terminal",
            tone: "warning",
          };
        }
        case "terminal.open_unconfirmed": {
          // A root shell asked for, with no passkey following: what a stolen session looks like.
          const container = event.terminal?.container ?? "";
          return {
            title: container
              ? t`Asked for a passkey to open a shell in ${container} on ${server}, and none came`
              : t`Asked for a passkey to open a terminal on ${server}, and none came`,
            icon: "terminal",
            tone: "warning",
          };
        }
        case "file.read": {
          const path = event.subject?.path ?? "";
          return { title: t`Read ${path} on ${server}`, icon: "file" };
        }
        case "file.refuse": {
          const path = event.subject?.path ?? "";
          return { title: t`Couldn't open ${path} on ${server}`, icon: "file", tone: "warning" };
        }
        case "file.save": {
          const path = event.subject?.path ?? "";
          return { title: t`Saved ${path} on ${server}`, icon: "file" };
        }
        case "file.save_way_back_in": {
          // Passkey or not, this is what a stolen session would do: worth a look.
          const path = event.subject?.path ?? "";
          return { title: t`Saved ${path} on ${server}, which can let someone back in`, icon: "file", tone: "warning" };
        }
        case "file.save_unconfirmed": {
          const path = event.subject?.path ?? "";
          return {
            title: t`Asked for a passkey to save ${path} on ${server}, and none came`,
            icon: "file",
            tone: "warning",
          };
        }
        case "file.save_failed": {
          const path = event.subject?.path ?? "";
          return { title: t`Couldn't save ${path} on ${server}`, icon: "file", tone: "warning" };
        }
        case "file.save_unknown": {
          const path = event.subject?.path ?? "";
          return { title: t`May have saved ${path} on ${server}`, icon: "file", tone: "warning" };
        }
        // Who, as what, where (ADR 0049).
        case "member.invite": {
          const m = event.member;
          return {
            title: t`Invited ${m?.email ?? ""} as ${rolePhrase(m?.role ?? Role.UNSPECIFIED, { projectId: m?.projectId ?? "", projectName: m?.projectName ?? "" })}`,
            icon: "users",
          };
        }
        case "member.invite_revoke":
          return { title: t`Revoked the invite for ${event.member?.email ?? ""}`, icon: "users" };
        case "member.join": {
          const m = event.member;
          const who = m?.name || m?.email || "";
          const as = rolePhrase(m?.role ?? Role.UNSPECIFIED, {
            projectId: m?.projectId ?? "",
            projectName: m?.projectName ?? "",
          });
          // The address they joined from, which is how a join nobody expected is noticed (ADR 0049).
          const from = m?.address ?? "";
          return { title: from ? t`${who} joined as ${as}, from ${from}` : t`${who} joined as ${as}`, icon: "users" };
        }
        // Each says what it replaced, as that's the way back to it.
        case "settings.name": {
          const value = event.setting?.value ?? "";
          const previous = event.setting?.previous ?? "";
          if (!value) return { title: t`Cleared this install's name, which was ${previous}`, icon: "settings" };
          return previous
            ? { title: t`Named this install ${value}, which was ${previous}`, icon: "settings" }
            : { title: t`Named this install ${value}`, icon: "settings" };
        }
        case "settings.time_zone": {
          const value = event.setting?.value ?? "";
          const previous = event.setting?.previous ?? "";
          if (!value) {
            return {
              title: t`Cleared the time zone for new schedules, which was ${previous}, so they start in UTC`,
              icon: "settings",
            };
          }
          return previous
            ? { title: t`Set the time zone for new schedules to ${value}, which was ${previous}`, icon: "settings" }
            : { title: t`Set the time zone for new schedules to ${value}`, icon: "settings" };
        }
        case "process.end": {
          const what = event.process?.name ?? "";
          const pid = event.process?.pid ?? 0;
          return event.process?.killed
            ? {
                title: t`Killed ${what} (PID ${pid}) on ${server}, as it didn't stop within 10 s`,
                icon: "stop",
                tone: "warning",
              }
            : { title: t`Stopped ${what} (PID ${pid}) on ${server}`, icon: "stop" };
        }
        case "process.end_failed": {
          const pid = event.process?.pid ?? 0;
          return { title: t`Couldn't stop PID ${pid} on ${server}`, icon: "stop", tone: "warning" };
        }
        case "process.end_unconfirmed": {
          const pid = event.process?.pid ?? 0;
          return {
            title: t`Asked for a passkey to stop PID ${pid} on ${server}, and none came`,
            icon: "key",
            tone: "warning",
          };
        }
        // What a person makes, moves, deletes and puts back (ADR 0045). What can let
        // someone back in reads as a warning, passkey or not, as a stolen session would do it.
        case "file.make": {
          const path = event.subject?.path ?? "";
          return event.subject?.wayBackIn
            ? { title: t`Made ${path} on ${server}, which can let someone back in`, icon: "file", tone: "warning" }
            : { title: t`Made ${path} on ${server}`, icon: "file" };
        }
        case "file.move": {
          const path = event.subject?.path ?? "";
          const to = event.subject?.toPath ?? "";
          return event.subject?.wayBackIn
            ? {
                title: t`Moved ${path} to ${to} on ${server}, which can let someone back in`,
                icon: "file",
                tone: "warning",
              }
            : { title: t`Moved ${path} to ${to} on ${server}`, icon: "file" };
        }
        case "file.delete": {
          const path = event.subject?.path ?? "";
          if (event.subject?.wayBackIn) {
            return {
              title: t`Deleted ${path} on ${server}, which can let someone back in`,
              icon: "trash",
              tone: "warning",
            };
          }
          return event.subject?.forGood
            ? { title: t`Deleted ${path} on ${server} for good`, icon: "trash" }
            : { title: t`Deleted ${path} on ${server}`, icon: "trash" };
        }
        case "file.restore": {
          const path = event.subject?.path ?? "";
          return event.subject?.wayBackIn
            ? { title: t`Put back ${path} on ${server}, which can let someone back in`, icon: "undo", tone: "warning" }
            : { title: t`Put back ${path} on ${server}`, icon: "undo" };
        }
        case "file.make_failed": {
          const path = event.subject?.path ?? "";
          return { title: t`Couldn't make ${path} on ${server}`, icon: "file", tone: "warning" };
        }
        case "file.move_failed": {
          const path = event.subject?.path ?? "";
          return { title: t`Couldn't move ${path} on ${server}`, icon: "file", tone: "warning" };
        }
        case "file.delete_failed": {
          const path = event.subject?.path ?? "";
          return { title: t`Couldn't delete ${path} on ${server}`, icon: "trash", tone: "warning" };
        }
        case "file.restore_failed": {
          const path = event.subject?.path ?? "";
          return { title: t`Couldn't put back ${path} on ${server}`, icon: "undo", tone: "warning" };
        }
        case "file.make_unconfirmed": {
          const path = event.subject?.path ?? "";
          return {
            title: t`Asked for a passkey to make ${path} on ${server}, and none came`,
            icon: "file",
            tone: "warning",
          };
        }
        case "file.move_unconfirmed": {
          const path = event.subject?.path ?? "";
          return {
            title: t`Asked for a passkey to move ${path} on ${server}, and none came`,
            icon: "file",
            tone: "warning",
          };
        }
        case "file.delete_unconfirmed": {
          const path = event.subject?.path ?? "";
          return {
            title: t`Asked for a passkey to delete ${path} on ${server}, and none came`,
            icon: "trash",
            tone: "warning",
          };
        }
        case "file.restore_unconfirmed": {
          const path = event.subject?.path ?? "";
          return {
            title: t`Asked for a passkey to put back ${path} on ${server}, and none came`,
            icon: "undo",
            tone: "warning",
          };
        }
        // An upload is a make with what's in it, and says as much (ADR 0045).
        case "file.upload": {
          const path = event.subject?.path ?? "";
          const size = formatBytes(event.subject?.sizeBytes ?? 0n, i18n.locale);
          const to = event.subject?.toPath ?? "";
          if (event.subject?.wayBackIn) {
            return {
              title: t`Uploaded ${path} to ${server}, which can let someone back in`,
              icon: "upload",
              tone: "warning",
            };
          }
          return to
            ? { title: t`Uploaded ${path} to ${server}, ${size}, and unpacked it into ${to}`, icon: "upload" }
            : { title: t`Uploaded ${path} to ${server}, ${size}`, icon: "upload" };
        }
        case "file.upload_failed": {
          const path = event.subject?.path ?? "";
          return { title: t`Couldn't upload ${path} to ${server}`, icon: "upload", tone: "warning" };
        }
        case "file.upload_unconfirmed": {
          const path = event.subject?.path ?? "";
          return {
            title: t`Asked for a passkey to upload ${path} to ${server}, and none came`,
            icon: "upload",
            tone: "warning",
          };
        }
        case "file.download": {
          // A folder goes as a .zip made as it's sent, so its size isn't known first.
          const path = event.subject?.path ?? "";
          const bytes = event.subject?.sizeBytes ?? 0n;
          const size = formatBytes(bytes, i18n.locale);
          return bytes > 0n
            ? { title: t`Downloaded ${path} from ${server}, ${size}`, icon: "download" }
            : { title: t`Downloaded ${path} from ${server}`, icon: "download" };
        }
        case "file.change_unknown": {
          const path = event.subject?.path ?? "";
          return { title: t`May have changed ${path} on ${server}`, icon: "file", tone: "warning" };
        }
        case "change.apply_unconfirmed": {
          const change = event.subject?.id ?? "";
          return { title: t`Asked for a passkey to apply ${change}, and none came`, icon: "key", tone: "warning" };
        }
        case "owner.claim":
          return { title: t`Claimed this install`, icon: "key" };
        case "auth.sign_in":
          return { title: t`Signed in`, icon: "user" };
        case "session.revoke":
          return { title: t`Signed out other sessions`, icon: "logout" };
        case "passkey.add":
          return { title: name ? t`Added the passkey ${name}` : t`Added a passkey`, icon: "key" };
        case "passkey.add_with_recovery_code":
          // A recovery code stood in for the passkey: worth a look, as
          // whoever holds a code can do it.
          return {
            title: name
              ? t`Added the passkey ${name} after signing in with a recovery code`
              : t`Added a passkey after signing in with a recovery code`,
            icon: "key",
            tone: "warning",
          };
        case "passkey.add_with_authenticator_code":
          // As after a recovery code: a code stood in for the passkey.
          return {
            title: name
              ? t`Added the passkey ${name} after signing in with an authenticator code`
              : t`Added a passkey after signing in with an authenticator code`,
            icon: "key",
            tone: "warning",
          };
        case "passkey.remove":
          return { title: name ? t`Removed the passkey ${name}` : t`Removed a passkey`, icon: "key" };
        case "totp.enable":
          return { title: t`Turned on codes from an authenticator app`, icon: "lock" };
        case "totp.disable":
          return { title: t`Turned off codes from an authenticator app`, icon: "lock" };
        case "totp.setup_discarded":
          // Wrong codes for a setup someone began, which may not be yours.
          return { title: t`Ended an authenticator app's setup after 5 wrong codes`, icon: "lock", tone: "warning" };
        case "totp.unlock":
          return {
            title: name ? t`Unlocked codes for ${name}` : t`Unlocked codes from an authenticator app`,
            icon: "lock",
          };
        case "recovery_codes.regenerate":
          return { title: t`Made new recovery codes`, icon: "lock" };
        case "token.create":
          return { title: name ? t`Created the API token ${name}` : t`Created an API token`, icon: "key" };
        case "token.revoke":
          return { title: name ? t`Revoked the API token ${name}` : t`Revoked an API token`, icon: "key" };
        case "sign_in_link.create":
          return {
            title: name ? t`Made a one-time sign-in link for ${name}` : t`Made a one-time sign-in link`,
            icon: "link",
          };
        case "claim_link.create":
          return { title: t`Made a link to claim this install`, icon: "link" };
        case "join_token.create":
          return { title: t`Made a join ticket for a new server`, icon: "server" };
        case "backup_destination.test": {
          // The storage's host, which no resource names.
          const host = name || event.subject?.id || "";
          return { title: host ? t`Tested backup storage at ${host}` : t`Tested backup storage`, icon: "archive" };
        }
        case "secret.reveal":
          return { title: name ? t`Revealed the secret ${name}` : t`Revealed a secret`, icon: "eye" };
        case "secrets.rotate_key":
          return { title: t`Rotated the key that wraps every secret`, icon: "key" };
      }
      return { title: event.action, icon: event.kind === EventKind.SERVER ? "server" : "users" };
    },
    [t, i18n, time, rolePhrase],
  );
}

/** Whether a health event stands for a service that keeps changing, which the server tells as one event that goes on. */
export function keepsChanging(event: Event): boolean {
  return (event.health?.flips ?? 0) > 0 && event.health?.since !== undefined;
}

/**
 * When what an event tells of happened: for a change of health, when the service came
 * to it, which the server tells once it has lasted, up to 30 s on; for one that keeps
 * changing, the last time it was told, as it goes on. Otherwise the event's time.
 */
export function happenedAt(event: Event): Date {
  const since = event.health?.since;
  if (since && !keepsChanging(event)) return timestampDate(since);
  return event.time ? timestampDate(event.time) : new Date(0);
}

/**
 * How a service's health changed. The server tells of a state that lasted, as it
 * happened, and of a service that keeps changing as one event, which says how often
 * since when (useHealthEventWords), and what it keeps falling to.
 */
/** An alert that fired or resolved, or a mute that began or ended, as Activity's mock says them. */
/**
 * What an alert says, in the panel's language, from its fields: the condition, what
 * it's about, where, and a value. An event from before alerts carried them says
 * its title as it was sent, in English.
 */
export function alertTitle(
  a: Pick<
    NonNullable<Event["alert"]>,
    "condition" | "subjectName" | "serverName" | "variant" | "where" | "value" | "title"
  >,
  i18n: I18n,
): string {
  const subject = a.subjectName;
  const server = a.serverName;
  const where = a.where;
  const value = a.value;
  switch (a.condition) {
    case AlertRule_Condition.SERVICE_DOWN:
      switch (a.variant) {
        case "unhealthy":
          return i18n._(msg`${subject} fails its health check on ${server}`);
        case "not_running":
          return i18n._(msg`${subject} isn't running on ${server}`);
        case "flapping":
          return i18n._(msg`${subject} keeps going down on ${server}`);
        case "restarting":
          return i18n._(msg`${subject} keeps restarting on ${server}`);
      }
      break;
    case AlertRule_Condition.SERVER_UNREACHABLE:
      return i18n._(msg`${server} can't be reached`);
    case AlertRule_Condition.DISK_FULL:
      return i18n._(msg`${server}'s disk ${where} is ${value}% full`);
    case AlertRule_Condition.MEMORY_HIGH:
      return i18n._(msg`${server} uses ${value}% of its memory`);
    case AlertRule_Condition.CERTIFICATE_EXPIRING:
      return value <= 0
        ? i18n._(msg`${subject}'s certificate has ended`)
        : plural(value, {
            one: `${subject}'s certificate ends within a day`,
            other: `${subject}'s certificate ends in # days`,
          });
    case AlertRule_Condition.CERTIFICATE_FAILED:
      return i18n._(msg`${subject} has no certificate`);
    case AlertRule_Condition.BACKUP_FAILED:
      return a.variant === "copy"
        ? i18n._(msg`The backup of ${subject} didn't reach ${where}`)
        : i18n._(msg`The backup of ${subject} failed`);
    case AlertRule_Condition.BACKUP_LATE:
      return i18n._(msg`The backup of ${subject} is late`);
    case AlertRule_Condition.CHANGE_FAILED:
      return i18n._(msg`A change failed: ${subject}`);
  }
  return a.title;
}

function describeAlert(event: Event, i18n: I18n): Described {
  const a = event.alert;
  const title = a ? alertTitle(a, i18n) : "";
  const rule = a?.rule ?? "";
  const server = a?.serverName || event.subject?.name || i18n._(msg`a server`);
  const until = a?.until ? formatDateTime(timestampDate(a.until), i18n.locale) : "";
  switch (event.action) {
    case "alert.firing":
      return {
        title: i18n._(msg`Alert opened: ${title}`),
        icon: "alert",
        tone: a?.severity === AlertRule_Severity.CRITICAL ? "critical" : "warning",
      };
    case "alert.resolved":
      return { title: i18n._(msg`Resolved: ${title}`), icon: "alert" };
    case "alert.acknowledge": {
      // Taken over from someone, the event says from whom, so it stands without the one before it.
      const from = a?.takenFrom ? a.takenFrom.name || a.takenFrom.id : "";
      return from
        ? { title: i18n._(msg`Took over the alert from ${from}: ${title}`), icon: "alert" }
        : { title: i18n._(msg`Acknowledged the alert: ${title}`), icon: "alert" };
    }
    case "alert.unacknowledge":
      return { title: i18n._(msg`Took back the acknowledgement of the alert: ${title}`), icon: "alert" };
    case "alert.mute":
      return rule
        ? { title: i18n._(msg`Muted the alert rule ${rule} until ${until}`), icon: "alert" }
        : { title: i18n._(msg`Muted the alerts of ${server} until ${until}`), icon: "alert" };
    case "alert.unmute":
      return rule
        ? { title: i18n._(msg`Ended the mute of the alert rule ${rule}`), icon: "alert" }
        : { title: i18n._(msg`Ended the mute of ${server}'s alerts`), icon: "alert" };
    case "alert.test": {
      // Who tested which channel, of which type, at which host, and whether it arrived.
      const name = a?.subjectName ?? "";
      const kind = a?.variant ?? "";
      const host = a?.where ?? "";
      const arrived = a?.value === 1;
      const title = name
        ? arrived
          ? i18n._(msg`Sent a test to ${name} (${kind}, ${host}), which arrived`)
          : i18n._(msg`Sent a test to ${name} (${kind}, ${host}), which didn't arrive`)
        : arrived
          ? i18n._(msg`Sent a test to a new ${kind} channel at ${host}, which arrived`)
          : i18n._(msg`Sent a test to a new ${kind} channel at ${host}, which didn't arrive`);
      return arrived ? { title, icon: "alert" } : { title, icon: "alert", tone: "warning" };
    }
  }
  return { title: event.action, icon: "alert" };
}

function describeHealth(event: Event, i18n: I18n): Described {
  const name = event.subject?.name || event.subject?.id || i18n._(msg`A service`);
  const server = event.health?.serverName || i18n._(msg`its server`);
  if (keepsChanging(event)) {
    const tone =
      event.health?.state === HealthState.UNHEALTHY
        ? "warning"
        : event.health?.state === HealthState.PAUSED
          ? undefined
          : "critical";
    return { title: i18n._(msg`${name} keeps changing on ${server}`), icon: "activity", ...(tone ? { tone } : {}) };
  }
  switch (event.health?.state) {
    case HealthState.UNHEALTHY:
      return { title: i18n._(msg`${name} turned unhealthy on ${server}`), icon: "activity", tone: "warning" };
    case HealthState.NOT_RUNNING:
      return { title: i18n._(msg`${name} stopped running on ${server}`), icon: "activity", tone: "critical" };
    case HealthState.PAUSED:
      // Grey, as its state is: someone paused it, which is news, not trouble of its own.
      return { title: i18n._(msg`${name} was paused on ${server}`), icon: "activity" };
    case HealthState.HEALTHY:
      return { title: i18n._(msg`${name} is healthy again on ${server}`), icon: "activity" };
    case HealthState.NO_CHECK:
      return { title: i18n._(msg`${name} is running again on ${server}`), icon: "activity" };
  }
  return { title: i18n._(msg`The health of ${name} changed on ${server}`), icon: "activity" };
}

/** What a health event says beyond its title. */
export interface HealthEventWords {
  /** Its state, as a shape, a word and a colour. */
  look(event: Event): { status: Status; word: string } | undefined;
  /** What its check last said, and how often it changed, for one that keeps changing. */
  detail(event: Event): string;
  /** How often it changed, and over how long, for one that keeps changing, or nothing. */
  flips(event: Event): string;
}

export function useHealthEventWords(): HealthEventWords {
  const { t } = useLingui();
  const time = useTimeWords();
  return useMemo<HealthEventWords>(() => {
    const flips = (event: Event): string => {
      const count = event.health?.flips ?? 0;
      if (count === 0) return "";
      const since = event.health?.since;
      if (!since || !event.time) {
        // Told before the server said since when: it counted the times in between.
        return plural(count, {
          one: "It changed once more since the last of these.",
          other: "It changed # more times since the last of these.",
        });
      }
      const span = time.span(Math.max(timestampDate(event.time).getTime() - timestampDate(since).getTime(), 0));
      return plural(count, { one: `It changed once in ${span}.`, other: `It changed # times in ${span}.` });
    };
    return {
      flips,
      look(event) {
        switch (event.health?.state) {
          case HealthState.UNHEALTHY:
            return { status: "degraded", word: t`Unhealthy` };
          case HealthState.NOT_RUNNING:
            return { status: "down", word: t`Not running` };
          case HealthState.PAUSED:
            return { status: "stopped", word: t`Paused` };
          case HealthState.HEALTHY:
            return { status: "healthy", word: t`Healthy` };
          case HealthState.NO_CHECK:
            return { status: "running", word: t`Running` };
        }
        return undefined;
      },
      detail(event) {
        const said = event.health?.output ?? "";
        return [said.charAt(0).toUpperCase() + said.slice(1), flips(event)].filter(Boolean).join(" · ");
      },
    };
  }, [t, time]);
}

function describeChange(change: Change, summaries: ReadonlyMap<string, string>, i18n: I18n): Described {
  const reverted = change.reverts ? summaries.get(change.reverts) : undefined;
  let title = change.summary;
  if (reverted !== undefined) title = i18n._(change.rollback ? msg`Roll back: ${reverted}` : msg`Undo: ${reverted}`);
  const icon = changeIcon(change);
  switch (change.state) {
    case ChangeState.FAILED:
      return { title, icon, tone: "critical" };
    case ChangeState.APPLYING:
      return { title, icon, tone: "cobalt" };
  }
  return change.rollback ? { title, icon, tone: "warning" } : { title, icon };
}

/**
 * What a change does, at a glance, as Activity's mock marks it: an undo or rollback,
 * a restart, a deploy of an image, taking things away, or else a plan.
 */
export function changeIcon(change: Change): IconName {
  if (change.reverts) return "undo";
  if (change.restart) return "refresh";
  const intent = change.plan?.intent?.kind;
  if (intent === IntentKind.DEPLOY || intent === IntentKind.REPULL) return "deploy";
  if (intent === IntentKind.ROLL_BACK) return "undo";
  const s = change.plan?.summary;
  if (s && s.destroy > 0 && !s.create && !s.change && !s.replace) return "trash";
  return "commit";
}

/** A change's plan in brief: "2 to create · 1 to delete". Empty when it changes nothing. */
export function useCounts(): (change: Change) => string {
  const { t } = useLingui();
  return useMemo(
    () => (change: Change) => {
      const s = change.plan?.summary;
      if (!s) return "";
      const parts: string[] = [];
      if (s.create) parts.push(t`${s.create} to create`);
      if (s.change) parts.push(t`${s.change} to update`);
      // A restart replaces what runs as it was, which is no replace to whoever asked for it.
      if (s.replace) parts.push(change.restart ? t`${s.replace} to restart` : t`${s.replace} to replace`);
      if (s.destroy) parts.push(t`${s.destroy} to delete`);
      return parts.join(" · ");
    },
    [t],
  );
}

/**
 * How far an applying change has got: "1 of 3 steps done", counting the
 * steps that ran to the end, not one that failed or was skipped. Empty
 * with no steps.
 */
export function useProgress(): (change: Change) => string {
  // The words change with the locale, which t carries.
  const { t } = useLingui();
  // biome-ignore lint/correctness/useExhaustiveDependencies: t marks a change of locale
  return useMemo(
    () => (change: Change) => {
      const total = change.steps.length;
      if (total === 0) return "";
      const done = change.steps.filter((s) => s.state === StepState.DONE).length;
      return plural(total, { one: `${done} of # step done`, other: `${done} of # steps done` });
    },
    [t],
  );
}
