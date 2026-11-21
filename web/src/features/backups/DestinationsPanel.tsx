// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate } from "@bufbuild/protobuf/wkt";
import { useQuery } from "@connectrpc/connect-query";
import { type BackupDestinationInfo, BackupService, ChangeService } from "@dockwell/api";
import { Button, Loading, Panel, SkeletonRows } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link } from "@tanstack/react-router";
import { useState } from "react";

import { formatBytes, formatDateTime } from "../../lib/format";
import { StateLine } from "../apps/DomainStep";
import { useMay } from "../auth/may";
import { useChangeSheet } from "../changes/ChangeSheet";
import { ConnectionSheet } from "../connections/ConnectionSheet";
import { ConnectionMark, DestinationMark } from "../connections/ProviderMark";
import { pickerStatus, useStorage } from "./storage";
import "./Backups.css";

/** The storage backups can go to. */
export function useDestinations() {
  return useQuery(BackupService.method.listBackupDestinations, {});
}

/** Storage where backups go: backup destinations and unused S3 connections that can take backups. */
export function DestinationsPanel({
  destinations,
  pending = false,
}: {
  destinations: readonly BackupDestinationInfo[];
  /** The destinations are on their way, so none isn't known to be none yet. */
  pending?: boolean;
}) {
  const { t, i18n } = useLingui();
  const sheet = useChangeSheet();
  const storage = useStorage(destinations);
  const [adding, setAdding] = useState(false);
  // Storage belongs to no project, so adding or removing it is an admin's: off, with why, otherwise (ADR 0059).
  const mayPlan = useMay()(ChangeService.method.proposeChange, { install: true });
  // Leave out connections that can't take backups (app-serving, shared, or key-held); the picker shows them disabled with the way out.
  const usable = storage.items.filter((item) => {
    const state = pickerStatus(item).state;
    return state === "ready" || state === "available";
  });

  return (
    <Panel
      title={t`Destinations`}
      flush
      actions={
        // While there's none, the empty state's Add storage is the one way in.
        usable.length > 0 ? (
          <Button size="sm" variant="ghost" icon="plus" onClick={() => setAdding(true)} {...mayPlan.off}>
            <Trans>Add</Trans>
          </Button>
        ) : undefined
      }
    >
      {pending || storage.isPending ? (
        <Loading label={t`Loading destinations`}>
          <SkeletonRows rows={2} lead="mark" />
        </Loading>
      ) : usable.length === 0 ? (
        // A state with its one action, not an empty page, so this column stays as short as its neighbour (#793).
        <div className="backup-destinations-none">
          <StateLine
            status="degraded"
            action={
              <button
                type="button"
                className="domain-step-act"
                onClick={() => setAdding(true)}
                disabled={!mayPlan.allowed}
                title={mayPlan.reason || undefined}
              >
                <Trans>Add storage</Trans>
              </button>
            }
          >
            {t`Only on each server's disk, unencrypted`}
          </StateLine>
        </div>
      ) : (
        <ul className="backup-policies">
          {usable.map((item) => {
            const d = item.destination;
            const c = item.connection;
            return (
              <li key={item.key} className="backup-policy">
                <div className="backup-dest-head">
                  {c ? <ConnectionMark connection={c} /> : d ? <DestinationMark destination={d} /> : null}
                  <div className="backup-state">
                    {c ? (
                      <Link to="/connections/$name" params={{ name: c.name }} className="backup-dest-link">
                        {c.name}
                      </Link>
                    ) : (
                      <span>{item.name}</span>
                    )}
                    {item.bucket ? <code className="backup-why">{item.bucket}</code> : null}
                  </div>
                </div>
                <div className="backup-state">
                  {d ? (
                    <>
                      <span className="backup-why">{t`${d.backups} backups, ${formatBytes(d.usedBytes, i18n.locale)}`}</span>
                      {d.lastUpload ? (
                        <span className="backup-why">
                          {t`Last upload ${formatDateTime(timestampDate(d.lastUpload), i18n.locale)}`}
                        </span>
                      ) : null}
                      <span className="backup-why">{t`Encrypted on each server before upload`}</span>
                    </>
                  ) : (
                    <span className="backup-why">{t`Connection · pick in a schedule or Back up now`}</span>
                  )}
                </div>
                {d ? (
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
                ) : c ? (
                  <Link to="/connections/$name" params={{ name: c.name }} className="dw-btn dw-btn-ghost dw-btn-sm">
                    <Trans>Manage</Trans>
                  </Link>
                ) : null}
              </li>
            );
          })}
        </ul>
      )}
      {adding ? (
        <ConnectionSheet
          kind="storage"
          existing={storage.allConnections}
          onClose={() => setAdding(false)}
          onSaved={() => void storage.refetch()}
        />
      ) : null}
    </Panel>
  );
}
