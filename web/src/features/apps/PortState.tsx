// SPDX-License-Identifier: AGPL-3.0-only

import { PortAnswer } from "@dockwell/api";
import { useLingui } from "@lingui/react/macro";

import type { AppPort, useServicePorts } from "./appPorts";
import { StateLine } from "./DomainStep";

/**
 * Whether the port picked answers, as the app's server found it at the container's
 * own address (#791): an HTTP answer, something else, or nothing, with Check again.
 * A port the server wasn't asked about says nothing listens there only where the
 * server read what listens; otherwise, and before any answer, it says nothing.
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
  if (!(port > 0)) return null;
  const c = ports.find((p) => p.port === port)?.check;
  const again = (
    <button type="button" className="domain-step-act" disabled={read.checking} onClick={read.again}>
      {read.checking ? t`Checking` : t`Check again`}
    </button>
  );
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
  if (!c && read.readListening) {
    return (
      <StateLine status="degraded" action={again}>
        {t`Nothing in the app listens on port ${port}`}
      </StateLine>
    );
  }
  return null;
}
