// SPDX-License-Identifier: AGPL-3.0-only

import { PortAnswer } from "@dockwell/api";
import { useLingui } from "@lingui/react/macro";
import { Link } from "@tanstack/react-router";
import type { ReactNode } from "react";

import { reasonOf, sentence } from "../../lib/errors";
import type { AppPort, useServicePorts } from "./appPorts";
import { StateLine } from "./DomainStep";

/**
 * Whether the port picked answers, as the app's server found it at the container's
 * own address (#791): an HTTP answer, something else, or nothing, with Check again.
 * Where the server didn't check, it says why, with what to do where there's
 * something (the review's L5). A port the server didn't list says nothing listens
 * there only where the list is whole and the server read what listens.
 */
export function PortState({
  port,
  ports,
  read,
}: {
  port: number;
  ports: readonly AppPort[];
  read: ReturnType<typeof useServicePorts>;
}) {
  const { t } = useLingui();
  const again = (
    <button type="button" className="domain-step-act" disabled={read.checking} onClick={read.again}>
      {read.checking ? t`Checking` : t`Check again`}
    </button>
  );
  const c = port > 0 ? ports.find((p) => p.port === port)?.check : undefined;
  switch (c?.answer) {
    case PortAnswer.HTTP:
      return (
        <StateLine status="healthy" action={again}>
          {t`Port ${port} answers: ${c.said}`}
        </StateLine>
      );
    case PortAnswer.OTHER:
      return (
        <StateLine status="degraded" action={again}>
          {t`Port ${port} answers, but not over HTTP`}
        </StateLine>
      );
    case PortAnswer.NOTHING:
      return (
        <StateLine status="down" action={again}>
          {t`Nothing answers on port ${port} yet`}
        </StateLine>
      );
  }
  const server = read.server?.serverName ?? "";
  if (read.error !== undefined) {
    const said = (words: string, action?: ReactNode) => (
      <StateLine status="unknown" action={action}>
        {words}
      </StateLine>
    );
    switch (reasonOf(read.error)) {
      case "agent_outdated":
        return said(
          t`${server}'s agent is too old to check ports`,
          read.server ? (
            <Link
              className="domain-step-act"
              to="/servers/$serverId/updates"
              params={{ serverId: read.server.serverId }}
            >
              {t`Update its agent`}
            </Link>
          ) : undefined,
        );
      case "unavailable":
        return said(t`${server} isn't answering, so ports aren't checked`, again);
      case "too_many_requests":
        return said(t`Ports were checked often just now. Check again in a minute`);
    }
    return said(sentence(read.error), again);
  }
  if (read.unread !== "") {
    return (
      <StateLine status="unknown" action={again}>
        {t`Ports aren't checked: ${read.unread}`}
      </StateLine>
    );
  }
  if (c !== undefined) {
    return <StateLine status="unknown">{t`Port ${port} isn't checked, as only the first 24 ports are`}</StateLine>;
  }
  if (port > 0 && read.readListening) {
    return (
      <StateLine status="degraded" action={again}>
        {t`Nothing in the app listens on port ${port}`}
      </StateLine>
    );
  }
  return null;
}
