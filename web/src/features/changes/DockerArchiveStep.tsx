// SPDX-License-Identifier: AGPL-3.0-only

import { useQuery } from "@connectrpc/connect-query";
import {
  DockerArtifactState,
  type DockerImageAction,
  DockerService,
  downloadDockerArtifactURL,
  type StepProgress,
} from "@dockwell/api";
import { ButtonLink, Loading, Skeleton, Tag } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";

import { formatBytes } from "../../lib/format";
import { useMay } from "../auth/may";
import { shortImageId } from "../docker/imageIdentity";
import { CopyButton } from "../servers/CopyButton";

export function DockerArchiveStep({
  action,
  server,
  live,
}: {
  action: DockerImageAction;
  server: string;
  live?: StepProgress | undefined;
}) {
  const { i18n, t } = useLingui();
  const receipt = live?.dockerImageReceipts.find((item) => item.artifactId);
  const serverId = receipt?.imageKey?.serverId ?? "";
  const artifactId = receipt?.artifactId ?? "";
  const mayDownload = useMay()(DockerService.method.getDockerArtifact).allowed;
  const lease = useQuery(
    DockerService.method.getDockerArtifact,
    { serverId, artifactId },
    {
      enabled: mayDownload && !!serverId && !!artifactId,
      retry: false,
    },
  );
  const imageId = action.export?.sourceKey?.imageId ?? "";
  return (
    <li className="plan-step plan-archive-step">
      {action.export ? (
        <>
          <div className="plan-archive-identity">
            <span>
              <Trans>Image</Trans>
            </span>{" "}
            <code title={imageId}>{shortImageId(imageId)}</code>
            <CopyButton text={imageId} label={t`Copy full image ID`} />
          </div>
          <div className="plan-archive-tags">
            {action.export.tags.length ? (
              action.export.tags.map((tag) => <Tag key={tag}>{tag}</Tag>)
            ) : (
              <small>
                <Trans>Image only</Trans>
              </small>
            )}
          </div>
        </>
      ) : (
        <>
          <Trans>
            Load the reviewed archive on <code>{server}</code>
          </Trans>
          <ul>
            {action.load?.selectedImageIds.map((image) => (
              <li key={image}>
                <code>{image}</code>
              </li>
            ))}
            {action.load?.tags.map(({ alias }) => (
              <li key={alias}>
                <code>{alias}</code>
              </li>
            ))}
          </ul>
        </>
      )}
      {receipt && serverId ? (
        <div className="plan-archive-download">
          {lease.isLoading ? (
            <Loading label={t`Checking download access`}>
              <Skeleton shape="pill" width={112} />
            </Loading>
          ) : mayDownload && lease.data?.state === DockerArtifactState.READY ? (
            <>
              <ButtonLink variant="primary" icon="download" href={downloadDockerArtifactURL({ serverId, artifactId })}>
                <Trans>Download</Trans>
              </ButtonLink>
              <small>{formatBytes(receipt.sizeBytes, i18n.locale)}</small>
            </>
          ) : (
            <small>
              <Trans>The archive is available to the session that planned it while its lease is active.</Trans>
            </small>
          )}
        </div>
      ) : null}
    </li>
  );
}
