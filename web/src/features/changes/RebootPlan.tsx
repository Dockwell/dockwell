// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate } from "@bufbuild/protobuf/wkt";
import { useQuery } from "@connectrpc/connect-query";
import { BackupCopy, BackupService, BackupState, type HostReboot } from "@dockwell/api";
import { useLingui } from "@lingui/react/macro";

import { useTimeWords } from "../../lib/time";
import { useMay } from "../auth/may";
import { useControlPlaneNow, useFleetView } from "../servers/useFleet";
import { DockerStops } from "./DockerStops";

/** How old a volume's newest backup off its server may be before a reboot's plan names it. */
const STALE_MS = 24 * 3600_000;

/**
 * What rebooting a server does, under its step (ADR 0064): on the control server,
 * that the panel and the API stop with it; how long its last reboot took, once
 * Dockwell has seen one; what goes offline, with whether Docker starts each again;
 * and the volumes whose newest backup off the server is missing or old.
 */
export function RebootPlan({ reboot, server, serverId }: { reboot: HostReboot; server: string; serverId: string }) {
  const { t } = useLingui();
  const time = useTimeWords();
  const tookMs = reboot.lastTook ? Number(reboot.lastTook.seconds) * 1000 + reboot.lastTook.nanos / 1e6 : 0;
  const took = time.bound(tookMs);
  // Only a fleet has other servers to keep serving.
  const fleet = useFleetView().fleet;
  return (
    <div className="plan-install">
      <span className="plan-step-meta">
        {reboot.controlServer ? (
          <span className="plan-step-warn">
            {fleet
              ? t`The panel and the API stop with ${server} and come back on their own; the other servers keep serving`
              : t`The panel and the API stop with ${server} and come back on their own`}
          </span>
        ) : null}
        {tookMs > 0 ? <span>{t`Its last reboot took ${took}`}</span> : null}
        <DockerStops serverId={serverId} server={server} until="reboot" />
        <AtRisk serverId={serverId} server={server} />
      </span>
    </div>
  );
}

/** The volumes on a server whose newest backup off it is missing, or more than a day old. */
function AtRisk({ serverId, server }: { serverId: string; server: string }) {
  const { t } = useLingui();
  const words = useTimeWords();
  const now = useControlPlaneNow();
  const may = useMay()(BackupService.method.listBackups).allowed;
  const backups = useQuery(BackupService.method.listBackups, { pageSize: 200 }, { enabled: may });
  const byVolume = new Map<string, { name: string; off?: Date }>();
  for (const b of backups.data?.backups ?? []) {
    if (b.serverId !== serverId || b.state !== BackupState.DONE) continue;
    const was = byVolume.get(b.volumeId) ?? { name: b.volume };
    if (b.copy === BackupCopy.STORED && b.finishedAt) {
      const at = timestampDate(b.finishedAt);
      if (!was.off || at > was.off) was.off = at;
    }
    byVolume.set(b.volumeId, was);
  }
  const risky = [...byVolume.entries()]
    .filter(([, v]) => !v.off || now - v.off.getTime() > STALE_MS)
    .sort(([, a], [, b]) => a.name.localeCompare(b.name));
  if (risky.length === 0) return null;
  return (
    <ul className="plan-stops-list" aria-label={t`Backups a reboot of ${server} puts at risk`}>
      {risky.map(([id, v]) => {
        const name = v.name;
        const ago = v.off ? words.ago(v.off, now) : "";
        return (
          <li key={id}>
            <code>{name}</code>{" "}
            <span className="plan-stops-maybe">
              {v.off ? t`Its newest backup off ${server} is from ${ago}` : t`Backed up only on ${server}`}
            </span>
          </li>
        );
      })}
    </ul>
  );
}
