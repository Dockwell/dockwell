// SPDX-License-Identifier: AGPL-3.0-only

import type { ConnectError } from "@connectrpc/connect";
import { cx, Glyph } from "@dockwell/ui";
import { Trans } from "@lingui/react/macro";

import { useElapsed } from "../../lib/useElapsed";
import "./FleetNotes.css";

/** For a page's status slot while the live view catches up: what's shown may be a few seconds old. */
export function Reconnecting() {
  return (
    <span className="fleet-reconnecting" role="status">
      <Glyph shape="spinner" />
      <Trans>Reconnecting</Trans>
    </span>
  );
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
