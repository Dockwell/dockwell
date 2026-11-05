// SPDX-License-Identifier: AGPL-3.0-only

import type { ConnectError } from "@connectrpc/connect";
import { cx, Glyph, PHONE_QUERY, useMediaQuery } from "@dockwell/ui";
import { Trans } from "@lingui/react/macro";
import type { ReactNode } from "react";

import { useOnline } from "../../lib/online";
import { useElapsed } from "../../lib/useElapsed";
import type { FleetState } from "./useFleet";
import "./FleetNotes.css";

/**
 * While the live view catches up: what's shown may be a few seconds old.
 * Show one per screen, so it is seen and announced once.
 */
export function Reconnecting() {
  // A browser that is offline can't reconnect until it isn't, which is the reader's to fix.
  const online = useOnline();
  return (
    <span className="fleet-reconnecting" role="status">
      <Glyph shape={online ? "spinner" : "dashed-ring"} />
      {online ? <Trans>Reconnecting</Trans> : <Trans>You're offline</Trans>}
    </span>
  );
}

/**
 * The note for a page's head while the fleet's watch reconnects. On wide
 * screens the top bar, just above, says it for every page, so a page says
 * it only on phones, which have no top bar.
 */
export function usePageReconnecting(status: FleetState["status"]): ReactNode {
  const phone = useMediaQuery(PHONE_QUERY);
  return status === "reconnecting" && phone ? <Reconnecting /> : undefined;
}

/**
 * While the first list of servers is on its way: nothing at first, so fast
 * answers don't flash, then a quiet note; or why it failed.
 */
export function Connecting({ error }: { error?: ConnectError | undefined }) {
  const slow = useElapsed(true);
  if (error) {
    return (
      <div className="fleet-note">
        <p>
          <Trans>The server list didn't load. Dockwell keeps trying.</Trans>
        </p>
        <p className="fleet-error">{error.rawMessage}</p>
      </div>
    );
  }
  return (
    <p className={cx("fleet-note", !slow && "is-quiet")} aria-busy="true">
      <Trans>Loading servers</Trans>
    </p>
  );
}
