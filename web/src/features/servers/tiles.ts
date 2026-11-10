// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate } from "@bufbuild/protobuf/wkt";
import { Connection, type Server, ServerStrain } from "@dockwell/api";
import type { FleetServer } from "@dockwell/ui";
import { useLingui } from "@lingui/react/macro";

import { useTimeWords } from "../../lib/time";
import { publicAddressOf } from "./network";
import { HIGH_USE, primaryAddress, serverStatus } from "./server";
import { useControlPlaneNow } from "./useFleet";

/**
 * The address to show for a server: its public one as its network says, which a role
 * that sees no inventory still gets, then the best of its own interfaces.
 */
function addressOf(server: Server): string | undefined {
  return publicAddressOf(server) ?? primaryAddress(server.inventory?.addresses ?? []);
}

/**
 * Each server as a tile names it: its state, its numbers while it reports, its address, and why it's not well.
 * While what the fleet last said can't stand, nobody can say how any server is, so each reads Unknown.
 */
export function useServerTiles(servers: readonly Server[], unknown: boolean): FleetServer[] {
  const { t } = useLingui();
  const words = useTimeWords();
  const now = useControlPlaneNow(15_000);
  return servers.map((s) => {
    const stats = s.connection === Connection.CONNECTED ? s.stats : undefined;
    const total = Number(stats?.memoryTotalBytes ?? 0n);
    const seen = s.lastSeenTime ? words.ago(timestampDate(s.lastSeenTime), now) : undefined;
    const note =
      s.connection === Connection.UNREACHABLE
        ? seen
          ? t`Last reported ${seen}`
          : t`Agent not connected`
        : s.connection === Connection.PENDING
          ? t`Waiting for its agent`
          : s.dockerUnreadableSince
            ? t`Can't read Docker`
            : s.strainedSince
              ? // Why it reads Degraded, in the server list's words.
                s.strain === ServerStrain.CPU
                ? t`CPU above ${HIGH_USE}%`
                : t`Memory above ${HIGH_USE}%`
              : undefined;
    if (unknown) return { id: s.id, name: s.name, status: "unknown", ip: addressOf(s) };
    return {
      id: s.id,
      name: s.name,
      status: serverStatus(s),
      cpu: stats ? Math.round(stats.cpuPercent) : null,
      mem: stats && total > 0 ? Math.round((Number(stats.memoryUsedBytes) / total) * 100) : null,
      ip: addressOf(s),
      note,
    };
  });
}
