// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate } from "@bufbuild/protobuf/wkt";
import { Connection, type Server } from "@dockwell/api";
import { Callout, PHONE_QUERY, type Tone, useMediaQuery } from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { useLingui } from "@lingui/react/macro";
import type { ReactNode } from "react";

import { RouterButtonLink } from "../../lib/links";
import { useTimeWords } from "../../lib/time";
import { serviceCommands } from "../servers/commands";
import { useControlPlaneNow } from "../servers/useFleet";
import { CopyButton } from "../system/CopyCommand";

/** What a callout on the fleet's overview says, and which server it opens, if one. */
interface Notice {
  tone: Tone;
  title: string;
  body: ReactNode;
  /** The server to inspect; every server when it's about several. */
  serverId?: string;
  /** A command to copy and run on the server, where Dockwell can't act itself. */
  command?: { text: string; label: string; copied: string };
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
  // On a phone, as M-Overview-Fleet has it, a full button under the words.
  const phone = useMediaQuery(PHONE_QUERY);
  if (!notice) return null;
  const button = phone
    ? ({ size: "md", variant: "secondary", className: "overview-callout-action" } as const)
    : ({ size: "sm", variant: "ghost" } as const);
  return (
    <Callout
      tone={notice.tone}
      title={notice.title}
      actions={
        <>
          {notice.command ? (
            <CopyButton
              text={notice.command.text}
              label={notice.command.label}
              copiedLabel={notice.command.copied}
              {...button}
            />
          ) : null}
          {notice.serverId ? (
            <RouterButtonLink to="/servers/$serverId" params={{ serverId: notice.serverId }} {...button}>
              {t`Inspect`}
            </RouterButtonLink>
          ) : (
            <RouterButtonLink to="/servers" {...button}>
              {t`Inspect`}
            </RouterButtonLink>
          )}
        </>
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
  // What to run on a server where Dockwell can't act, as its service manager takes it. Closures, as t works only here.
  const restart = (s: Server) => ({
    text: serviceCommands(s).restartAgent,
    label: t`Copy the restart command`,
    copied: t`Restart command copied`,
  });
  const start = (s: Server) => ({
    text: serviceCommands(s).startAgent,
    label: t`Copy the start command`,
    copied: t`Start command copied`,
  });
  const docker = (s: Server) => ({
    text: serviceCommands(s).restartDocker,
    label: t`Copy the command that restarts Docker`,
    copied: t`Command copied`,
  });

  const down = servers.filter((s) => s.connection === Connection.UNREACHABLE);
  const [first] = down;
  if (down.length === 1 && first) {
    const name = first.name;
    const seen = first.lastSeenTime ? words.ago(timestampDate(first.lastSeenTime), now) : undefined;
    return {
      tone: "critical",
      title: t`${name} isn't reporting`,
      serverId: first.id,
      body: seen ? t`Last heard ${seen}; its numbers are left out of the fleet's.` : t`Its agent disconnected.`,
      command: restart(first),
    };
  }
  if (down.length > 1) {
    const list = names(down);
    return {
      tone: "critical",
      title: plural(down.length, { one: "# server isn't reporting", other: "# servers aren't reporting" }),
      body: t`${list} stopped reporting; their numbers are left out of the fleet's.`,
      command: first ? restart(first) : undefined,
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
      body: t`Its agent is connected; Docker doesn't answer.`,
      command: docker(one),
    };
  }
  if (blind.length > 1) {
    const list = names(blind);
    return {
      tone: "warning",
      title: plural(blind.length, { one: "# server can't read Docker", other: "# servers can't read Docker" }),
      body: t`On ${list}, the agent is connected and Docker doesn't answer.`,
      command: one ? docker(one) : undefined,
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
      body: t`It joined, and its agent hasn't connected yet.`,
      command: start(next),
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
      body: t`${list} joined, and their agents haven't connected yet.`,
      command: next ? start(next) : undefined,
    };
  }
  return undefined;
}
