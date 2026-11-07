// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate } from "@bufbuild/protobuf/wkt";
import { ActorKind, type Change, ChangeState, type Event, EventKind, HealthState, StepState } from "@dockwell/api";
import type { IconName, Status } from "@dockwell/ui";
import type { I18n } from "@lingui/core";
import { msg, plural } from "@lingui/core/macro";
import { useLingui } from "@lingui/react/macro";
import { useMemo } from "react";

import { useTimeWords } from "../../lib/time";

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
  return useMemo(
    () => (event: Event, summaries: ReadonlyMap<string, string>) => {
      const change = event.change;
      if (event.kind === EventKind.CHANGE && change) return describeChange(change, summaries, i18n);
      const name = event.subject?.name ?? "";
      if (event.kind === EventKind.HEALTH) return describeHealth(event, i18n);
      const volume = event.backup?.volume ?? name;
      if (event.action === "backup.taken") return { title: t`Backed up ${volume}`, icon: "archive" };
      if (event.action === "backup.failed") {
        return { title: t`The backup of ${volume} failed`, icon: "archive", tone: "critical" };
      }
      if (event.action === "backup.deleted") return { title: t`Deleted a backup of ${volume}`, icon: "archive" };
      const server = name || event.subject?.id || t`a server`;
      switch (event.action) {
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
        case "secret.reveal":
          return { title: name ? t`Revealed the secret ${name}` : t`Revealed a secret`, icon: "eye" };
        case "secrets.rotate_key":
          return { title: t`Rotated the key that wraps every secret`, icon: "key" };
      }
      return { title: event.action, icon: event.kind === EventKind.SERVER ? "server" : "users" };
    },
    [t, i18n],
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
  const icon: IconName = change.reverts ? "undo" : "commit";
  switch (change.state) {
    case ChangeState.FAILED:
      return { title, icon, tone: "critical" };
    case ChangeState.APPLYING:
      return { title, icon, tone: "cobalt" };
  }
  return change.rollback ? { title, icon, tone: "warning" } : { title, icon };
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
