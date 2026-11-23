// SPDX-License-Identifier: AGPL-3.0-only

import { useQuery } from "@connectrpc/connect-query";
import {
  BackupService,
  DockerCleanupKind,
  DockerObjectKind,
  DockerOwnership,
  DockerService,
  FileService,
  VolumeService,
} from "@dockwell/api";
import { Button, Callout, Loading, Panel, Skeleton, SkeletonCard, SkeletonRows, SlowLine } from "@dockwell/ui";
import { useLingui } from "@lingui/react/macro";
import { Link } from "@tanstack/react-router";
import { useState } from "react";
import { sentence } from "../../lib/errors";
import { formatBytes } from "../../lib/format";
import { RouterButtonLink } from "../../lib/links";
import { useMay } from "../auth/may";
import { BackUpSheet } from "../backups/BackUpSheet";
import { useDestinations } from "../backups/DestinationsPanel";
import { askable } from "../files/lookup";
import { EmptyState } from "../shell/EmptyState";
import { DockerPruneSheet } from "./DockerPruneSheet";
import { formatMeasurement } from "./DockerStorage";
import { useDockerWatch } from "./live";
import { Fact, LiveUpdates, MeasuringSize } from "./parts";
import { StorageAttachments, StorageDetailLoading, StoragePage } from "./storageParts";
import { useVolumeMeasurements } from "./volumeMeasurements";

export function DockerVolumeDetail({ serverId, volumeKey }: { serverId: string; volumeKey: string }) {
  return <VolumeDetail key={`${serverId}/${volumeKey}`} serverId={serverId} volumeKey={volumeKey} />;
}

function VolumeDetail({ serverId, volumeKey }: { serverId: string; volumeKey: string }) {
  const { t, i18n } = useLingui();
  const may = useMay();
  const allowed = may(DockerService.method.getDockerObject).allowed;
  const read = useQuery(
    DockerService.method.getDockerObject,
    { key: { serverId, engineId: volumeKey, kind: DockerObjectKind.VOLUME } },
    { enabled: allowed, retry: false },
  );
  const row = read.data?.object;
  const replacement = read.data?.replacementKey;
  const measurement = useVolumeMeasurements(row ? [row] : []);
  const measured = row ? measurement(row) : undefined;
  const volume = row?.detail.case === "volume" ? row.detail.value : undefined;
  const path = volume?.driver === "local" ? volume.mountpoint : "";
  const canList = may(FileService.method.listFiles).allowed && askable(serverId, path);
  const files = useQuery(
    FileService.method.listFiles,
    { serverId, path, lookup: true },
    { enabled: canList, retry: false, refetchOnWindowFocus: false },
  );
  const managedId = row?.managedResourceId ?? "";
  const managed = useQuery(VolumeService.method.getVolume, { id: managedId }, { enabled: !!managedId, retry: false });
  const current = managed.data?.volume;
  const refresh = () => {
    void read.refetch();
    if (canList) void files.refetch();
    if (managedId) void managed.refetch();
  };
  const watch = useDockerWatch(allowed ? [serverId] : [], [], refresh, DockerObjectKind.VOLUME);
  const [taking, setTaking] = useState(false);
  const [removing, setRemoving] = useState(false);
  const mayPrune = may(DockerService.method.planDockerMaintenance).allowed;
  const removableVolume = !!(
    mayPrune &&
    row &&
    volume &&
    !managedId &&
    row.ownership === DockerOwnership.EXTERNAL &&
    volume.driver === "local" &&
    volume.attachmentCount === 0 &&
    !volume.attachmentsPartial
  );
  const name = row?.managedResource?.name ?? "";
  const environment = row?.managedResource?.scope ?? "";
  const verified = !!(
    current &&
    volume?.createdAt &&
    current.revision === row?.managedRevision &&
    current.servers.some((server) => server.serverId === serverId)
  );
  const filePath = canList && !files.error ? files.data?.path : undefined;
  const filesPending = canList && files.isPending;
  const placementPending = !!managedId && managed.isPending;
  const backup = () => {
    void read.refetch().then(({ data, error }) => {
      if (
        !error &&
        data?.object?.managedResourceId === managedId &&
        data.object.managedRevision === current?.revision
      ) {
        setTaking(true);
      }
    });
  };
  const actions =
    row && volume ? (
      <div className="docker-storage-actions">
        {filesPending ? (
          <Loading label={t`Checking Files access`} slow={t`The server is slow to check Files access`}>
            <Skeleton shape="block" width={96} height={32} />
          </Loading>
        ) : filePath ? (
          <RouterButtonLink
            size="sm"
            variant="secondary"
            icon="folder"
            to="/servers/$serverId/files"
            params={{ serverId }}
            search={{ path: filePath }}
          >{t`Open files`}</RouterButtonLink>
        ) : (
          <Button
            size="sm"
            variant="secondary"
            icon="folder"
            disabled
            title={!canList || files.error ? t`Files can't open this mountpoint` : undefined}
          >{t`Open files`}</Button>
        )}
        {removableVolume ? (
          <Button size="sm" variant="secondary" icon="trash" onClick={() => setRemoving(true)}>
            {t`Remove volume`}
          </Button>
        ) : null}
        {may(BackupService.method.createBackup, { environment }).allowed ? (
          <Button
            size="sm"
            variant="primary"
            loading={placementPending || (verified && read.isFetching)}
            icon="archive"
            disabled={!verified || read.isFetching}
            title={!managedId ? t`Backups are available for managed volumes` : undefined}
            onClick={backup}
          >{t`Back up`}</Button>
        ) : null}
      </div>
    ) : undefined;
  return (
    <StoragePage serverId={serverId} row={row} actions={actions} pending={allowed && read.isPending} volumes>
      <LiveUpdates paused={watch.paused} retry={watch.retry} />
      {!allowed ? (
        <EmptyState icon="lock" title={t`Docker inventory needs an admin`} />
      ) : read.isPending ? (
        <StorageDetailLoading volumes />
      ) : read.error ? (
        <Callout
          tone="warning"
          title={t`Volume couldn't be read`}
          actions={<Button size="sm" onClick={refresh}>{t`Retry`}</Button>}
        >
          {sentence(read.error)}
        </Callout>
      ) : !row || !volume ? (
        <EmptyState
          icon="archive"
          title={replacement ? t`This volume was replaced` : t`This volume is gone`}
          action={
            replacement ? (
              <RouterButtonLink
                to="/servers/$serverId/docker/volumes/$volumeKey"
                params={{ serverId, volumeKey: replacement.engineId }}
              >{t`Open current volume`}</RouterButtonLink>
            ) : undefined
          }
        />
      ) : (
        <>
          <StorageAttachments
            serverId={serverId}
            title={t`Mounted by`}
            empty={t`No containers mount this volume`}
            partial={volume.attachmentsPartial}
            rows={volume.attachments.map((mount) => ({
              ...mount,
              id: JSON.stringify([mount.containerId, mount.mountTarget]),
            }))}
            volumes
          />
          {placementPending ? (
            <Loading label={t`Loading desired mounts`} slow={t`The server is slow to list desired mounts`}>
              <SkeletonCard title={t`Desired mounts`} flush>
                <SkeletonRows rows={2} lines={1} dense />
              </SkeletonCard>
            </Loading>
          ) : current?.mountedBy.length ? (
            <Panel title={t`Desired mounts`}>
              <ul className="docker-storage-attachments">
                {current.mountedBy.map((mount) => (
                  <li key={JSON.stringify([mount.serviceId, mount.target])}>
                    <Link className="dw-link" to="/apps/$serviceId" params={{ serviceId: mount.serviceId }}>
                      {mount.service}
                    </Link>
                    <code>{mount.target}</code>
                  </li>
                ))}
              </ul>
            </Panel>
          ) : null}
          <Panel title={t`Volume details`}>
            {measured === "pending" ? <SlowLine slow={t`Docker is measuring disk use on this server`} /> : null}
            <dl className="docker-facts">
              <Fact label={t`Driver`} value={volume.driver} mono />
              <Fact
                label={t`Size`}
                value={
                  measured === "pending" ? (
                    <MeasuringSize />
                  ) : measured ? (
                    formatBytes(measured.bytes, i18n.locale)
                  ) : (
                    t`Size unknown`
                  )
                }
              />
              {measured && measured !== "pending" ? (
                <Fact label={t`Measured`} value={formatMeasurement(measured.time.seconds, i18n.locale)} />
              ) : null}
              {volume.createdAt ? (
                <Fact label={t`Created`} value={formatMeasurement(volume.createdAt.seconds, i18n.locale)} />
              ) : null}
              {volume.labelKeys.length ? <Fact label={t`Labels`} value={volume.labelKeys.join(", ")} mono /> : null}
              {volume.optionKeys.length ? (
                <Fact label={t`Driver options`} value={volume.optionKeys.join(", ")} mono />
              ) : null}
              <Fact label={t`Mountpoint`} value={volume.mountpoint || t`Unavailable`} mono />
              {volume.anonymous ? <Fact label={t`Type`} value={t`Anonymous`} /> : null}
            </dl>
            <div className="docker-storage-actions">
              {!canList || files.error ? (
                <span className="docker-storage-muted">{t`Files can't open this mountpoint`}</span>
              ) : null}
              {current ? (
                <RouterButtonLink
                  size="sm"
                  variant="ghost"
                  to="/volumes/$volumeId"
                  params={{ volumeId: managedId }}
                  hash="volume-backups"
                >{t`Backups and schedules`}</RouterButtonLink>
              ) : null}
              {!volume.createdAt ? (
                <span className="docker-storage-muted">{t`Docker didn't report when this volume was created`}</span>
              ) : null}
            </div>
          </Panel>
          {taking && verified ? (
            <VolumeBackup
              volumeId={managedId}
              name={name}
              environment={environment}
              serverId={serverId}
              onClose={() => setTaking(false)}
            />
          ) : null}
          {removing ? (
            <DockerPruneSheet
              kind={DockerCleanupKind.VOLUME}
              servers={[{ id: serverId, name: serverId }]}
              focusEngineId={volumeKey}
              onClose={() => setRemoving(false)}
              onApplied={refresh}
            />
          ) : null}
        </>
      )}
    </StoragePage>
  );
}

function VolumeBackup({
  volumeId,
  name,
  environment,
  serverId,
  onClose,
}: {
  volumeId: string;
  name: string;
  environment: string;
  serverId: string;
  onClose: () => void;
}) {
  const destinations = useDestinations().data?.destinations ?? [];
  return (
    <BackUpSheet
      volumes={[{ id: volumeId, name, environment }]}
      chosen={volumeId}
      serverId={serverId}
      destinations={destinations}
      onClose={onClose}
    />
  );
}
