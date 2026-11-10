// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate } from "@bufbuild/protobuf/wkt";
import { useQuery } from "@connectrpc/connect-query";
import { type BackupDestinationInfo, BackupService, ChangeService } from "@dockwell/api";
import { Button, Panel } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { useState } from "react";

import { formatBytes, formatDateTime } from "../../lib/format";
import { useChangeSheet } from "../changes/ChangeSheet";
import { DestinationSheet } from "./DestinationSheet";
import "./Backups.css";
import { useMay } from "../auth/may";

/** The storage backups can go to. */
export function useDestinations() {
  return useQuery(BackupService.method.listBackupDestinations, {});
}

/** The names a form offers: none is a choice too, so it starts there. */
export function destinationOptions(list: readonly BackupDestinationInfo[], none: string) {
  return [{ value: "", label: none }, ...list.map((d) => ({ value: d.name, label: d.name }))];
}

/** Each destination: where it is, what's there, and when something last got there. */
export function DestinationsPanel({ destinations }: { destinations: readonly BackupDestinationInfo[] }) {
  const { t, i18n } = useLingui();
  const sheet = useChangeSheet();
  const [adding, setAdding] = useState(false);
  // A destination belongs to no project, so it's an admin's to add or remove: off, with why, otherwise (ADR 0059).
  const mayPlan = useMay()(ChangeService.method.proposeChange, { install: true });
  return (
    <Panel
      title={t`Destinations`}
      flush
      actions={
        <Button size="sm" variant="ghost" icon="plus" onClick={() => setAdding(true)} {...mayPlan.off}>
          <Trans>Add</Trans>
        </Button>
      }
    >
      {destinations.length === 0 ? (
        <p className="backup-policy-empty">
          <Trans>
            Backups stay on each server's disk until you add somewhere else to keep them. Any S3-compatible storage
            works, such as Cloudflare R2, Backblaze B2 or Amazon S3.
          </Trans>
        </p>
      ) : (
        <ul className="backup-policies">
          {destinations.map((d) => (
            <li key={d.id} className="backup-policy">
              <div className="backup-state">
                <span>{d.name}</span>
                <code className="backup-why">{d.bucket}</code>
              </div>
              <div className="backup-state">
                <span className="backup-why">{t`${d.backups} backups, ${formatBytes(d.usedBytes, i18n.locale)}`}</span>
                {d.lastUpload ? (
                  <span className="backup-why">
                    {t`Last upload ${formatDateTime(timestampDate(d.lastUpload), i18n.locale)}`}
                  </span>
                ) : null}
                <span className="backup-why">{t`Encrypted on each server before upload`}</span>
              </div>
              <Button
                size="sm"
                variant="ghost"
                icon="trash"
                aria-label={t`Remove this destination`}
                {...mayPlan.off}
                onClick={() =>
                  sheet.open({
                    kind: "propose",
                    summary: t`Stop sending backups to ${d.name}`,
                    edits: [{ resource: { kind: "backup.destination", name: d.name }, delete: true }],
                  })
                }
              />
            </li>
          ))}
        </ul>
      )}
      {adding ? <DestinationSheet existing={destinations} onClose={() => setAdding(false)} /> : null}
    </Panel>
  );
}
