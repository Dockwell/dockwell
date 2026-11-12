// SPDX-License-Identifier: AGPL-3.0-only

import type { AuditEvent } from "@dockwell/api";
import { useLingui } from "@lingui/react/macro";
import { useMemo } from "react";

import { formatDateTime } from "../../lib/format";
import { type Described, useDescribe } from "../activity/describe";

const NO_SUMMARIES: ReadonlyMap<string, string> = new Map();

/** What an audit event did, in a sentence, with a tone for an outcome worth noticing. */
export interface Said {
  title: string;
  tone?: Described["tone"];
}

/**
 * Says what an audit event did, in the active locale: as Activity says it, from
 * the facts the event carries, and here for what only the audit log keeps, such
 * as refusals and a change at each step. What it recorded stays as it was, for
 * the JSON export and the opened row.
 */
export function useAuditWords(): (event: AuditEvent) => Said {
  const { t, i18n } = useLingui();
  const describe = useDescribe();
  return useMemo(
    () => (e: AuditEvent) => {
      const event = e.event;
      const name = e.target?.name || event?.subject?.name || "";
      // A change's events record its summary, as its author wrote it.
      const summary = event?.change?.summary || e.detail;
      switch (e.action) {
        case "change.planned":
          return { title: summary ? t`Planned ${summary}` : t`Planned a change` };
        case "change.applied":
          return { title: summary ? t`Applied ${summary}` : t`Applied a change` };
        case "change.failed":
          return { title: summary ? t`Couldn't apply ${summary}` : t`A change failed`, tone: "critical" };
        case "change.discarded":
          return { title: summary ? t`Discarded ${summary}` : t`Discarded a change` };
        case "auth.sign_in_failed":
          return { title: t`Couldn't sign in`, tone: "warning" };
        case "auth.sign_out":
          return { title: t`Signed out` };
        case "auth.step_up":
          return { title: t`Confirmed with a passkey` };
        case "auth.step_up_failed":
          return { title: t`Couldn't confirm with a passkey`, tone: "warning" };
        // The session stood in for a passkey, as its person chose (ADR 0075).
        case "auth.step_up_hour":
          return { title: t`Confirmed within an hour of a passkey` };
        case "auth.step_up_sign_in":
          return { title: t`Confirmed by signing in` };
        case "auth.step_up_policy":
          return { title: t`Changed how often their passkey is asked for` };
        case "token.refuse":
          return { title: t`Refused a call this token can't make`, tone: "warning" };
        case "role.refuse":
          return { title: t`Refused a call their role doesn't cover`, tone: "warning" };
        case "owner.transfer":
          return { title: name ? t`Made ${name} the owner` : t`Moved ownership of this install` };
        case "token.use":
          return { title: t`Used from a new address` };
        case "build.cancel":
          return { title: name ? t`Asked to cancel a build on ${name}` : t`Asked to cancel a build` };
        case "install.create":
          return { title: t`Set up this install` };
        case "passkey.clone_suspected":
          return {
            title: name ? t`The passkey ${name} may have been copied` : t`A passkey may have been copied`,
            tone: "critical",
          };
        case "server.renew_rejected":
          return {
            title: name ? t`Refused to renew the certificate of ${name}` : t`Refused to renew a server's certificate`,
            tone: "warning",
          };
        case "totp.lock":
          return { title: t`Stopped sign-in with codes after too many wrong ones`, tone: "warning" };
        case "logs.read":
          // A container Dockwell didn't make is read through its server, which the target names.
          if (e.target?.id.startsWith("srv_")) {
            return { title: name ? t`Read a container's logs on ${name}` : t`Read a container's logs` };
          }
          return { title: name ? t`Read the logs of ${name}` : t`Read logs` };
        case "terminal.opening":
          return { title: name ? t`Started opening a terminal on ${name}` : t`Started opening a terminal` };
        case "recovery_code.use":
          return { title: t`Signed in with a recovery code`, tone: "warning" };
        case "file.saving": {
          const path = event?.subject?.path || e.subject;
          return { title: name ? t`Saving ${path} on ${name}` : t`Saving ${path}` };
        }
        // What a person set aside of an app's updates, as its event recorded it (ADR 0079).
        case "service.update_skip": {
          const { app, version } = updateFacts(e.detail, name || e.subject);
          return { title: version ? t`Skipped ${version} of ${app}` : t`Skipped an update of ${app}` };
        }
        case "service.update_unskip": {
          const { app, version } = updateFacts(e.detail, name || e.subject);
          return { title: version ? t`Showed ${version} of ${app} again` : t`Showed an update of ${app} again` };
        }
        case "service.update_snooze": {
          const { app, until } = updateFacts(e.detail, name || e.subject);
          const at = until ? formatDateTime(until, i18n.locale) : "";
          return { title: at ? t`Snoozed the updates of ${app} until ${at}` : t`Snoozed the updates of ${app}` };
        }
        case "service.update_unsnooze": {
          const { app } = updateFacts(e.detail, name || e.subject);
          return { title: t`Ended the snooze of the updates of ${app}` };
        }
      }
      if (event) {
        const said = describe(event, NO_SUMMARIES);
        // Activity says an action it has no words for as it is.
        if (said.title !== event.action)
          return said.tone ? { title: said.title, tone: said.tone } : { title: said.title };
      }
      return { title: e.action };
    },
    [t, i18n, describe],
  );
}

/**
 * What a skip or a snooze of an app's updates recorded: the app, the version and
 * when a snooze ends, from its JSON, as internal/control/serviceupdates writes it.
 * The app's name falls back to what the event names, then to its ID.
 */
function updateFacts(detail: string, named: string): { app: string; version: string; until: Date | undefined } {
  let facts: { service?: unknown; version?: unknown; until?: unknown } = {};
  try {
    const read: unknown = JSON.parse(detail || "{}");
    if (read && typeof read === "object") facts = read as typeof facts;
  } catch {
    // Not JSON: the sentence goes on with what the event names.
  }
  const text = (v: unknown) => (typeof v === "string" ? v : "");
  const until = text(facts.until) ? new Date(text(facts.until)) : undefined;
  return {
    app: text(facts.service) || named,
    version: text(facts.version),
    until: until && !Number.isNaN(until.getTime()) ? until : undefined,
  };
}
