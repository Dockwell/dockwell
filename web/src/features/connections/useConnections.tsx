// SPDX-License-Identifier: AGPL-3.0-only

import { useQuery } from "@connectrpc/connect-query";
import { type CheckConnectionResponse, ConnectionService } from "@dockwell/api";
import { StatusBadge } from "@dockwell/ui";
import { useLingui } from "@lingui/react/macro";

import { useAllResources } from "../projects/useProjects";

/** Every connection the caller may see, or those an environment's project may use. */
export function useConnections(environment = "") {
  return useQuery(ConnectionService.method.listConnections, { environment });
}

/** The probe's own codes for what kept it from an answer, as the agent says them. */
const NO_ANSWER = ["refused", "timeout", "unreachable", "tls", "mismatch"] as const;

/**
 * What a check found, as a state and a line: Reached from a server, or the fact and
 * its fix in the words a person acts on, never the bucket's own body.
 */
export function useCheckWords() {
  const { t } = useLingui();
  return (c: CheckConnectionResponse): { passed: boolean; state: string; line: string } => {
    if (c.passed) {
      return {
        passed: true,
        state: c.server ? t`Reached from ${c.server}` : t`Reached`,
        line: c.region ? t`It answered from region ${c.region}.` : t`Wrote, read and deleted a test object.`,
      };
    }
    const code = c.code;
    const said = c.words || c.message;
    switch (code as (typeof NO_ANSWER)[number] | string) {
      case "refused":
        return { passed: false, state: t`Address refused`, line: said || t`Dockwell keeps apps from that address.` };
      case "timeout":
        return { passed: false, state: t`No answer`, line: t`Check the endpoint, and that the server can reach it.` };
      case "unreachable":
        return { passed: false, state: t`Can't reach it`, line: said || t`Check the endpoint.` };
      case "tls":
        return { passed: false, state: t`Untrusted certificate`, line: t`Check the endpoint's address.` };
      case "mismatch":
        return {
          passed: false,
          state: t`Answered wrongly`,
          line: t`The bucket gave back something other than what was written.`,
        };
      case "SignatureDoesNotMatch":
      case "InvalidAccessKeyId":
        return { passed: false, state: t`Keys refused`, line: t`Check the access key ID and its secret.` };
      case "AccessDenied":
        return { passed: false, state: t`Access denied`, line: t`The keys may not write in this bucket.` };
      case "NoSuchBucket":
        return { passed: false, state: t`No such bucket`, line: t`Check the bucket's name.` };
    }
    return {
      passed: false,
      state: t`Refused`,
      line: said ? `${code || c.status}: ${said}` : t`The bucket refused the check with ${code || String(c.status)}.`,
    };
  };
}

/** A check's state, as a badge: healthy where it passed, down where the bucket refused, unknown where nothing answered. */
export function CheckBadge({ check }: { check: CheckConnectionResponse }) {
  const words = useCheckWords()(check);
  const silent = ["timeout", "unreachable", "refused"].includes(check.code);
  return (
    <StatusBadge status={words.passed ? "healthy" : silent ? "unknown" : "down"} label={words.state} labelIsState />
  );
}

/** What the panel calls a project, by its ID: its display name, or its name. */
export function useProjectLabel(): (id: string) => string {
  const resources = useAllResources();
  return (id: string) => {
    const p = (resources.data ?? []).find((r) => r.id === id && r.resource?.kind === "project");
    return p?.displayName || p?.resource?.name || id;
  };
}
