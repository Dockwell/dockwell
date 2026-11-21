// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate } from "@bufbuild/protobuf/wkt";
import { useQuery } from "@connectrpc/connect-query";
import { type DockerImage, DockerImageState, DockerObjectKind, DockerService, type Server } from "@dockwell/api";
import { Button, Callout, Icon, Panel, StatusBadge, TextField } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link } from "@tanstack/react-router";
import { useEffect, useState } from "react";

import { reasonOf, sentence } from "../../lib/errors";
import { formatBytes, formatDateTime } from "../../lib/format";
import { RouterButtonLink } from "../../lib/links";
import { useMay } from "../auth/may";
import { CopyButton } from "../servers/CopyButton";
import { ServerFrame } from "../servers/ServerFrame";
import { EmptyState } from "../shell/EmptyState";
import { useDockerWatch } from "./live";
import { Fact, LiveUpdates, MeasuringSize, Unread } from "./parts";
import "./DockerArtifacts.css";

function shortImageId(id: string): string {
  return id.replace(/^sha256:/, "").slice(0, 12);
}

function ImageStatus({ state }: { state: DockerImageState }) {
  const { t } = useLingui();
  switch (state) {
    case DockerImageState.USED:
      return <StatusBadge status="running" label={t`Used`} labelIsState />;
    case DockerImageState.UNUSED:
      return <StatusBadge status="stopped" label={t`Unused`} labelIsState />;
    case DockerImageState.DANGLING:
      return <StatusBadge status="degraded" label={t`Dangling`} labelIsState />;
    default:
      return <StatusBadge status="unknown" label={t`Unknown`} labelIsState />;
  }
}

export function DockerImages({ servers, fleet }: { servers: readonly Pick<Server, "id" | "name">[]; fleet: boolean }) {
  const { t, i18n } = useLingui();
  const [input, setInput] = useState("");
  const [search, setSearch] = useState("");
  const [pages, setPages] = useState([""]);
  const pageToken = pages.at(-1) ?? "";
  useEffect(() => {
    const value = input.trim();
    if (value === search) return;
    const timer = setTimeout(() => {
      setSearch(value);
      setPages([""]);
    }, 200);
    return () => clearTimeout(timer);
  }, [input, search]);
  const serverIds = servers.map((s) => s.id);
  const query = useQuery(
    DockerService.method.listDockerObjects,
    { serverIds, kind: DockerObjectKind.IMAGE, filter: { search }, pageToken },
    { retry: false, refetchOnWindowFocus: false },
  );
  const refresh = () => {
    if (pageToken) setPages([""]);
    else void query.refetch();
  };
  const watch = useDockerWatch(serverIds, [], () => void query.refetch(), false, DockerObjectKind.IMAGE);
  const rows = query.data?.objects ?? [];
  const next = query.data?.nextPageToken;
  return (
    <>
      <div className="docker-section-heading">
        <h2>
          <Trans>Local images</Trans>
        </h2>
        <Button variant="ghost" size="sm" icon="refresh" onClick={refresh}>
          <Trans>Refresh</Trans>
        </Button>
      </div>
      <div className="docker-filters">
        <TextField
          label={t`Search images`}
          placeholder={t`Tag, digest or ID`}
          value={input}
          maxLength={255}
          onChange={(event) => setInput(event.target.value)}
        />
      </div>
      <LiveUpdates paused={watch.paused} retry={watch.retry} />
      <Unread values={query.data?.unavailable ?? []} servers={servers} retry={refresh} />
      {query.error ? (
        <Callout tone="warning" title={t`Images couldn't be read`}>
          {reasonOf(query.error) === "docker_resync"
            ? t`The image list changed. Refresh it to continue.`
            : sentence(query.error)}
        </Callout>
      ) : null}
      {query.isPending ? <ImageLoading /> : null}
      {rows.length === 0 && query.isSuccess && !next && !query.data.unavailable.length ? (
        <EmptyState icon="archive" title={search ? t`No matching images` : t`No local images`} />
      ) : null}
      {rows.length ? (
        <ul className="docker-artifact-list" aria-label={t`Local images`}>
          {rows.map((row) => {
            if (!row.key || row.detail.case !== "image") return null;
            const { serverId, engineId } = row.key;
            const image = row.detail.value;
            return (
              <li className="docker-image-row" key={`${serverId}/${engineId}`}>
                <div className="docker-artifact-name">
                  <Icon name="archive" />
                  <div>
                    <Link
                      className="dw-link docker-row-link"
                      to="/servers/$serverId/docker/images/$imageId"
                      params={{ serverId, imageId: engineId }}
                    >
                      <code>{image.tags[0] ?? shortImageId(engineId)}</code>
                    </Link>
                    {fleet ? (
                      <div className="docker-muted">{servers.find((s) => s.id === serverId)?.name ?? serverId}</div>
                    ) : null}
                    {image.tags.length > 1 ? (
                      <details className="docker-aliases">
                        <summary>
                          <Trans>More tags</Trans>
                        </summary>
                        <ul className="docker-detail-list">
                          {image.tags.slice(1).map((tag) => (
                            <li key={tag}>
                              <code>{tag}</code>
                            </li>
                          ))}
                        </ul>
                      </details>
                    ) : null}
                  </div>
                </div>
                {image.state === DockerImageState.UNSPECIFIED && !row.observedAt ? (
                  <span className="docker-pending-size" aria-hidden="true" />
                ) : (
                  <ImageStatus state={image.state} />
                )}
                <dl className="docker-image-data">
                  <Fact label={t`Platform`} value={image.platform || t`Unknown`} mono />
                  <Fact label={t`Total`} value={<ImageBytes bytes={image.totalBytes} pending={!row?.observedAt} />} />
                  <Fact label={t`Unique`} value={<ImageBytes bytes={image.uniqueBytes} pending={!row?.observedAt} />} />
                  <Fact
                    label={t`Created`}
                    value={image.createdAt ? formatDateTime(timestampDate(image.createdAt), i18n.locale) : t`Unknown`}
                  />
                </dl>
              </li>
            );
          })}
        </ul>
      ) : null}
      {next || pageToken ? (
        <div className="docker-pager">
          <Button variant="secondary" disabled={!pageToken} onClick={() => setPages((old) => old.slice(0, -1))}>
            <Trans>Previous</Trans>
          </Button>
          <Button variant="secondary" disabled={!next} onClick={() => next && setPages((old) => [...old, next])}>
            <Trans>Next</Trans>
          </Button>
        </div>
      ) : null}
    </>
  );
}

function ImageBytes({ bytes, pending }: { bytes: bigint | undefined; pending: boolean }) {
  const { t, i18n } = useLingui();
  if (bytes !== undefined) return formatBytes(bytes, i18n.locale);
  if (!pending) return t`Unknown`;
  return <MeasuringSize />;
}

function ImageLoading() {
  const { t } = useLingui();
  return (
    <div role="status" aria-label={t`Reading local images`}>
      <div className="docker-artifact-list" aria-hidden="true">
        {["first", "second", "third"].map((row) => (
          <div className="docker-image-row docker-image-loading" key={row}>
            <span className="docker-pending-size" />
            <span className="docker-pending-size" />
            <div className="docker-image-data">
              <span className="docker-pending-size" />
              <span className="docker-pending-size" />
              <span className="docker-pending-size" />
            </div>
          </div>
        ))}
      </div>
    </div>
  );
}

export function DockerImageDetail({ serverId, imageId }: { serverId: string; imageId: string }) {
  const { t, i18n } = useLingui();
  const allowed = useMay()(DockerService.method.getDockerObject).allowed;
  const read = useQuery(
    DockerService.method.getDockerObject,
    { key: { serverId, engineId: imageId, kind: DockerObjectKind.IMAGE } },
    { enabled: allowed, retry: false },
  );
  const watch = useDockerWatch(allowed ? [serverId] : [], [], () => void read.refetch(), false, DockerObjectKind.IMAGE);
  const row = read.data?.object;
  const image = row?.detail.case === "image" ? row.detail.value : undefined;
  return (
    <ServerFrame id={serverId} phoneHead={{ phoneTitle: t`Image` }}>
      {(server) => (
        <section className="docker-detail-body">
          <div className="docker-section-heading docker-artifact-heading">
            <h2>
              <code>{image?.tags[0] ?? shortImageId(imageId)}</code>
            </h2>
            <RouterButtonLink
              to="/servers/$serverId/docker/images"
              params={{ serverId: server.id }}
              variant="secondary"
              size="sm"
            >
              <Trans>Local images</Trans>
            </RouterButtonLink>
          </div>
          <LiveUpdates paused={watch.paused} retry={watch.retry} />
          {!allowed ? <EmptyState icon="lock" title={t`Docker inventory needs an admin`} /> : null}
          {allowed && read.isPending ? (
            <p role="status">
              <Trans>Reading image</Trans>
            </p>
          ) : null}
          {read.error ? (
            <Callout title={t`Image couldn't be read`} tone="warning">
              {sentence(read.error)}
            </Callout>
          ) : null}
          {read.isSuccess && !image ? <EmptyState icon="archive" title={t`Image is gone`} /> : null}
          {image ? (
            <>
              <Panel title={t`Image details`} actions={<ImageStatus state={image.state} />}>
                <dl className="docker-facts">
                  <Fact
                    label={t`Image ID`}
                    value={
                      <span className="docker-image-identity">
                        <code>{shortImageId(imageId)}</code>
                        <CopyButton text={imageId} label={t`Copy full image ID`} />
                      </span>
                    }
                  />
                  <Fact label={t`Platform`} value={image.platform || t`Unknown`} mono />
                  <Fact
                    label={t`Total size`}
                    value={<ImageBytes bytes={image.totalBytes} pending={!row?.observedAt} />}
                  />
                  <Fact
                    label={t`Unique size`}
                    value={<ImageBytes bytes={image.uniqueBytes} pending={!row?.observedAt} />}
                  />
                  <Fact
                    label={t`Created`}
                    value={image.createdAt ? formatDateTime(timestampDate(image.createdAt), i18n.locale) : t`Unknown`}
                  />
                </dl>
                {image.tags.length || image.digests.length ? <ImageAliases image={image} /> : null}
              </Panel>
              <Panel title={t`Used by`}>
                {image.containers.length || image.deployments.length ? (
                  <ul className="docker-servers">
                    {image.containers.map((container) => (
                      <li key={container.key?.engineId ?? container.name}>
                        <Icon name="service" />
                        <Link
                          className="dw-link docker-row-link"
                          to="/servers/$serverId/docker/containers/$containerId"
                          params={{ serverId, containerId: container.key?.engineId ?? "" }}
                        >
                          <code>{container.name}</code>
                        </Link>
                      </li>
                    ))}
                    {image.deployments.map((deployment) => (
                      <li key={`${deployment.serviceId}/${deployment.revision}`}>
                        <Icon name="undo" />
                        <Link
                          className="dw-link docker-row-link"
                          to="/apps/$serviceId/deployments"
                          params={{ serviceId: deployment.serviceId }}
                        >
                          {deployment.serviceName}
                        </Link>
                        <span className="docker-muted">
                          <Trans>Retained revision {String(deployment.revision)}</Trans>
                        </span>
                      </li>
                    ))}
                  </ul>
                ) : (
                  <span className="docker-muted">
                    <Trans>No containers or retained deployments</Trans>
                  </span>
                )}
              </Panel>
              <Panel title={t`Layers`}>
                {image.layers.length ? (
                  <ol className="docker-layer-list">
                    {image.layers.map((layer, index) => (
                      // biome-ignore lint/suspicious/noArrayIndexKey: The image history is immutable and can share layer IDs.
                      <li key={`${layer.diffId}/${index}`}>
                        <code>{layer.diffId}</code>
                        {layer.sizeBytes === undefined ? null : (
                          <span className="docker-cleanup-meta">{formatBytes(layer.sizeBytes, i18n.locale)}</span>
                        )}
                      </li>
                    ))}
                  </ol>
                ) : (
                  <span className="docker-muted">
                    <Trans>No layers reported</Trans>
                  </span>
                )}
              </Panel>
              <Panel title={t`History`} actions={<Icon name="lock" aria-label={t`Instructions redacted`} />}>
                {image.historyUnavailable ? (
                  <span className="docker-muted">
                    <Trans>History unavailable</Trans>
                  </span>
                ) : (
                  <>
                    <span className="docker-muted">
                      <Trans>Build instructions are redacted</Trans>
                    </span>
                    <ol className="docker-history-list">
                      {image.history.map((entry, index) => (
                        // biome-ignore lint/suspicious/noArrayIndexKey: Redacted history entries can have identical timestamps and sizes.
                        <li key={index}>
                          <span>
                            {entry.createdAt
                              ? formatDateTime(timestampDate(entry.createdAt), i18n.locale)
                              : t`Unknown date`}
                          </span>
                          <code>
                            {entry.sizeBytes === undefined
                              ? t`Unknown size`
                              : formatBytes(entry.sizeBytes, i18n.locale)}
                          </code>
                        </li>
                      ))}
                    </ol>
                  </>
                )}
              </Panel>
              {image.detailsTruncated ? (
                <p className="docker-muted">
                  <Trans>Only the first entries are shown</Trans>
                </p>
              ) : null}
            </>
          ) : null}
        </section>
      )}
    </ServerFrame>
  );
}

function ImageAliases({ image }: { image: DockerImage }) {
  return (
    <details className="docker-aliases">
      <summary>
        <Trans>Tags and digests</Trans>
      </summary>
      <ul className="docker-detail-list">
        {[...image.tags, ...image.digests].map((alias) => (
          <li key={alias}>
            <code>{alias}</code>
          </li>
        ))}
      </ul>
    </details>
  );
}
