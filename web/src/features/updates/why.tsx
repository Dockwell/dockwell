// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate } from "@bufbuild/protobuf/wkt";
import type { DockwellRelease, Server, ServerUpdates } from "@dockwell/api";
import type { GlyphShape, Tone } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import type { ReactNode } from "react";

import { useTimeWords } from "../../lib/time";
import { useControlPlaneNow } from "../servers/useFleet";
import { installCommand } from "./updates";

/** Why what's shown of a server's updates isn't a current read, for people. */
export interface WhyNot {
  title: string;
  body: ReactNode;
  /** A line for the overview's card. */
  short: string;
  tone: Tone;
  shape: GlyphShape;
  /** Whether Check now could help: a read that failed, rather than a server that can't be read. */
  retry: boolean;
}

/**
 * Says why a server's updates aren't a current read, by the reason the control plane
 * gives (ServerUpdates.reason), or undefined when they are. Each says what's true and
 * what to do, in the panel's own words.
 */
export function useWhyNot() {
  const { t } = useLingui();
  const words = useTimeWords();
  const now = useControlPlaneNow();
  return (
    server: Server,
    updates: ServerUpdates | undefined,
    release: DockwellRelease | undefined,
  ): WhyNot | undefined => {
    if (!updates) return undefined;
    const name = server.name;
    const read = updates.readTime ? words.ago(timestampDate(updates.readTime), now) : "";
    const readAt = read ? <> {t`What's here was read ${read}.`}</> : null;
    // One with nothing to read and no reason, as from an older control plane, is one
    // Dockwell can't read.
    const reason = updates.reason || (updates.unavailable ? "unsupported" : "");
    switch (reason) {
      case "unsupported":
        return {
          title: t`Dockwell can't read updates on ${name}`,
          body: (
            <>
              <Trans>
                It reads them with apt on Debian and Ubuntu, dnf on AlmaLinux, Rocky Linux and RHEL, and apk on Alpine.
              </Trans>{" "}
              <span className="updates-detail">{updates.unavailable}</span>
            </>
          ),
          short: t`It reads apt, dnf and apk`,
          tone: "info",
          shape: "dashed-ring",
          retry: false,
        };
      case "agent_outdated": {
        const command = installCommand(release?.releases ?? "", release?.controlPlane ?? "");
        return {
          title: t`${name}'s agent doesn't read updates yet`,
          body: command ? (
            <>
              <Trans>
                It's older than the control plane. Update it by running the install script on {name} again, which keeps
                what runs:
              </Trans>
              <code className="updates-command">{command}</code>
            </>
          ) : (
            <Trans>
              It's older than the control plane. Update it to the control plane's version by running the install script
              on {name} again, which keeps what runs.
            </Trans>
          ),
          short: t`Update its agent to read them`,
          tone: "warning",
          shape: "triangle",
          retry: false,
        };
      }
      case "server_offline":
        return {
          title: t`${name}'s agent isn't connected`,
          body: (
            <>
              {t`Dockwell can't read its updates until it's back.`}
              {readAt}
            </>
          ),
          short: t`Its agent isn't connected`,
          tone: "warning",
          shape: "triangle",
          retry: false,
        };
      case "read_failed":
        return {
          title: t`Dockwell couldn't read ${name}'s updates`,
          body: (
            <>
              <span className="updates-detail">{updates.error}</span>
              {readAt}
            </>
          ),
          short: t`The last read failed`,
          tone: "warning",
          shape: "triangle",
          retry: true,
        };
    }
    return undefined;
  };
}
