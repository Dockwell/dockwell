// SPDX-License-Identifier: AGPL-3.0-only

import type { AuditEvent } from "@dockwell/api";
import { useLingui } from "@lingui/react/macro";
import { useMemo } from "react";

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
  const { t } = useLingui();
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
      }
      if (event) {
        const said = describe(event, NO_SUMMARIES);
        // Activity says an action it has no words for as it is.
        if (said.title !== event.action)
          return said.tone ? { title: said.title, tone: said.tone } : { title: said.title };
      }
      return { title: e.action };
    },
    [t, describe],
  );
}
