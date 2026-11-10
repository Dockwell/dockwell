// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate } from "@bufbuild/protobuf/wkt";
import { useQuery } from "@connectrpc/connect-query";
import { BackupCopy, BackupPolicy_Drill, type BackupPolicyInfo, BackupService, BackupState } from "@dockwell/api";
import { Button, Panel, StatusBadge } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";

import { formatBytes, formatDateTime } from "../../lib/format";
import { useChangeSheet } from "../changes/ChangeSheet";
import { useScheduleWords } from "./words";
import "./Backups.css";

/** How often a list of schedules is read again, for each's next run and last. */
const REFRESH = 30_000;

/** The schedules there are. */
export function usePolicies() {
  return useQuery(BackupService.method.listBackupPolicies, {}, { refetchInterval: REFRESH });
}

/** Each schedule: what it does, when next, and how its last run went. */
export function PoliciesPanel({ policies, onAdd }: { policies: readonly BackupPolicyInfo[]; onAdd: () => void }) {
  const { t } = useLingui();
  return (
    <Panel
      title={t`Schedules`}
      flush
      actions={
        <Button size="sm" variant="ghost" icon="plus" onClick={onAdd}>
          <Trans>Add</Trans>
        </Button>
      }
    >
      {policies.length === 0 ? (
        <p className="backup-policy-empty">
          <Trans>No schedules yet. A backup you take by hand stays until you delete it.</Trans>
        </p>
      ) : (
        <ul className="backup-policies">
          {policies.map((policy) => (
            <PolicyRow key={policy.id} policy={policy} />
          ))}
        </ul>
      )}
    </Panel>
  );
}

function PolicyRow({ policy }: { policy: BackupPolicyInfo }) {
  const { t, i18n } = useLingui();
  const sheet = useChangeSheet();
  const words = useScheduleWords();
  const volume = policy.spec?.volume ?? "";
  const failed = policy.last?.state === BackupState.FAILED;
  const copyFailed = policy.last?.copy === BackupCopy.FAILED;
  // In the schedule's own zone, as its time is, and named, as that may not be yours.
  const zone = policy.spec?.timeZone || "UTC";
  const next = policy.nextRun ? formatDateTime(timestampDate(policy.nextRun), i18n.locale, zone) : "";
  const keeps = t`Keeps ${policy.spec?.keep ?? 0}, holding ${policy.kept} now (${formatBytes(policy.keptBytes, i18n.locale)})`;
  const remove = () =>
    sheet.open({
      kind: "propose",
      summary: t`Stop backing up ${volume} on a schedule`,
      edits: [{ resource: { kind: "backup.policy", scope: policy.environmentId, name: policy.name }, delete: true }],
    });
  return (
    <li className="backup-policy">
      <div className="backup-state">
        <code>{volume}</code>
        <span className="backup-why">{policy.spec ? t`${words(policy.spec)}, ${zone}` : ""}</span>
        <span className="backup-why">{keeps}</span>
        <span className="backup-why">
          {policy.spec?.drill === BackupPolicy_Drill.WEEKLY
            ? t`Checks a restore weekly`
            : policy.spec?.drill === BackupPolicy_Drill.MONTHLY
              ? t`Checks a restore monthly`
              : t`Doesn't check a restore`}
        </span>
      </div>
      <div className="backup-state">
        {policy.last ? (
          <StatusBadge
            status={
              failed
                ? "down"
                : copyFailed
                  ? "degraded"
                  : policy.last.state === BackupState.RUNNING
                    ? "deploying"
                    : "healthy"
            }
            label={
              failed
                ? t`Last run failed`
                : copyFailed
                  ? t`Last copy failed`
                  : policy.last.state === BackupState.RUNNING
                    ? t`Running`
                    : t`On schedule`
            }
            labelIsState
          />
        ) : (
          <StatusBadge status="unknown" label={t`Hasn't run yet`} labelIsState />
        )}
        {failed ? <span className="backup-why">{policy.last?.error}</span> : null}
        {copyFailed ? <span className="backup-why">{policy.last?.copyError}</span> : null}
        <span className="backup-why">{t`Next run ${next}`}</span>
      </div>
      <Button size="sm" variant="ghost" icon="trash" aria-label={t`Stop this schedule`} onClick={remove} />
    </li>
  );
}
