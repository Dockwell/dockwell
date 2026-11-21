// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate } from "@bufbuild/protobuf/wkt";
import { createClient } from "@connectrpc/connect";
import { createConnectQueryKey, useTransport } from "@connectrpc/connect-query";
import {
  DockerCleanupKind,
  type DockerCleanupPause,
  DockerCleanupReason,
  type DockerCleanupTarget,
  DockerReadState,
  DockerService,
  type Server,
  ServerService,
} from "@dockwell/api";
import { Button, Callout, Icon, Panel } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { useInfiniteQuery, useQueryClient } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { useState } from "react";
import { reasonOf, sentence } from "../../lib/errors";
import { formatBytes, formatDateTime } from "../../lib/format";
import { useMay } from "../auth/may";
import { useChangeSheet } from "../changes/ChangeSheet";
import { EmptyState } from "../shell/EmptyState";
import { useDockerArtifactWords } from "./artifactWords";
import { Unread } from "./parts";
import "./DockerArtifacts.css";

type ServerName = Pick<Server, "id" | "name">;

function keyOf(target: DockerCleanupTarget): string {
  return `${target.serverId}/${target.kind}/${target.engineId}`;
}

export function DockerCleanup({ servers, group }: { servers: readonly ServerName[]; group?: string | undefined }) {
  const { t } = useLingui();
  const [taggedImages, setTaggedImages] = useState(false);
  const [networks, setNetworks] = useState(group === "networks");
  return (
    <>
      <div className="docker-section-heading">
        <h2>
          <Trans>Review cleanup</Trans>
        </h2>
      </div>
      <details className="docker-cleanup-disclosure" open={group === "networks" || undefined}>
        <summary>
          <Trans>Cleanup options</Trans>
        </summary>
        <div className="docker-cleanup-filters">
          <label className="docker-cleanup-choice">
            <input type="checkbox" checked={taggedImages} onChange={(event) => setTaggedImages(event.target.checked)} />
            {t`Include unused images with tags or digests`}
          </label>
          <label className="docker-cleanup-choice">
            <input type="checkbox" checked={networks} onChange={(event) => setNetworks(event.target.checked)} />
            {t`Include unused networks`}
          </label>
        </div>
      </details>
      <CleanupSelection key={String(taggedImages) + String(networks)} {...{ servers, group, taggedImages, networks }} />
    </>
  );
}

function CleanupSelection({
  servers,
  group,
  taggedImages,
  networks,
}: {
  servers: readonly ServerName[];
  group?: string | undefined;
  taggedImages: boolean;
  networks: boolean;
}) {
  const { t, i18n } = useLingui();
  const words = useDockerArtifactWords();
  const transport = useTransport();
  const client = createClient(DockerService, transport);
  const sheet = useChangeSheet();
  const cache = useQueryClient();
  const may = useMay()(DockerService.method.planDockerMaintenance).allowed;
  const [overrides, setOverrides] = useState<ReadonlyMap<string, boolean>>(() => new Map());
  const serverIds = servers.map((s) => s.id);
  const filters = { taggedImages, networks };
  const query = useInfiniteQuery({
    queryKey: ["docker-cleanup", serverIds, filters],
    queryFn: ({ pageParam, signal }) =>
      client.previewDockerCleanup({ serverIds, filters, pageToken: pageParam }, { signal }),
    initialPageParam: "",
    getNextPageParam: (last) => last.nextPageToken || undefined,
    refetchOnWindowFocus: false,
    refetchInterval: ({ state }) =>
      state.data?.pages[0]?.unavailable.some((row) => row.state === DockerReadState.UNSPECIFIED) ? 2_000 : false,
    retry: false,
  });
  const pages = query.data?.pages ?? [];
  const pauses = pages[0]?.pauses ?? [];
  const unavailable = pages[0]?.unavailable ?? [];
  const measuring = unavailable.some((row) => row.state === DockerReadState.UNSPECIFIED);
  const candidates = pages.flatMap((page) => page.candidates);
  const protectedRows = pages.flatMap((page) => page.exclusions);
  const selected = candidates.filter((target, index) => overrides.get(keyOf(target)) ?? index < 12);
  const estimated = formatBytes(
    selected.reduce((sum, target) => sum + (target.estimatedBytes ?? 0n), 0n),
    i18n.locale,
  );
  const reviewAgain = () => {
    cache.setQueryData<typeof query.data>(["docker-cleanup", serverIds, filters], (data) =>
      data ? { pages: data.pages.slice(0, 1), pageParams: data.pageParams.slice(0, 1) } : data,
    );
    void query.refetch();
  };
  const review = () => {
    const expectedSnapshot = pages[0]?.snapshot;
    if (!expectedSnapshot) return;
    // Keep the reviewed selection when a regenerated preview drops an object.
    setOverrides(new Map(candidates.map((target) => [keyOf(target), selected.includes(target)])));
    sheet.open({
      kind: "plan",
      retryLabel: t`Review again`,
      make: () =>
        client.planDockerMaintenance({
          serverIds,
          expectedSnapshot,
          operation: { case: "cleanup", value: { filters, targets: selected } },
        }),
      onClose: () => {
        reviewAgain();
        void cache.invalidateQueries({
          queryKey: createConnectQueryKey({ schema: ServerService.method.getServerDiskUse, cardinality: undefined }),
        });
      },
    });
  };
  const reviewResume = (pause: DockerCleanupPause) => {
    sheet.open({
      kind: "plan",
      make: () =>
        client.planDockerMaintenance({
          serverIds: [pause.serverId],
          operation: { case: "resumeCleanupToken", value: pause.token },
        }),
      onClose: reviewAgain,
    });
  };
  const kinds = [...new Set(candidates.map((target) => target.kind))];
  const protectedKinds = [...new Set(protectedRows.map((target) => target.kind))];
  const showProtected = group === "volumes" || group === "logs" || group === "networks";
  return (
    <>
      <Unread
        values={unavailable.filter((row) => row.state !== DockerReadState.UNSPECIFIED)}
        servers={servers}
        retry={reviewAgain}
      />
      {pauses.map((pause) => {
        const server = servers.find((s) => s.id === pause.serverId)?.name ?? pause.serverId;
        return (
          <Callout
            key={pause.serverId}
            tone="warning"
            title={t`Cleanup paused on ${server}`}
            actions={
              <Button
                size="sm"
                variant="secondary"
                disabled={!may || query.isFetching}
                onClick={() => reviewResume(pause)}
              >
                <Trans>Re-enable cleanup</Trans>
              </Button>
            }
          >
            {words.receiptReason(pause.reason)}
          </Callout>
        );
      })}
      {query.isPending || measuring ? (
        <Panel title={t`Checking Docker artifacts`} actions={<Icon name="archive" />}>
          <div role="status" aria-label={t`Checking Docker artifacts`}>
            <span className="docker-pending-size" aria-hidden="true" />
          </div>
        </Panel>
      ) : null}
      {query.error ? (
        <Callout
          tone="warning"
          title={
            reasonOf(query.error) === "docker_cleanup_changed"
              ? t`Cleanup candidates changed`
              : t`Cleanup couldn't be read`
          }
          actions={
            <Button size="sm" variant="secondary" onClick={reviewAgain}>
              <Trans>Review again</Trans>
            </Button>
          }
        >
          {reasonOf(query.error) === "docker_cleanup_changed" ? null : sentence(query.error)}
        </Callout>
      ) : null}
      {!query.isPending &&
      !query.error &&
      candidates.length === 0 &&
      (pages[0]?.unavailable.length ?? 0) + pauses.length < servers.length ? (
        <EmptyState icon="archive" title={t`No eligible artifacts`} />
      ) : null}
      {kinds.map((kind) => (
        <Panel key={kind} title={words.kind(kind)} actions={<Icon name={words.icon(kind)} />}>
          <ul className="docker-cleanup-list">
            {candidates
              .filter((row) => row.kind === kind)
              .map((target) => {
                const checked = selected.includes(target);
                return (
                  <li className="docker-cleanup-item" key={keyOf(target)}>
                    <label className="docker-cleanup-choice">
                      <input
                        type="checkbox"
                        checked={checked}
                        disabled={!checked && selected.length >= 12}
                        onChange={(event) => {
                          const next = event.target.checked;
                          setOverrides(
                            new Map(
                              candidates.map((row) => [keyOf(row), row === target ? next : selected.includes(row)]),
                            ),
                          );
                        }}
                      />
                      <span>
                        <CleanupLabel target={target} servers={servers} />
                      </span>
                    </label>
                    {target.estimatedBytes === undefined ? null : (
                      <code>{formatBytes(target.estimatedBytes, i18n.locale)}</code>
                    )}
                  </li>
                );
              })}
          </ul>
        </Panel>
      ))}
      {protectedRows.length ? (
        <details className="docker-cleanup-disclosure" open={showProtected || undefined}>
          <summary>
            <span className="docker-disclosure-label">
              <Icon name="lock" /> <Trans>Protected artifacts</Trans>
            </span>
          </summary>
          {protectedKinds.map((kind) => (
            <Panel key={kind} title={words.kind(kind)}>
              <ul className="docker-cleanup-list">
                {protectedRows
                  .filter((target) => target.kind === kind)
                  .map((target) => (
                    <li className="docker-cleanup-item" key={keyOf(target)}>
                      <div>
                        <CleanupLabel target={target} servers={servers} />
                      </div>
                      {kind === DockerCleanupKind.CONTAINER ? (
                        <Link
                          className="dw-link docker-row-link"
                          to="/servers/$serverId/docker/containers/$containerId"
                          params={{ serverId: target.serverId, containerId: target.engineId }}
                          search={{ name: target.name }}
                        >
                          <Trans>Open container</Trans>
                        </Link>
                      ) : target.estimatedBytes !== undefined ? (
                        <code>{formatBytes(target.estimatedBytes, i18n.locale)}</code>
                      ) : null}
                    </li>
                  ))}
              </ul>
            </Panel>
          ))}
        </details>
      ) : null}
      {query.hasNextPage ? (
        <Button
          variant="secondary"
          size="sm"
          loading={query.isFetchingNextPage}
          onClick={() => void query.fetchNextPage()}
        >
          <Trans>Show more artifacts</Trans>
        </Button>
      ) : null}
      {candidates.length > 0 ? (
        <div className="docker-cleanup-footer">
          <span aria-live="polite">
            <Trans>
              {selected.length} selected · {estimated} estimated
            </Trans>
          </span>
          <Button
            icon="trash"
            variant="primary"
            disabled={
              !may || measuring || !selected.length || selected.length > 12 || query.isFetching || !!query.error
            }
            onClick={review}
          >
            <Trans>Review plan</Trans>
          </Button>
        </div>
      ) : null}
    </>
  );
}

function CleanupLabel({ target, servers }: { target: DockerCleanupTarget; servers: readonly ServerName[] }) {
  const { t, i18n } = useLingui();
  const words = useDockerArtifactWords();
  const lastUsed = target.lastUsedAt ? formatDateTime(timestampDate(target.lastUsedAt), i18n.locale) : undefined;
  const reason = words.reason(target.reason, target.kind);
  const eligible = target.evidence !== "";
  const lastUse = eligible && lastUsed ? t`Last used ${lastUsed}` : undefined;
  const name =
    target.kind === DockerCleanupKind.IMAGE && target.reason === DockerCleanupReason.DANGLING
      ? target.engineId.slice(7, 19)
      : target.name || target.engineId || words.kind(target.kind);
  const server = servers.length > 1 ? servers.find((s) => s.id === target.serverId)?.name : undefined;
  return (
    <>
      <code>{name}</code>
      {target.imageTags.length > 1 || target.imageDigests.length > 0 ? (
        <div className="docker-cleanup-meta">
          <code>{[...target.imageTags.slice(1), ...target.imageDigests].join(", ")}</code>
        </div>
      ) : null}
      <span className="docker-cleanup-meta">{[reason, server, lastUse].filter(Boolean).join(" · ")}</span>
    </>
  );
}
