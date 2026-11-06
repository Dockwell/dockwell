// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate, timestampMs } from "@bufbuild/protobuf/wkt";
import { createClient } from "@connectrpc/connect";
import { createConnectQueryKey, useTransport } from "@connectrpc/connect-query";
import { type Backup, BackupMethod, BackupService, BackupState } from "@dockwell/api";
import {
  Button,
  Callout,
  Panel,
  PHONE_QUERY,
  Sheet,
  StatusBadge,
  Table,
  type TableColumn,
  useMediaQuery,
} from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { useQueryClient } from "@tanstack/react-query";
import { useMemo, useState } from "react";

import { sentence } from "../../lib/errors";
import { formatBytes, formatDateTime } from "../../lib/format";
import { useTimeWords } from "../../lib/time";
import { usePlaces } from "../apps/useServices";
import { useChangeSheet } from "../changes/ChangeSheet";
import { useFleetView } from "../servers/useFleet";
import { EmptyState } from "../shell/EmptyState";
import { Page } from "../shell/Page";
import { BackUpSheet } from "./BackUpSheet";
import { PoliciesPanel, usePolicies } from "./PoliciesPanel";
import { PolicySheet } from "./PolicySheet";
import { useBackups, useVolumes } from "./useBackups";
import { backupStatus, useMethodWord, useStateWord } from "./words";
import "./Backups.css";

/**
 * Every backup, newest first: back one up, restore one through a plan, or delete
 * one. A server is a field here, as it is everywhere: with one, it's left out.
 */
export function BackupsPage() {
  const { t } = useLingui();
  const phone = useMediaQuery(PHONE_QUERY);
  const volumes = useVolumes();
  const query = useBackups();
  const sheet = useChangeSheet();
  const transport = useTransport();
  const client = useMemo(() => createClient(BackupService, transport), [transport]);
  const [taking, setTaking] = useState(false);
  const [scheduling, setScheduling] = useState(false);
  const [deleting, setDeleting] = useState<Backup>();
  const backups = query.data?.backups ?? [];
  const policies = usePolicies().data?.policies ?? [];

  const restore = (backup: Backup) => {
    sheet.open({ kind: "plan", make: () => client.restoreBackup({ id: backup.id }) });
  };
  const action =
    volumes.length > 0 ? (
      <Button variant="primary" icon="archive" onClick={() => setTaking(true)}>
        <Trans>Back up now</Trans>
      </Button>
    ) : null;

  const where = t`On each server's own disk, unencrypted, so they go if the server does`;
  return (
    <Page title={t`Backups`} lead={where} phoneSub={where} back="/more" actions={backups.length > 0 ? action : null}>
      {query.error ? (
        <Callout tone="critical" title={t`Dockwell can't read its backups`}>
          {sentence(query.error)}
        </Callout>
      ) : null}
      {query.isPending ? null : backups.length === 0 ? (
        <Panel>
          {volumes.length > 0 ? (
            <EmptyState icon="archive" title={t`No backups yet`} action={action}>
              <Trans>Back up a volume before you change what's in it. A database's backup stops nothing.</Trans>
            </EmptyState>
          ) : (
            <EmptyState icon="archive" title={t`Nothing to back up yet`}>
              <Trans>A backup is of a volume. Add one to an app, or install a database from the catalog.</Trans>
            </EmptyState>
          )}
        </Panel>
      ) : (
        <>
          {phone ? <div className="backup-phone-action">{action}</div> : null}
          <BackupsList backups={backups} onRestore={restore} onDelete={setDeleting} />
        </>
      )}
      {volumes.length > 0 ? <PoliciesPanel policies={policies} onAdd={() => setScheduling(true)} /> : null}
      {taking ? <BackUpSheet volumes={volumes} onClose={() => setTaking(false)} /> : null}
      {scheduling ? <PolicySheet volumes={volumes} onClose={() => setScheduling(false)} /> : null}
      {deleting ? <DeleteSheet backup={deleting} client={client} onClose={() => setDeleting(undefined)} /> : null}
    </Page>
  );
}

function BackupsList({
  backups,
  onRestore,
  onDelete,
}: {
  backups: readonly Backup[];
  onRestore: (backup: Backup) => void;
  onDelete: (backup: Backup) => void;
}) {
  const { t, i18n } = useLingui();
  const phone = useMediaQuery(PHONE_QUERY);
  const view = useFleetView();
  const places = usePlaces();
  const method = useMethodWord();
  const stateWord = useStateWord();
  const time = useTimeWords();
  const fleet = view.fleet;
  const several = new Set(backups.map((b) => b.environmentId)).size > 1;
  const where = (b: Backup) => {
    const place = several ? places.label(b.environmentId) : undefined;
    const server = fleet ? b.serverName : undefined;
    return [place, server].filter(Boolean).join(" · ");
  };
  const when = (b: Backup) => (b.startedAt ? formatDateTime(timestampDate(b.startedAt), i18n.locale) : "");
  const took = (b: Backup) =>
    b.startedAt && b.finishedAt ? time.span(timestampMs(b.finishedAt) - timestampMs(b.startedAt)) : "";
  const state = (b: Backup) => (
    <div className="backup-state">
      <StatusBadge status={backupStatus(b)} label={stateWord(b)} labelIsState />
      {b.state === BackupState.FAILED ? <span className="backup-why">{b.error}</span> : null}
      {b.state === BackupState.RUNNING && b.progress ? <span className="backup-why">{b.progress}</span> : null}
      {b.filesChanged > 0n ? (
        <span className="backup-why">
          {plural(Number(b.filesChanged), {
            one: "# file changed while it copied",
            other: "# files changed while it copied",
          })}
        </span>
      ) : null}
    </div>
  );
  const actions = (b: Backup) =>
    b.state === BackupState.RUNNING ? null : (
      <span className="backup-actions">
        {b.state === BackupState.DONE ? (
          <Button size="sm" icon="undo" onClick={() => onRestore(b)}>
            <Trans>Restore</Trans>
          </Button>
        ) : null}
        <Button size="sm" variant="ghost" icon="trash" aria-label={t`Delete this backup`} onClick={() => onDelete(b)} />
      </span>
    );
  const columns: TableColumn<Backup>[] = [
    { key: "started", header: t`Started`, width: 160, render: when },
    {
      key: "volume",
      header: t`Volume`,
      render: (b) => (
        <div className="backup-state">
          <code>{b.volume}</code>
          {where(b) ? <span className="backup-why">{where(b)}</span> : null}
        </div>
      ),
    },
    { key: "state", header: t`Status`, render: state },
    { key: "how", header: t`How`, render: (b) => method(b.method, b.database) },
    {
      key: "size",
      header: t`Size`,
      align: "right",
      width: 88,
      render: (b) => (b.sizeBytes > 0n ? formatBytes(b.sizeBytes, i18n.locale) : ""),
    },
    { key: "took", header: t`Took`, align: "right", width: 88, muted: true, render: took },
    {
      key: "actions",
      header: <span className="dw-sr">{t`Actions`}</span>,
      width: 140,
      align: "right",
      render: actions,
    },
  ];
  if (phone) {
    return (
      <ul className="backup-phone">
        {backups.map((b) => (
          <li key={b.id}>
            <div className="backup-phone-head">
              <code>{b.volume}</code>
              {actions(b)}
            </div>
            {state(b)}
            <span className="backup-why">
              {[
                when(b),
                method(b.method, b.database),
                b.sizeBytes > 0n ? formatBytes(b.sizeBytes, i18n.locale) : "",
                where(b),
              ]
                .filter(Boolean)
                .join(" · ")}
            </span>
          </li>
        ))}
      </ul>
    );
  }
  return <Table<Backup> className="backup-table" columns={columns} rows={backups} caption={t`Backups, newest first`} />;
}

/** Deleting a backup can't be taken back, so it asks. */
function DeleteSheet({
  backup,
  client,
  onClose,
}: {
  backup: Backup;
  client: ReturnType<typeof createClient<typeof BackupService>>;
  onClose: () => void;
}) {
  const { t } = useLingui();
  const queries = useQueryClient();
  const [busy, setBusy] = useState(false);
  const [failure, setFailure] = useState<string>();
  const failed = backup.state === BackupState.FAILED;
  const remove = async () => {
    setBusy(true);
    setFailure(undefined);
    try {
      await client.deleteBackup({ id: backup.id });
      await queries.invalidateQueries({
        queryKey: createConnectQueryKey({ schema: BackupService.method.listBackups, cardinality: "finite" }),
      });
      onClose();
    } catch (error) {
      setFailure(sentence(error));
      setBusy(false);
    }
  };
  return (
    <Sheet
      open
      onClose={onClose}
      title={t`Delete this backup of ${backup.volume}`}
      footer={
        <>
          <Button variant="secondary" onClick={onClose}>
            <Trans>Cancel</Trans>
          </Button>
          <Button variant="danger" icon="trash" loading={busy} onClick={() => void remove()}>
            <Trans>Delete it</Trans>
          </Button>
        </>
      }
    >
      <div className="backup-sheet">
        <p className="backup-sheet-line">
          {failed ? (
            <Trans>This backup failed, so nothing is on the server. This removes the record of it.</Trans>
          ) : backup.method === BackupMethod.UNSPECIFIED ? null : (
            <Trans>This removes it from the server's disk. Nothing brings it back.</Trans>
          )}
        </p>
        {failure ? (
          <Callout tone="critical" title={t`It wasn't deleted`}>
            {failure}
          </Callout>
        ) : null}
      </div>
    </Sheet>
  );
}
