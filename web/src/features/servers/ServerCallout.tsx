// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate } from "@bufbuild/protobuf/wkt";
import { Connection, type Server } from "@dockwell/api";
import { Callout } from "@dockwell/ui";
import { useLingui } from "@lingui/react/macro";

import { useTimeWords } from "../../lib/time";
import { CopyButton } from "../system/CopyCommand";
import { serviceCommands } from "./commands";
import { useControlPlaneNow } from "./useFleet";

/**
 * Says when a server's agent isn't reporting, in a line, with the command that starts
 * it again on the server to copy: Dockwell can't reach the server to do it itself.
 */
export function ServerCallout({ server }: { server: Server }) {
  const { t } = useLingui();
  const words = useTimeWords();
  // Every second, as the age counts in seconds for its first minute.
  const now = useControlPlaneNow();
  const name = server.name;
  const commands = serviceCommands(server);
  if (server.connection === Connection.UNREACHABLE) {
    const seen = server.lastSeenTime ? words.ago(timestampDate(server.lastSeenTime), now) : undefined;
    return (
      <Callout
        tone="critical"
        title={t`${name} isn't reporting`}
        actions={
          <CopyButton
            text={commands.restartAgent}
            label={t`Copy the restart command`}
            copiedLabel={t`Restart command copied`}
            variant="secondary"
          />
        }
      >
        {seen ? t`Last heard ${seen}; the numbers here are from then.` : t`Its agent disconnected.`}
      </Callout>
    );
  }
  if (server.connection === Connection.PENDING) {
    return (
      <Callout
        tone="info"
        title={t`${name} is waiting for its agent`}
        actions={
          <CopyButton
            text={commands.startAgent}
            label={t`Copy the start command`}
            copiedLabel={t`Start command copied`}
            variant="secondary"
          />
        }
      >
        {t`It joined, and its agent hasn't connected yet.`}
      </Callout>
    );
  }
  return null;
}
