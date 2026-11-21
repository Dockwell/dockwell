// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate } from "@bufbuild/protobuf/wkt";
import { DockerCleanupKind, type ServerDiskUse } from "@dockwell/api";
import { Icon } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link } from "@tanstack/react-router";
import { formatBytes, formatDateTime } from "../../lib/format";
import { useDockerArtifactWords } from "./artifactWords";
import "./DockerArtifacts.css";

export function DockerDiskTable({ used, serverId }: { used: ServerDiskUse; serverId: string }) {
  const { t, i18n } = useLingui();
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
                <span className="docker-disk-quantity">
                  {row.reclaimableBytes === undefined ? t`Unknown` : formatBytes(row.reclaimableBytes, i18n.locale)}
                </span>
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
  const group = kind === DockerCleanupKind.VOLUME ? "volumes" : kind === DockerCleanupKind.LOG ? "logs" : "cache";
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
