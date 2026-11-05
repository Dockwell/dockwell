// SPDX-License-Identifier: AGPL-3.0-only

import type { ConnectError } from "@connectrpc/connect";
import { Callout, cx, Glyph, PHONE_QUERY, StatusBadge, useMediaQuery } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import type { ReactNode } from "react";

import { type Unread, useOnline } from "../../lib/online";
import { useTimeWords } from "../../lib/time";
import { useElapsed } from "../../lib/useElapsed";
import { type FleetState, useAgoNow } from "./useFleet";

/** Why nobody here can say how a server is, as the list says it of apps: the cause, and when the panel last heard. */
export function useFleetUnreadWords(): (why: Unread, heard: Date, now: number, of: "servers" | "server") => string {
  const { t } = useLingui();
  const time = useTimeWords();
  return (why, heard, now, of) => {
    const ago = time.ago(heard, now);
    if (why === "offline") {
      return of === "server"
        ? t`This browser is offline, so the panel can't say how this server is. It last heard ${ago}. Check your connection.`
        : t`This browser is offline, so the panel can't say how your servers are. It last heard ${ago}. Check your connection.`;
    }
    return of === "server"
      ? t`The panel can't reach the control plane, so it can't say how this server is. It last heard ${ago}, and keeps trying.`
      : t`The panel can't reach the control plane, so it can't say how your servers are. It last heard ${ago}, and keeps trying.`;
  };
}

/** A server's state, or the fleet's, once what the panel last heard of it is too old to stand. */
export function FleetUnknown({ why, heard }: { why: Unread; heard: Date }) {
  const { t } = useLingui();
  const words = useFleetUnreadWords();
  const now = useAgoNow(heard);
  return <StatusBadge status="unknown" label={t`Unknown`} title={words(why, heard, now, "servers")} />;
}

/** The same said on a server's page, once, above what it shows. */
export function FleetUnreadNote({ why, heard }: { why: Unread; heard: Date }) {
  const words = useFleetUnreadWords();
  const now = useAgoNow(heard);
  return (
    <Callout tone="info" shape="dashed-ring" className="fleet-unread">
      {words(why, heard, now, "server")}
    </Callout>
  );
}

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
