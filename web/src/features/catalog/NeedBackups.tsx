// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate } from "@bufbuild/protobuf/wkt";
import { createClient } from "@connectrpc/connect";
import { useQuery, useTransport } from "@connectrpc/connect-query";
import { BackupPolicy_Every, BackupState, type CatalogInstall, CatalogService, ChangeService } from "@dockwell/api";
import { Button, Callout, Icon } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { useState } from "react";

import { sentence } from "../../lib/errors";
import { formatBytes, formatDateTime } from "../../lib/format";
import { useMay } from "../auth/may";
import { usePolicies } from "../backups/PoliciesPanel";
import { policyName, useScheduleWords } from "../backups/words";
import { useChangeSheet } from "../changes/ChangeSheet";
import "../backups/Backups.css";

/** How many of a database's backups the line shows, newest first. */
const shown = 5;

/**
 * An app's own database's backups, taken for its need, on whatever server meets it:
 * the newest few, the way to take one, and each one's restore, of that database
 * alone, as a plan (ADR 0084).
 */
export function NeedBackups({ install, need }: { install: CatalogInstall; need: string }) {
  const { t, i18n } = useLingui();
  const transport = useTransport();
  const sheet = useChangeSheet();
  const may = useMay();
  const [failed, setFailed] = useState<string>();
  const [taking, setTaking] = useState(false);
  const id = install.projectId;
  const listed = useQuery(
    CatalogService.method.listCatalogNeedBackups,
    { id, need, pageSize: shown },
    {
      // While one runs, it's asked again until it ends.
      refetchInterval: (q) => (q.state.data?.backups.some((b) => b.state === BackupState.RUNNING) ? 2000 : false),
    },
  );
  const backups = listed.data?.backups ?? [];
  const client = createClient(CatalogService, transport);
  const backUp = async () => {
    setFailed(undefined);
    setTaking(true);
    try {
      await client.backUpCatalogNeed({ id, need });
      await listed.refetch();
    } catch (error) {
      setFailed(sentence(error));
    } finally {
      setTaking(false);
    }
  };
  const restore = (backupId: string) =>
    sheet.open({ kind: "plan", make: () => client.restoreCatalogNeed({ id, need, backupId }) });
  // Its schedule: a policy of its own, in the app's environment, on its link.
  const words = useScheduleWords();
  const environment = install.environmentId;
  const schedule = (usePolicies().data?.policies ?? []).find(
    (p) => p.environmentId === environment && p.spec?.link === need,
  );
  const mayPlan = may(ChangeService.method.proposeChange, { environment });
  const how = schedule?.spec ? words(schedule.spec) : "";
  const keeps = schedule?.spec?.keep ?? 0;
  const daily = () =>
    sheet.open({
      kind: "propose",
      summary: t`Back up ${need}'s database daily`,
      edits: [
        {
          resource: { kind: "backup.policy", scope: environment, name: policyName(need, "day") },
          spec: {
            kind: { case: "backupPolicy", value: { link: need, every: BackupPolicy_Every.DAY, at: "03:00", keep: 7 } },
          },
        },
      ],
    });
  const stop = (name: string) =>
    sheet.open({
      kind: "propose",
      summary: t`Stop backing up ${need}'s database on a schedule`,
      edits: [{ resource: { kind: "backup.policy", scope: environment, name }, delete: true }],
    });
  const state = (s: BackupState) =>
    s === BackupState.RUNNING ? t`Backing up` : s === BackupState.FAILED ? t`Failed` : "";
  return (
    <div className="catalog-need-backups">
      {failed ? (
        <Callout tone="critical" title={t`It wasn't backed up`}>
          {failed}
        </Callout>
      ) : null}
      <ul className="app-backups" aria-label={t`Its database's backups`}>
        {backups.length === 0 ? (
          <li className="app-backup">
            <span className="app-backup-latest">
              {listed.isPending ? t`Reading its backups` : t`Its database isn't backed up alone yet`}
            </span>
          </li>
        ) : null}
        {backups.map((b) => (
          <li key={b.id} className="app-backup">
            <span className="app-backup-name">
              <Icon name="archive" size={14} />
              <code>{b.databaseName}</code>
            </span>
            <span className="app-backup-latest">
              {[
                b.startedAt ? formatDateTime(timestampDate(b.startedAt), i18n.locale) : "",
                b.state === BackupState.DONE ? formatBytes(b.sizeBytes, i18n.locale) : state(b.state),
              ]
                .filter(Boolean)
                .join(" · ")}
            </span>
            <Button
              size="sm"
              variant="ghost"
              disabled={b.state !== BackupState.DONE}
              onClick={() => restore(b.id)}
              {...may(CatalogService.method.restoreCatalogNeed, { project: id }).off}
            >
              <Trans>Restore</Trans>
            </Button>
          </li>
        ))}
      </ul>
      <div className="app-backup">
        <span className="app-backup-latest">{schedule?.spec ? t`${how}, keeps ${keeps}` : t`Only by hand`}</span>
        {schedule ? (
          <Button size="sm" variant="ghost" onClick={() => stop(schedule.name)} {...mayPlan.off}>
            <Trans>Stop the schedule</Trans>
          </Button>
        ) : (
          <Button size="sm" variant="ghost" onClick={daily} {...mayPlan.off}>
            <Trans>Back up daily</Trans>
          </Button>
        )}
      </div>
      <Button
        size="sm"
        icon="archive"
        aria-busy={taking}
        onClick={() => void backUp()}
        {...may(CatalogService.method.backUpCatalogNeed, { project: id }).off}
      >
        <Trans>Back up now</Trans>
      </Button>
    </div>
  );
}
