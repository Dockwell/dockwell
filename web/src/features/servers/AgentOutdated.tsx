// SPDX-License-Identifier: AGPL-3.0-only

import { Callout } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import type { ReactNode } from "react";

import { reasonOf } from "../../lib/errors";
import { RouterButtonLink } from "../../lib/links";

/** Whether a read failed as the server's agent is older than the control plane. */
export function isAgentOutdated(error: unknown): boolean {
  return reasonOf(error) === "agent_outdated";
}

/**
 * A server whose agent is older than the control plane, so this page can't read
 * what it shows: said once, with the way to update it on the server's Updates tab,
 * which has the command.
 */
export function AgentOutdated({ server, children }: { server: { id: string; name: string }; children: ReactNode }) {
  const { t } = useLingui();
  return (
    <Callout
      tone="warning"
      title={t`${server.name}'s agent is outdated`}
      actions={
        <RouterButtonLink size="sm" to="/servers/$serverId/updates" params={{ serverId: server.id }}>
          <Trans>Update its agent</Trans>
        </RouterButtonLink>
      }
    >
      {children}
    </Callout>
  );
}
