// SPDX-License-Identifier: AGPL-3.0-only

import { type CheckConnectionResponse, type ConnectionInfo, LinkKind } from "@dockwell/api";
import { StatusBadge, type StatusBadgeProps } from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { useLingui } from "@lingui/react/macro";

import { useTickingNow, useTimeWords } from "../../lib/time";
import { neutral, SILENT, useCheckWords } from "./useConnections";

/** A connection's last checks as one state: the list's Last check, and its page's head. */
export interface LastCheck {
  status: StatusBadgeProps["status"];
  /** "Passed", "Fails from web-1", or below an admin "Failed". */
  label: string;
  /** Where and when: "web-1 · 3 h ago", "2 servers · 3 h ago", or "3 h ago". */
  meta: string;
  /** What a failing check found, in a few words, as "Keys refused", for an admin. */
  why: string;
  /** The failing check, with what it found and its fix, for an admin. */
  failing: CheckConnectionResponse | undefined;
}

/**
 * What the control plane's checks of a connection found, from each server it's
 * checked from, once it's saved or changed and every 6 hours (#799): a failing
 * one first, as that's what a person acts on. Undefined before its first check.
 */
export function useLastCheck(connection: ConnectionInfo): LastCheck | undefined {
  const { t } = useLingui();
  const words = useCheckWords();
  const time = useTimeWords();
  const checks = connection.checks;
  const newest = checks.reduce((n, c) => Math.max(n, c.time ? Number(c.time.seconds) * 1000 : 0), 0);
  const now = useTickingNow(newest || undefined);
  if (checks.length === 0) return undefined;
  const at = (c: CheckConnectionResponse) => (c.time ? time.ago(new Date(Number(c.time.seconds) * 1000), now) : "");
  // A real failure first, then one that couldn't confirm the keys or couldn't run.
  const failing = checks.find((c) => !c.passed && !neutral(c)) ?? checks.find((c) => !c.passed);
  if (failing) {
    const said = words(failing, mailCheckOf(connection));
    // Below an admin, a check is whether it passed and when, alone.
    const admin = failing.server !== "";
    const server = failing.server;
    if (neutral(failing)) {
      // Not green and not red: what it couldn't say, with Check as the way on (#799's
      // review, M3, L5), and the same to someone below an admin, without where (N1).
      return {
        status: "unknown",
        label: said.state,
        meta: [server, at(failing)].filter(Boolean).join(" · "),
        why: "",
        failing: admin ? failing : undefined,
      };
    }
    return {
      status: SILENT.includes(failing.code) ? "degraded" : "down",
      label: admin ? t`Fails from ${server}` : t`Failed`,
      meta: at(failing),
      why: admin ? said.state : "",
      failing: admin ? failing : undefined,
    };
  }
  const servers = checks.filter((c) => c.server !== "").map((c) => c.server);
  const where =
    servers.length === 1
      ? servers[0]
      : servers.length > 1
        ? plural(servers.length, { one: "# server", other: "# servers" })
        : "";
  return {
    status: "healthy",
    label: t({ message: "Passed", context: "a connection's last check" }),
    meta: [where, at(checks[0] as CheckConnectionResponse)].filter(Boolean).join(" · "),
    why: "",
    failing: undefined,
  };
}

/** What a mail server's check knows beyond its answer: its port. */
export function mailCheckOf(c: ConnectionInfo) {
  return c.kind === LinkKind.SMTP ? { port: c.port } : undefined;
}

/** A connection's last check, as the list's cell: a state, and what failed under it. */
export function LastCheckCell({ connection }: { connection: ConnectionInfo }) {
  const { t } = useLingui();
  const last = useLastCheck(connection);
  if (!last) {
    return (
      <span className="connections-meta">
        {connection.kind === LinkKind.CUSTOM && !connection.custom?.hasCheck ? t`No check` : t`Not checked yet`}
      </span>
    );
  }
  return (
    <div className="dw-cell">
      <StatusBadge status={last.status} label={last.label} meta={last.meta} labelIsState />
      {last.why ? <span className="connections-meta connection-check-why">{last.why}</span> : null}
    </div>
  );
}
