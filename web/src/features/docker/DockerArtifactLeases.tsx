// SPDX-License-Identifier: AGPL-3.0-only

import { Code, ConnectError, createClient } from "@connectrpc/connect";
import { useQuery, useTransport } from "@connectrpc/connect-query";
import {
  DockerArtifactKind,
  DockerArtifactState,
  DockerService,
  downloadDockerArtifactURL,
  type Server,
} from "@dockwell/api";
import { Button, ButtonLink, Callout, Icon, Loading, SelectField, SkeletonFacts, StatusBadge } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { useState } from "react";

import { sentence } from "../../lib/errors";
import { formatBytes } from "../../lib/format";

export function DockerArtifactLeases({
  servers,
  resume,
}: {
  servers: readonly Pick<Server, "id" | "name">[];
  resume: (serverId: string, artifactId: string, kind: DockerArtifactKind) => void;
}) {
  const { i18n, t } = useLingui();
  const [server, setServer] = useState("");
  const serverId = servers.some(({ id }) => id === server) ? server : (servers[0]?.id ?? "");
  const client = createClient(DockerService, useTransport());
  const [aborting, setAborting] = useState("");
  const [failure, setFailure] = useState("");
  const list = useQuery(
    DockerService.method.listDockerArtifacts,
    { serverId },
    {
      enabled: !!serverId,
      refetchInterval: 3000,
      retry: false,
    },
  );
  const abort = async (artifactId: string) => {
    setFailure("");
    setAborting(artifactId);
    try {
      await client.abortDockerArtifact({ serverId, artifactId });
    } catch (error) {
      if (ConnectError.from(error).code !== Code.NotFound) setFailure(sentence(error));
    } finally {
      setAborting("");
      void list.refetch();
    }
  };
  if (!list.data?.artifacts.length && !list.isLoading && !list.isError && servers.length < 2) return null;
  return (
    <section className="docker-archive-leases" aria-label={t`Image transfers`}>
      <div className="docker-archive-lease-heading">
        <h3>
          <Trans>Image transfers</Trans>
        </h3>
        {servers.length > 1 ? (
          <SelectField
            label={t`Server`}
            value={serverId}
            options={servers.map(({ id, name }) => ({ value: id, label: name }))}
            onChange={(event) => setServer(event.target.value)}
          />
        ) : null}
      </div>
      {failure ? (
        <Callout tone="warning" title={t`Transfer couldn't be aborted`}>
          {failure}
        </Callout>
      ) : null}
      {list.isLoading ? (
        <Loading label={t`Reading image transfers`}>
          <SkeletonFacts layout="rows" count={2} />
        </Loading>
      ) : list.isError ? (
        <Button variant="ghost" icon="refresh" onClick={() => void list.refetch()}>
          <Trans>Retry transfers</Trans>
        </Button>
      ) : list.data?.artifacts.length ? (
        <ul>
          {list.data.artifacts.map((item) => {
            const incoming = item.kind !== DockerArtifactKind.IMAGE_EXPORT;
            const label =
              item.kind === DockerArtifactKind.BUILD_CONTEXT
                ? t`Build context`
                : item.kind === DockerArtifactKind.DOCKERFILE
                  ? t`Dockerfile`
                  : incoming
                    ? t`Image archive`
                    : t`Image export`;
            const ready = item.state === DockerArtifactState.READY;
            const failed = item.state === DockerArtifactState.FAILED;
            const checking = item.state === DockerArtifactState.CHECKING;
            const state = ready
              ? t`Ready`
              : failed
                ? t`Check failed`
                : checking
                  ? incoming
                    ? t`Checking`
                    : t`Awaiting export`
                  : t`Awaiting upload`;
            return (
              <li key={item.artifactId}>
                <div className="docker-archive-lease-name">
                  <Icon name="archive" size={18} />
                  <div>
                    <strong>{item.fileName || label}</strong>
                    <small>
                      {incoming && item.receivedBytes < item.sizeBytes
                        ? t`${formatBytes(item.receivedBytes, i18n.locale)} of ${formatBytes(item.sizeBytes, i18n.locale)}`
                        : item.sizeBytes > 0n
                          ? formatBytes(item.sizeBytes, i18n.locale)
                          : null}
                    </small>
                  </div>
                </div>
                <StatusBadge
                  status={ready ? "healthy" : failed ? "degraded" : checking ? "deploying" : "stopped"}
                  label={state}
                  labelIsState
                />
                <div className="docker-archive-lease-actions">
                  {incoming && !failed && item.expiresAt ? (
                    <Button variant="secondary" size="sm" onClick={() => resume(serverId, item.artifactId, item.kind)}>
                      {ready ? <Trans>Review</Trans> : checking ? <Trans>View</Trans> : <Trans>Resume</Trans>}
                    </Button>
                  ) : !incoming && ready ? (
                    <ButtonLink
                      variant="secondary"
                      size="sm"
                      icon="download"
                      href={downloadDockerArtifactURL({ serverId, artifactId: item.artifactId })}
                    >
                      <Trans>Download</Trans>
                    </ButtonLink>
                  ) : null}
                  <Button
                    variant="ghost"
                    size="sm"
                    loading={aborting === item.artifactId}
                    disabled={!!aborting}
                    onClick={() => void abort(item.artifactId)}
                  >
                    <Trans>Abort</Trans>
                  </Button>
                </div>
              </li>
            );
          })}
        </ul>
      ) : (
        <small>
          <Trans>No image transfers on this server.</Trans>
        </small>
      )}
    </section>
  );
}
