// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate } from "@bufbuild/protobuf/wkt";
import { DockerCleanupKind, DockerService, type ServerDiskUse } from "@dockwell/api";
import { Button, Icon } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link } from "@tanstack/react-router";
import { useState } from "react";
import { formatBytes, formatDateTime } from "../../lib/format";
import { useMay } from "../auth/may";
import { useDockerArtifactWords } from "./artifactWords";
import { DockerPruneSheet } from "./DockerPruneSheet";
import "./DockerArtifacts.css";

export function DockerDiskTable({
  used,
  serverId,
  serverName,
}: {
  used: ServerDiskUse;
  serverId: string;
  serverName?: string | undefined;
}) {
  const { t, i18n } = useLingui();
  const mayPrune = useMay()(DockerService.method.planDockerMaintenance).allowed;
  const [pruningCache, setPruningCache] = useState(false);
  return (
    <>
      <table className="docker-disk-table" aria-label={t`Docker disk usage`}>
        <thead>
          <tr>
            <th scope="col">
              <Trans>Category</Trans>
            </th>
            <th scope="col">
              <Trans>Used</Trans>
            </th>
            <th scope="col">
              <Trans>Reclaimable</Trans>
            </th>
          </tr>
        </thead>
        <tbody>
          {used.dockerBreakdown.map((row) => (
            <tr key={row.kind}>
              <th scope="row">
                <DiskCategoryLink kind={row.kind} serverId={serverId} />
              </th>
              <td>
                <span className="docker-disk-quantity">
                  {row.usedBytes === undefined ? t`Unknown` : formatBytes(row.usedBytes, i18n.locale)}
                </span>
                {row.otherMountsBytes || row.partial ? (
                  <div className="docker-cleanup-meta">
                    {row.otherMountsBytes
                      ? t`${formatBytes(row.otherMountsBytes, i18n.locale)} on other disks`
                      : t`Partial count`}
                  </div>
                ) : null}
              </td>
              <td>
                <div className="docker-disk-reclaimable">
                  <span className="docker-disk-quantity">
                    {row.reclaimableBytes === undefined ? t`Unknown` : formatBytes(row.reclaimableBytes, i18n.locale)}
                  </span>
                  {row.kind === DockerCleanupKind.BUILD_CACHE && mayPrune ? (
                    <Button
                      size="sm"
                      variant="ghost"
                      icon="trash"
                      onClick={() => setPruningCache(true)}
                      aria-label={t`Remove unused build cache`}
                    >
                      <Trans>Remove unused</Trans>
                    </Button>
                  ) : null}
                </div>
              </td>
            </tr>
          ))}
        </tbody>
      </table>
      {used.time ? (
        <p className="docker-disk-measured">
          <Trans>Measured {formatDateTime(timestampDate(used.time), i18n.locale)}</Trans>
        </p>
      ) : null}
      {pruningCache ? (
        <DockerPruneSheet
          kind={DockerCleanupKind.BUILD_CACHE}
          servers={[{ id: serverId, name: serverName ?? serverId }]}
          onClose={() => setPruningCache(false)}
          onReviewAgain={() => setPruningCache(true)}
        />
      ) : null}
    </>
  );
}

function DiskCategoryLink({ kind, serverId }: { kind: DockerCleanupKind; serverId: string }) {
  const words = useDockerArtifactWords();
  const content = (
    <>
      <Icon name={words.icon(kind)} size={14} /> {words.kind(kind)}
    </>
  );
  if (kind === DockerCleanupKind.IMAGE) {
    return (
      <Link
        className="dw-link docker-row-link docker-disk-category"
        to="/servers/$serverId/docker/images"
        params={{ serverId }}
      >
        {content}
      </Link>
    );
  }
  if (kind === DockerCleanupKind.CONTAINER) {
    return (
      <Link
        className="dw-link docker-row-link docker-disk-category"
        to="/servers/$serverId/docker/containers"
        params={{ serverId }}
      >
        {content}
      </Link>
    );
  }
  if (kind === DockerCleanupKind.VOLUME) {
    return (
      <Link
        className="dw-link docker-row-link docker-disk-category"
        to="/servers/$serverId/docker/volumes"
        params={{ serverId }}
      >
        {content}
      </Link>
    );
  }
  const group = kind === DockerCleanupKind.LOG ? "logs" : "cache";
  return (
    <Link
      className="dw-link docker-row-link docker-disk-category"
      to="/servers/$serverId/docker/cleanup"
      params={{ serverId }}
      search={{ group }}
    >
      {content}
    </Link>
  );
}
