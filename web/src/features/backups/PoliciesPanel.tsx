// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate } from "@bufbuild/protobuf/wkt";
import { useQuery } from "@connectrpc/connect-query";
import {
  BackupCopy,
  BackupPolicy_Drill,
  type BackupPolicyInfo,
  BackupService,
  BackupState,
  ChangeService,
} from "@dockwell/api";
import { Button, Panel, StatusBadge } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { useState } from "react";

import { formatBytes, formatDateTime } from "../../lib/format";
import { StateLine } from "../apps/DomainStep";
import { usePlaces } from "../apps/useServices";
import { useChangeSheet } from "../changes/ChangeSheet";
import { ConnectionSheet } from "../connections/ConnectionSheet";
import { useControlPlaneNow } from "../servers/useFleet";
import { useStorage } from "./storage";
import { useScheduleWords, useWhen } from "./words";
import "./Backups.css";
import { useMay } from "../auth/may";
import { EmptyState } from "../shell/EmptyState";

/** How often a list of schedules is read again, for each's next run and last. */
const REFRESH = 30_000;

/** The schedules there are. */
export function usePolicies() {
  return useQuery(BackupService.method.listBackupPolicies, {}, { refetchInterval: REFRESH });
}

/** Each schedule: what it does, when next, and how its last run went. */
export function PoliciesPanel({
  policies,
  onAdd,
  onEdit,
}: {
  policies: readonly BackupPolicyInfo[];
  onAdd: () => void;
  /** Opens a schedule to change it (#793). */
  onEdit: (policy: BackupPolicyInfo) => void;
}) {
  const { t } = useLingui();
  // A schedule is a plan, which is a developer's: off, with why, for a viewer (ADR 0059).
  const mayPlan = useMay()(ChangeService.method.proposeChange);
  // Where each is, where they're in more than one place, as several volumes share a name such as data.
  const places = usePlaces();
  const several = new Set(policies.map((p) => p.environmentId)).size > 1;
  return (
    <Panel
      title={t`Schedules`}
      flush
      actions={
        // While there's none, the empty state's Add a schedule is the one way in.
        policies.length > 0 ? (
          <Button size="sm" variant="ghost" icon="plus" onClick={onAdd} {...mayPlan.off}>
            <Trans>Add</Trans>
          </Button>
        ) : undefined
      }
    >
      {policies.length === 0 ? (
        <EmptyState
          icon="calendar"
          title={<Trans>No schedules yet</Trans>}
          action={
            <Button size="sm" icon="plus" onClick={onAdd} {...mayPlan.off}>
              <Trans>Add a schedule</Trans>
            </Button>
          }
        >
          <p>
            <Trans>A backup you take by hand stays until you delete it.</Trans>
          </p>
        </EmptyState>
      ) : (
        <ul className="backup-policies">
          {policies.map((policy) => (
            <PolicyRow
              key={policy.id}
              policy={policy}
              place={several ? places.label(policy.environmentId) : ""}
              onEdit={() => onEdit(policy)}
            />
          ))}
        </ul>
      )}
    </Panel>
  );
}

function PolicyRow({ policy, place, onEdit }: { policy: BackupPolicyInfo; place: string; onEdit: () => void }) {
  const { t, i18n } = useLingui();
  const sheet = useChangeSheet();
  const words = useScheduleWords();
  const when = useWhen();
  const now = useControlPlaneNow(30_000);
  const storage = useStorage();
  const [addingStorage, setAddingStorage] = useState(false);
  // An app's database alone goes by its need's link (ADR 0084).
  const volume = policy.spec?.volume || policy.spec?.link || "";
  const failed = policy.last?.state === BackupState.FAILED;
  const copyFailed = policy.last?.copy === BackupCopy.FAILED;
  // In the schedule's own zone, as its time is, and named, as that may not be yours.
  const zone = policy.spec?.timeZone || "UTC";
  const nextAt = policy.nextRun ? timestampDate(policy.nextRun) : undefined;
  const next = nextAt ? when(nextAt, now, zone) : "";
  const nextWhole = nextAt ? formatDateTime(nextAt, i18n.locale, zone) : "";
  const sentTo = policy.spec?.destination ?? "";
  const keeps = t`Keeps ${policy.spec?.keep ?? 0}, holding ${policy.kept} now (${formatBytes(policy.keptBytes, i18n.locale)})`;
  const may = useMay();
  const mayRemove = may(ChangeService.method.proposeChange, { environment: policy.environmentId });
  const mayAddStorage = may(ChangeService.method.proposeChange, { install: true });
  const actOff = mayRemove.allowed ? {} : { disabled: true, title: mayRemove.reason || undefined };
  const addOff = mayAddStorage.allowed ? {} : { disabled: true, title: mayAddStorage.reason || undefined };
  const remove = () =>
    sheet.open({
      kind: "propose",
      summary: t`Stop backing up ${volume} on a schedule`,
      edits: [{ resource: { kind: "backup.policy", scope: policy.environmentId, name: policy.name }, delete: true }],
    });
  return (
    <li className="backup-policy">
      <div className="backup-state">
        <span>
          <code>{volume}</code>
          {place ? <span className="backup-why backup-place"> · {place}</span> : null}
        </span>
        <span className="backup-why">{policy.spec ? t`${words(policy.spec)}, ${zone}` : ""}</span>
        {policy.spec?.link ? <span className="backup-why">{t`Its app's database alone`}</span> : null}
        <span className="backup-why">{keeps}</span>
        <span className="backup-why">
          {policy.spec?.drill === BackupPolicy_Drill.WEEKLY
            ? t`Tests a restore weekly`
            : policy.spec?.drill === BackupPolicy_Drill.MONTHLY
              ? t`Tests a restore monthly`
              : t`Never tests a restore`}
        </span>
        {sentTo ? (
          <span className="backup-why">{t`Copies sent to ${sentTo}, encrypted`}</span>
        ) : (
          // Kept on the disk it would be lost with: a warning, with the one thing that fixes it (#793).
          <StateLine
            status="degraded"
            action={
              storage.items.length > 0 ? (
                <button type="button" className="domain-step-act" onClick={onEdit} {...actOff}>
                  <Trans>Send copies off the server</Trans>
                </button>
              ) : (
                <button type="button" className="domain-step-act" onClick={() => setAddingStorage(true)} {...addOff}>
                  <Trans>Add storage</Trans>
                </button>
              )
            }
          >
            {t`Kept only on the server, unencrypted`}
          </StateLine>
        )}
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
        <span className="backup-why" title={nextWhole}>
          {t`Next run ${next}`}
        </span>
      </div>
      <div className="backup-policy-actions">
        <Button
          size="sm"
          variant="ghost"
          icon="edit"
          aria-label={t`Edit this schedule`}
          onClick={onEdit}
          {...mayRemove.off}
        />
        <Button
          size="sm"
          variant="ghost"
          icon="trash"
          aria-label={t`Stop this schedule`}
          onClick={remove}
          {...mayRemove.off}
        />
      </div>
      {addingStorage ? (
        <ConnectionSheet kind="storage" existing={storage.allConnections} onClose={() => setAddingStorage(false)} />
      ) : null}
    </li>
  );
}
