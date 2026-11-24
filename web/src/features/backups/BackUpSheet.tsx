// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { createConnectQueryKey, useQuery, useTransport } from "@connectrpc/connect-query";
import {
  type BackupDestinationInfo,
  BackupMethod,
  BackupService,
  ChangeService,
  ChangeState,
  type ConnectionInfo,
} from "@dockwell/api";
import {
  Button,
  Callout,
  Icon,
  Loading,
  PHONE_QUERY,
  SelectField,
  Sheet,
  Skeleton,
  SkeletonField,
  Switch,
  useMediaQuery,
} from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { useQueryClient } from "@tanstack/react-query";
import { useMemo, useState } from "react";

import { sentence } from "../../lib/errors";
import { formatBytes } from "../../lib/format";
import { useMay } from "../auth/may";
import { useChangeSheet } from "../changes/ChangeSheet";
import { ConnectionSheet } from "../connections/ConnectionSheet";
import { BackupSealFields, useBackupSeal } from "./BackupSealFields";
import { StoragePicker } from "./StoragePicker";
import { freeDestinationName, pickerStatus, useStorage } from "./storage";
import { useVolumeOptions, type VolumeChoice } from "./useBackups";
import "./Backups.css";

type ConnectionModal =
  | { mode: "add" }
  | { mode: "from"; connection: ConnectionInfo }
  | { mode: "rotate"; connection: ConnectionInfo };

/**
 * Backs up one volume now. It asks the server what that would take first, and says
 * so before anything runs: a database's dump stops nothing, and a copy of a running
 * app's files is only as good as what the app was doing, so stopping it is the
 * default, and a word first (ADR 0033).
 */
export function BackUpSheet({
  volumes,
  destinations,
  chosen,
  serverId = "",
  onClose,
}: {
  volumes: readonly VolumeChoice[];
  destinations?: readonly BackupDestinationInfo[];
  chosen?: string;
  serverId?: string;
  onClose: () => void;
}) {
  const { t, i18n } = useLingui();
  const transport = useTransport();
  const queries = useQueryClient();
  const sheet = useChangeSheet();
  const phone = useMediaQuery(PHONE_QUERY);
  const client = useMemo(() => createClient(BackupService, transport), [transport]);
  const may = useMay();
  const mayPlanInstall = may(ChangeService.method.proposeChange, { install: true });
  const storage = useStorage(destinations);
  const seal = useBackupSeal();
  const [volumeId, setVolumeId] = useState(chosen ?? volumes[0]?.id ?? "");
  const [stop, setStop] = useState(true);
  const [pickedStorage, setPickedStorage] = useState("");
  const [connModal, setConnModal] = useState<ConnectionModal>();
  const [busy, setBusy] = useState(false);
  const [failure, setFailure] = useState<string>();
  const preview = useQuery(
    BackupService.method.previewBackup,
    { volumeId, ...(serverId ? { server: serverId } : {}) },
    { enabled: volumeId !== "", retry: false },
  );
  const said = preview.data;
  const running = said?.running ?? [];
  const list = new Intl.ListFormat(undefined, { style: "long", type: "conjunction" }).format(running);
  const name = volumes.find((v) => v.id === volumeId)?.name ?? "";
  // Backing up is a developer's, and an admin's in a protected environment (ADR 0049): off, with why, otherwise.
  const mayBackUp = may(BackupService.method.createBackup, {
    environment: volumes.find((v) => v.id === volumeId)?.environment ?? "",
  });
  const options = useVolumeOptions(volumes);
  const risky = said?.method === BackupMethod.COPY_RUNNING;

  const selectedItem = pickedStorage ? storage.items.find((i) => i.key === pickedStorage) : undefined;
  const selectedStatus = selectedItem ? pickerStatus(selectedItem) : undefined;
  const needsDestination =
    selectedItem?.connection !== undefined &&
    selectedItem.destination === undefined &&
    selectedStatus?.state === "available";
  const storageBlocked =
    selectedItem !== undefined &&
    selectedStatus !== undefined &&
    selectedStatus.state !== "ready" &&
    selectedStatus.state !== "available";
  const storageReady = !storageBlocked && (!needsDestination || (seal.ready && mayPlanInstall.allowed));

  const startBackup = async (destinationName: string) => {
    setBusy(true);
    setFailure(undefined);
    try {
      await client.createBackup({
        volumeId,
        ...(serverId ? { server: serverId } : {}),
        stop: risky && stop,
        destination: destinationName,
      });
      await queries.invalidateQueries({
        queryKey: createConnectQueryKey({ schema: BackupService.method.listBackups, cardinality: undefined }),
      });
      onClose();
    } catch (error) {
      setFailure(sentence(error));
      setBusy(false);
    }
  };

  const submit = () => {
    if (!storageReady) return;
    if (needsDestination && selectedItem?.connection) {
      const destName = freeDestinationName(selectedItem.connection.name, storage.destinations);
      sheet.open({
        kind: "propose",
        summary: t`Send backups to ${destName}`,
        edits: [
          {
            resource: { kind: "backup.destination", name: destName },
            spec: {
              kind: {
                case: "backupDestination",
                value: {
                  connection: selectedItem.connection.name,
                  prefix: seal.prefix.trim(),
                  passphrase: seal.passphrase,
                },
              },
            },
          },
        ],
        onClose: (change) => {
          if (change?.state === ChangeState.APPLIED) void startBackup(destName);
        },
      });
      return;
    }
    const destinationName =
      selectedItem?.destination?.name ?? (pickedStorage.startsWith("dest:") ? pickedStorage.slice(5) : "");
    void startBackup(destinationName);
  };

  return (
    <Sheet
      open
      onClose={onClose}
      title={t`Back up ${name}`}
      className={phone ? "is-phone" : undefined}
      footer={
        <>
          <Button variant="secondary" onClick={onClose}>
            <Trans>Cancel</Trans>
          </Button>
          <Button
            variant="primary"
            icon="archive"
            loading={busy}
            disabled={!said || volumeId === "" || !storageReady}
            onClick={submit}
            {...mayBackUp.off}
          >
            {needsDestination ? <Trans>Review plan and back up</Trans> : <Trans>Back up</Trans>}
          </Button>
        </>
      }
    >
      <div className="backup-sheet">
        {volumes.length > 1 ? (
          <SelectField
            label={t`Volume`}
            mono
            value={volumeId}
            options={options}
            onChange={(event) => {
              setVolumeId(event.target.value);
              setFailure(undefined);
            }}
          />
        ) : null}
        {preview.error ? (
          <Callout tone="critical" title={t`Dockwell can't reach what this would back up`}>
            {sentence(preview.error)}
          </Callout>
        ) : !said ? (
          <Loading label={t`Asking the server what this takes`} slow={t`Asking the server what this takes`}>
            <Skeleton width="88%" />
            <Skeleton width="64%" />
          </Loading>
        ) : said.method === BackupMethod.DUMP ? (
          <p className="backup-sheet-line">
            <Icon name="database" size={14} />
            <Trans>
              This uses {said.database}'s own dump, with the database running, so nothing stops. A dump restores on a
              newer version too.
            </Trans>{" "}
            {said.database === "MySQL" ? (
              <Trans>A MyISAM table written to during the dump may not come out consistent.</Trans>
            ) : null}
          </p>
        ) : risky ? (
          <>
            <Callout
              tone="warning"
              title={plural(running.length, {
                one: `${list} runs on this volume`,
                other: `${list} run on this volume`,
              })}
            >
              <Trans>A copy of a running app's files can hold half a write, and the app may not start from it.</Trans>
            </Callout>
            <Switch
              checked={stop}
              onChange={setStop}
              label={t`Stop ${list} while it copies`}
              description={
                stop
                  ? t`What stops starts again once the copy ends, however that goes. The copy is exact.`
                  : t`What runs keeps running, and the backup says it was copied running.`
              }
            />
          </>
        ) : (
          <p className="backup-sheet-line">
            <Icon name="archive" size={14} />
            <Trans>Nothing runs on this volume, so the copy is exact.</Trans>
          </p>
        )}
        {storage.isPending ? (
          <SkeletonField height={96} />
        ) : storage.items.length > 0 ? (
          <StoragePicker
            items={storage.items}
            value={pickedStorage}
            onChange={setPickedStorage}
            noneLabel={t`Nowhere else`}
            noneHint={t`Stays on the server's own disk, unencrypted`}
            mayPlanInstall={mayPlanInstall}
            onAdd={() => setConnModal({ mode: "add" })}
            onNewFrom={(connection) => setConnModal({ mode: "from", connection })}
            onRotateKeys={(connection) => setConnModal({ mode: "rotate", connection })}
          />
        ) : null}
        {needsDestination ? <BackupSealFields seal={seal} /> : null}
        <p className="backup-sheet-note">
          <Trans>
            It stays on the server's own disk, with owners and permissions as they are, so it goes if the server does.
          </Trans>{" "}
          {said && said.estimateBytes > 0n
            ? t`It takes about ${formatBytes(said.estimateBytes, i18n.locale)}, and the disk has ${formatBytes(said.freeBytes, i18n.locale)} free.`
            : null}
        </p>
        {failure ? (
          <Callout tone="critical" title={t`This didn't start`}>
            {failure}
          </Callout>
        ) : null}
      </div>
      {connModal ? (
        <ConnectionSheet
          kind="storage"
          existing={storage.allConnections}
          editing={connModal.mode === "rotate" ? connModal.connection : undefined}
          rotateForBackups={connModal.mode === "rotate"}
          prefill={
            connModal.mode === "from"
              ? {
                  endpoint: connModal.connection.endpoint,
                  region: connModal.connection.region,
                  pathStyle: connModal.connection.pathStyle,
                }
              : undefined
          }
          onClose={() => setConnModal(undefined)}
          onSaved={(savedName) => {
            setConnModal(undefined);
            setPickedStorage(`conn:${savedName}`);
            void storage.refetch();
          }}
        />
      ) : null}
    </Sheet>
  );
}
