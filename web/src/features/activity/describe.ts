// SPDX-License-Identifier: AGPL-3.0-only

import { ActorKind, type Change, ChangeState, type Event, EventKind, StepState } from "@dockwell/api";
import type { IconName } from "@dockwell/ui";
import type { I18n } from "@lingui/core";
import { msg, plural } from "@lingui/core/macro";
import { useLingui } from "@lingui/react/macro";
import { useMemo } from "react";

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

/** A change's plan in brief: "2 to create · 1 to destroy". Empty when it changes nothing. */
export function useCounts(): (change: Change) => string {
  const { t } = useLingui();
  return useMemo(
    () => (change: Change) => {
      const s = change.plan?.summary;
      if (!s) return "";
      const parts: string[] = [];
      if (s.create) parts.push(t`${s.create} to create`);
      if (s.change) parts.push(t`${s.change} to change`);
      if (s.replace) parts.push(t`${s.replace} to replace`);
      if (s.destroy) parts.push(t`${s.destroy} to destroy`);
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
