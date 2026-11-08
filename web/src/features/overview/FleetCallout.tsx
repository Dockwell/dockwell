// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate } from "@bufbuild/protobuf/wkt";
import { Connection, type Server } from "@dockwell/api";
import { Callout, type Tone } from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import type { ReactNode } from "react";

import { RouterButtonLink } from "../../lib/links";
import { useTimeWords } from "../../lib/time";
import { useControlPlaneNow } from "../servers/useFleet";

/** What a callout on the fleet's overview says, and which server it opens, if one. */
interface Notice {
  tone: Tone;
  title: string;
  body: ReactNode;
  /** The server to inspect; every server when it's about several. */
  serverId?: string;
}

/**
 * The one thing about the fleet to look at first, as Overview-Fleet leads with it:
 * servers that stopped reporting, then servers whose agent can't read Docker, then
 * servers whose agent hasn't connected yet. Alerts that fire, from ListAlerts once
 * #304 lands, belong first in this order.
 */
export function FleetCallout({ servers }: { servers: readonly Server[] }) {
  const { t } = useLingui();
  const notice = useNotice(servers);
  if (!notice) return null;
  return (
    <Callout
      tone={notice.tone}
      title={notice.title}
      actions={
        notice.serverId ? (
          <RouterButtonLink to="/servers/$serverId" params={{ serverId: notice.serverId }} size="sm" variant="ghost">
            {t`Inspect`}
          </RouterButtonLink>
        ) : (
          <RouterButtonLink to="/servers" size="sm" variant="ghost">
            {t`Inspect`}
          </RouterButtonLink>
        )
      }
    >
      {notice.body}
    </Callout>
  );
}

function useNotice(servers: readonly Server[]): Notice | undefined {
  const { t, i18n } = useLingui();
  const words = useTimeWords();
  // Every second, as the age counts in seconds for its first minute.
  const now = useControlPlaneNow();
  const names = (list: readonly Server[]) =>
    new Intl.ListFormat(i18n.locale, { type: "conjunction" }).format(list.map((s) => s.name));

  const down = servers.filter((s) => s.connection === Connection.UNREACHABLE);
  const [first] = down;
  if (down.length === 1 && first) {
    const name = first.name;
    const seen = first.lastSeenTime ? words.ago(timestampDate(first.lastSeenTime), now) : undefined;
    return {
      tone: "critical",
      title: t`${name} isn't reporting`,
      serverId: first.id,
      body: seen ? (
        <Trans>
          Its agent last reported {seen}, so its numbers are left out of the fleet's. Check that{" "}
          <code>dockwell run</code> is still running on the server.
        </Trans>
      ) : (
        <Trans>
          Its agent has disconnected, so its numbers are left out of the fleet's. Check that <code>dockwell run</code>{" "}
          is still running on the server.
        </Trans>
      ),
    };
  }
  if (down.length > 1) {
    const list = names(down);
    return {
      tone: "critical",
      title: plural(down.length, { one: "# server isn't reporting", other: "# servers aren't reporting" }),
      body: (
        <Trans>
          {list} stopped reporting, so their numbers are left out of the fleet's. Check that <code>dockwell run</code>{" "}
          is still running on each.
        </Trans>
      ),
    };
  }

  const blind = servers.filter((s) => s.connection === Connection.CONNECTED && s.dockerUnreadableSince);
  const [one] = blind;
  if (blind.length === 1 && one) {
    const name = one.name;
    return {
      tone: "warning",
      title: t`${name} can't read Docker`,
      serverId: one.id,
      body: (
        <Trans>
          Its agent is connected, but Docker doesn't answer, so nobody can say how what runs there is. Check that Docker
          is running on the server.
        </Trans>
      ),
    };
  }
  if (blind.length > 1) {
    const list = names(blind);
    return {
      tone: "warning",
      title: plural(blind.length, { one: "# server can't read Docker", other: "# servers can't read Docker" }),
      body: (
        <Trans>
          On {list}, the agent is connected but Docker doesn't answer, so nobody can say how what runs there is. Check
          that Docker is running on each.
        </Trans>
      ),
    };
  }

  const waiting = servers.filter((s) => s.connection === Connection.PENDING);
  const [next] = waiting;
  if (waiting.length === 1 && next) {
    const name = next.name;
    return {
      tone: "info",
      title: t`${name} is waiting for its agent`,
      serverId: next.id,
      body: (
        <Trans>
          It joined, but its agent hasn't connected yet. Start it with <code>dockwell run</code> on the server.
        </Trans>
      ),
    };
  }
  if (waiting.length > 1) {
    const list = names(waiting);
    return {
      tone: "info",
      title: plural(waiting.length, {
        one: "# server is waiting for its agent",
        other: "# servers are waiting for their agents",
      }),
      body: (
        <Trans>
          {list} joined, but their agents haven't connected yet. Start each with <code>dockwell run</code> on the
          server.
        </Trans>
      ),
    };
  }
  return undefined;
}
