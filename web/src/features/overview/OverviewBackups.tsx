// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate, timestampMs } from "@bufbuild/protobuf/wkt";
import { BackupCopy, type BackupPolicyInfo, BackupState } from "@dockwell/api";
import { Button, Panel, type Status, StatusBadge } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { useMemo, useState } from "react";

import { formatBytes, formatDateTime } from "../../lib/format";
import { RouterButtonLink } from "../../lib/links";
import { useTimeWords } from "../../lib/time";
import { BackUpSheet } from "../backups/BackUpSheet";
import { useDestinations } from "../backups/DestinationsPanel";
import { usePolicies } from "../backups/PoliciesPanel";
import { useBackups, useVolumes } from "../backups/useBackups";
import { useControlPlaneNow } from "../servers/useFleet";
import "./Overview.css";

/** How the schedules stand, worst first: one that failed outweighs one that's fine. */
function useScheduleState(policies: readonly BackupPolicyInfo[]): { status: Status; label: string } {
  const { t } = useLingui();
  if (policies.length === 0) return { status: "stopped", label: t`No schedule` };
  if (policies.some((p) => p.last?.state === BackupState.FAILED)) return { status: "down", label: t`Last run failed` };
  if (policies.some((p) => p.last?.copy === BackupCopy.FAILED))
    return { status: "degraded", label: t`Last copy failed` };
  if (policies.some((p) => p.last?.state === BackupState.RUNNING)) return { status: "deploying", label: t`Running` };
  if (policies.some((p) => !p.last)) return { status: "unknown", label: t`Hasn't run yet` };
  return { status: "healthy", label: t`On schedule` };
}

/**
 * Backups on the overview, as Overview-Single has them: how the schedules stand,
 * the last backup and the next, where they're kept, and a backup now.
 */
export function OverviewBackups() {
  const { t, i18n } = useLingui();
  const time = useTimeWords();
  const now = useControlPlaneNow(15_000);
  const volumes = useVolumes();
  const backups = useBackups().data?.backups;
  const policies = usePolicies().data?.policies ?? [];
  const destinations = useDestinations().data?.destinations ?? [];
  const [taking, setTaking] = useState(false);
  const state = useScheduleState(policies);

  const last = useMemo(() => (backups ?? []).find((b) => b.state === BackupState.DONE), [backups]);
  const next = useMemo(() => {
    let soonest: BackupPolicyInfo | undefined;
    for (const p of policies) {
      if (!p.nextRun) continue;
      if (!soonest?.nextRun || timestampMs(p.nextRun) < timestampMs(soonest.nextRun)) soonest = p;
    }
    return soonest;
  }, [policies]);
  const kept = useMemo(
    () => [...new Set(policies.map((p) => p.spec?.destination ?? "").filter(Boolean))].join(", "),
    [policies],
  );

  if (volumes.length === 0 && (backups?.length ?? 0) === 0) {
    return (
      <Panel title={t`Backups`}>
        <p className="overview-quiet">
          <Trans>A backup is of a volume. Add one to an app, or install a database from the catalog.</Trans>
        </p>
      </Panel>
    );
  }

  const lastWords = last?.startedAt
    ? [
        time.ago(timestampDate(last.startedAt), now),
        last.volume,
        last.sizeBytes > 0n ? formatBytes(last.sizeBytes, i18n.locale) : "",
        last.finishedAt ? time.span(timestampMs(last.finishedAt) - timestampMs(last.startedAt)) : "",
      ]
        .filter(Boolean)
        .join(" · ")
    : "";
  // In the schedule's own zone, as its time is, and named, as that may not be yours.
  const zone = next?.spec?.timeZone || "UTC";
  const nextWords = next?.nextRun
    ? t`${formatDateTime(timestampDate(next.nextRun), i18n.locale, zone)}, ${zone} · ${next.spec?.volume ?? ""}`
    : "";

  return (
    <Panel title={t`Backups`} actions={<StatusBadge status={state.status} label={state.label} labelIsState />}>
      <div className="overview-backups">
        <dl className="overview-facts">
          <dt>
            <Trans>Last backup</Trans>
          </dt>
          <dd>{lastWords || t`None yet`}</dd>
          <dt>
            <Trans>Next run</Trans>
          </dt>
          <dd>{nextWords || t`None scheduled`}</dd>
          <dt>
            <Trans>Kept</Trans>
          </dt>
          <dd>{kept ? t`On the server, and on ${kept}` : t`On the server's own disk only`}</dd>
        </dl>
        <div className="overview-actions">
          {volumes.length > 0 ? (
            <Button size="sm" icon="archive" onClick={() => setTaking(true)}>
              <Trans>Back up now</Trans>
            </Button>
          ) : null}
          <RouterButtonLink to="/backups" size="sm" variant="ghost">
            <Trans>Restore</Trans>
          </RouterButtonLink>
        </div>
      </div>
      {taking ? <BackUpSheet volumes={volumes} destinations={destinations} onClose={() => setTaking(false)} /> : null}
    </Panel>
  );
}
