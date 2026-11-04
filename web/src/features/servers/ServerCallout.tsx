// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate } from "@bufbuild/protobuf/wkt";
import { Connection, type Server } from "@dockwell/api";
import { Callout } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";

import { useTimeWords } from "../../lib/time";
import { useControlPlaneNow } from "./useFleet";

/** Says when a server's agent isn't reporting, what that means for the page, and what to do. */
export function ServerCallout({ server }: { server: Server }) {
  const { t } = useLingui();
  const words = useTimeWords();
  // Every second, as the age counts in seconds for its first minute.
  const now = useControlPlaneNow();
  const name = server.name;
  if (server.connection === Connection.UNREACHABLE) {
    const seen = server.lastSeenTime ? words.ago(timestampDate(server.lastSeenTime), now) : undefined;
    return (
      <Callout tone="critical" title={t`${name} isn't reporting`}>
        {seen ? (
          <Trans>
            Its agent last reported {seen}, and the numbers here are from then. Check that <code>dockwell run</code> is
            still running on the server.
          </Trans>
        ) : (
          <Trans>
            Its agent has disconnected. Check that <code>dockwell run</code> is still running on the server.
          </Trans>
        )}
      </Callout>
    );
  }
  if (server.connection === Connection.PENDING) {
    return (
      <Callout tone="info" title={t`${name} is waiting for its agent`}>
        <Trans>
          It joined, but its agent hasn't connected yet, so there's nothing to show. Start it with{" "}
          <code>dockwell run</code> on the server.
        </Trans>
      </Callout>
    );
  }
  return null;
}
