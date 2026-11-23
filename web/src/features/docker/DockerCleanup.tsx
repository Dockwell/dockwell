// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate } from "@bufbuild/protobuf/wkt";
import {
  DockerCleanupKind,
  type DockerCleanupPause,
  DockerCleanupReason,
  type DockerCleanupTarget,
  DockerReadState,
  DockerService,
} from "@dockwell/api";
import { Button, Callout, Icon, Loading, Panel, Skeleton, SkeletonCard, SkeletonRows, Tag } from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link } from "@tanstack/react-router";
import { useMemo, useState } from "react";
import { reasonOf, sentence } from "../../lib/errors";
import { formatBytes, formatDateTime } from "../../lib/format";
import { useMay } from "../auth/may";
import { useChangeSheet } from "../changes/ChangeSheet";
import { CopyButton } from "../servers/CopyButton";
import { EmptyState } from "../shell/EmptyState";
import { useDockerArtifactWords } from "./artifactWords";
import {
  type CleanupFilterSpec,
  type CleanupServer,
  cleanupTargetDisplayName,
  cleanupTargetFullName,
  cleanupTargetKey,
  computeBoundedDefaults,
  isAnonymousVolumeTarget,
  isOptInNamedVolume,
  isOptInTaggedImage,
  resolveBoundedSelection,
  useCleanupPreview,
} from "./cleanupHelpers";
import { Unread } from "./parts";
import "./DockerArtifacts.css";

const ALL_CLEANUP_FILTERS: CleanupFilterSpec = {
  taggedImages: true,
  networks: true,
  volumes: true,
  kinds: [],
};

export function DockerCleanup({ servers, group }: { servers: readonly CleanupServer[]; group?: string | undefined }) {
  const { t, i18n } = useLingui();
  const words = useDockerArtifactWords();
  const sheet = useChangeSheet();
  const may = useMay()(DockerService.method.planDockerMaintenance).allowed;
  const [overrides, setOverrides] = useState<ReadonlyMap<string, boolean>>(() => new Map());

  const preview = useCleanupPreview(servers, ALL_CLEANUP_FILTERS, true);
  const { candidates, protectedRows, pauses, unavailable, measuring, loadingMore, hasAnyData } = preview;

  const bounded = useMemo(
    () => computeBoundedDefaults(candidates, preview.serverLimits, preview.maxTargetsTotal),
    [candidates, preview.serverLimits, preview.maxTargetsTotal],
  );
  const { selected, selectedKeys, deferredCount, canSelectTarget } = useMemo(
    () => resolveBoundedSelection(candidates, bounded, overrides),
    [candidates, bounded, overrides],
  );

  const isChecked = (target: DockerCleanupTarget) => selectedKeys.has(cleanupTargetKey(target));
  const optInTaggedImages = useMemo(() => candidates.filter(isOptInTaggedImage), [candidates]);
  const optInNamedVolumes = useMemo(() => candidates.filter(isOptInNamedVolume), [candidates]);

  const allTaggedSelected = optInTaggedImages.length > 0 && optInTaggedImages.every(isChecked);
  const allNamedVolumesSelected = optInNamedVolumes.length > 0 && optInNamedVolumes.every(isChecked);

  const setOne = (target: DockerCleanupTarget, checked: boolean) => {
    setOverrides((prev) => {
      const next = new Map(prev);
      next.set(cleanupTargetKey(target), checked);
      return next;
    });
  };

  const setGroup = (items: readonly DockerCleanupTarget[], checked: boolean) => {
    setOverrides((prev) => {
      const next = new Map(prev);
      for (const row of items) next.set(cleanupTargetKey(row), checked);
      return next;
    });
  };

  const estimatedBytes = selected.reduce((sum, target) => sum + (target.estimatedBytes ?? 0n), 0n);
  const estimated = formatBytes(estimatedBytes, i18n.locale);

  const initialLoading = preview.query.isPending || ((measuring || loadingMore) && !hasAnyData);
  const pendingMore = !initialLoading && (measuring || loadingMore);

  const review = () => {
    if (!preview.snapshot || selected.length === 0 || pendingMore) return;
    // Keep the reviewed selection when a regenerated preview drops an object.
    setOverrides(new Map(candidates.map((target) => [cleanupTargetKey(target), isChecked(target)])));
    sheet.open({
      kind: "plan",
      retryLabel: t`Review again`,
      make: () =>
        preview.client.planDockerMaintenance({
          serverIds: preview.serverIds,
          expectedSnapshot: preview.snapshot,
          operation: {
            case: "cleanup",
            value: {
              filters: {
                taggedImages: ALL_CLEANUP_FILTERS.taggedImages,
                networks: ALL_CLEANUP_FILTERS.networks,
                volumes: ALL_CLEANUP_FILTERS.volumes,
                kinds: [],
              },
              targets: selected,
            },
          },
        }),
      onClose: () => {
        preview.invalidateAfterCleanup();
      },
    });
  };

  const reviewResume = (pause: DockerCleanupPause) => {
    sheet.open({
      kind: "plan",
      make: () =>
        preview.client.planDockerMaintenance({
          serverIds: [pause.serverId],
          operation: { case: "resumeCleanupToken", value: pause.token },
        }),
      onClose: () => preview.invalidateAfterCleanup(),
    });
  };

  const kinds = [...new Set(candidates.map((target) => target.kind))];
  const protectedKinds = [...new Set(protectedRows.map((target) => target.kind))];
  const showProtected = group === "volumes" || group === "logs" || group === "networks";
  const slowLine =
    servers.length > 1
      ? t`Docker is measuring disk use on these servers`
      : t`Docker is measuring disk use on this server`;
  const unsupportedVolumeNames = bounded.unsupportedVolumeServers.map(
    (id) => servers.find((s) => s.id === id)?.name ?? id,
  );
  const stale = reasonOf(preview.query.error) === "docker_cleanup_changed";

  return (
    <>
      <div className="docker-section-heading docker-cleanup-heading">
        <div className="docker-cleanup-heading-top">
          <h2>
            <Trans>Cleanup</Trans>
          </h2>
          <div className="docker-heading-actions">
            <Button
              icon="refresh"
              size="sm"
              variant="ghost"
              onClick={preview.reviewAgain}
              loading={preview.query.isFetching && !preview.query.isPending}
              aria-label={t`Refresh cleanup`}
              title={t`Refresh cleanup`}
            />
          </div>
        </div>
        <p>
          <Trans>Dangling images, build cache, unused networks and anonymous volumes are selected by default.</Trans>
        </p>
      </div>

      {!initialLoading && (optInTaggedImages.length > 0 || optInNamedVolumes.length > 0) ? (
        <fieldset className="docker-cleanup-quick-options" aria-label={t`Additional cleanup options`}>
          {optInTaggedImages.length > 0 ? (
            <label className="docker-cleanup-choice">
              <input
                type="checkbox"
                checked={allTaggedSelected}
                onChange={(event) => setGroup(optInTaggedImages, event.target.checked)}
              />
              <span>
                {plural(optInTaggedImages.length, {
                  one: "Include unused tagged image (#)",
                  other: "Include unused tagged images (#)",
                })}
              </span>
            </label>
          ) : null}
          {optInNamedVolumes.length > 0 ? (
            <label className="docker-cleanup-choice">
              <input
                type="checkbox"
                checked={allNamedVolumesSelected}
                onChange={(event) => setGroup(optInNamedVolumes, event.target.checked)}
              />
              <span>
                {plural(optInNamedVolumes.length, {
                  one: "Include unused named volume (#)",
                  other: "Include unused named volumes (#)",
                })}
                <span className="docker-cleanup-meta">
                  <Trans>Data is deleted</Trans>
                </span>
              </span>
            </label>
          ) : null}
        </fieldset>
      ) : null}

      <Unread
        values={unavailable.filter((row) => row.state !== DockerReadState.UNSPECIFIED)}
        servers={servers}
        retry={preview.reviewAgain}
      />

      {!initialLoading && unsupportedVolumeNames.length > 0 ? (
        <Callout tone="info" title={t`Volume cleanup needs an agent update`}>
          {t`Update the agent on ${unsupportedVolumeNames.join(", ")} to remove unused Docker volumes.`}
        </Callout>
      ) : null}

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
                disabled={!may || preview.query.isFetching}
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

      {initialLoading && !preview.query.error ? (
        <Loading label={t`Checking Docker artifacts`} slow={slowLine}>
          <SkeletonCard title={<Skeleton text="heading" width={112} />} flush>
            <SkeletonRows rows={4} lines={1} end="value" dense />
          </SkeletonCard>
        </Loading>
      ) : null}

      {preview.query.error ? (
        <Callout
          tone="warning"
          title={stale ? t`Cleanup candidates changed` : t`Cleanup couldn't be read`}
          actions={
            <Button size="sm" variant="secondary" onClick={preview.reviewAgain}>
              {stale ? <Trans>Refresh</Trans> : <Trans>Review again</Trans>}
            </Button>
          }
        >
          {stale ? (
            <Trans>This preview is out of date and can't be reviewed until you refresh.</Trans>
          ) : (
            sentence(preview.query.error)
          )}
        </Callout>
      ) : null}

      {!initialLoading &&
      !pendingMore &&
      !preview.query.error &&
      candidates.length === 0 &&
      unavailable.length + pauses.length < servers.length ? (
        <EmptyState icon="archive" title={t`No eligible artifacts`} />
      ) : null}

      {!initialLoading &&
        kinds.map((kind) => {
          const rows = candidates.filter((row) => row.kind === kind);
          const allSelected = rows.length > 0 && rows.every(isChecked);
          return (
            <Panel
              key={kind}
              title={words.kind(kind)}
              actions={
                rows.length > 0 ? (
                  <Button size="sm" variant="ghost" onClick={() => setGroup(rows, !allSelected)}>
                    {allSelected ? t`Deselect all` : t`Select all`}
                  </Button>
                ) : undefined
              }
            >
              <ul className="docker-cleanup-list">
                {rows.map((target) => {
                  const checked = isChecked(target);
                  const namedVolume = isOptInNamedVolume(target);
                  const disabled = !checked && !canSelectTarget(target);
                  return (
                    <li className="docker-cleanup-item" key={cleanupTargetKey(target)}>
                      <label className="docker-cleanup-choice">
                        <input
                          type="checkbox"
                          checked={checked}
                          disabled={disabled}
                          onChange={(event) => setOne(target, event.target.checked)}
                        />
                        <span className="docker-cleanup-row-main">
                          <CleanupLabel target={target} servers={servers} namedVolume={namedVolume} />
                        </span>
                      </label>
                      <div className="docker-cleanup-item-end">
                        {target.estimatedBytes !== undefined ? (
                          <code>{formatBytes(target.estimatedBytes, i18n.locale)}</code>
                        ) : target.kind === DockerCleanupKind.VOLUME ? (
                          <span className="docker-cleanup-meta">{t`Size unknown`}</span>
                        ) : null}
                      </div>
                    </li>
                  );
                })}
              </ul>
            </Panel>
          );
        })}

      {pendingMore && !preview.query.error ? (
        <Loading label={t`Checking remaining Docker artifacts`} slow={slowLine}>
          <SkeletonCard title={<Skeleton text="heading" width={140} />} flush>
            <SkeletonRows rows={2} lines={1} end="value" dense />
          </SkeletonCard>
        </Loading>
      ) : null}

      {!initialLoading && protectedRows.length > 0 ? (
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
                    <li className="docker-cleanup-item" key={cleanupTargetKey(target)}>
                      <div className="docker-cleanup-row-main">
                        <CleanupLabel target={target} servers={servers} />
                      </div>
                      <div className="docker-cleanup-item-end">
                        {kind === DockerCleanupKind.CONTAINER ? (
                          <Link
                            className="dw-link docker-row-link"
                            to="/servers/$serverId/docker/containers/$containerId"
                            params={{ serverId: target.serverId, containerId: target.engineId }}
                            search={{ name: target.name }}
                          >
                            <Trans>Open container</Trans>
                          </Link>
                        ) : target.reason === DockerCleanupReason.COMPOSE && target.composeId ? (
                          <Link
                            className="dw-link docker-row-link"
                            to="/servers/$serverId/docker/composes/$composeId"
                            params={{ serverId: target.serverId, composeId: target.composeId }}
                          >
                            <Trans>Open Compose project</Trans>
                          </Link>
                        ) : target.estimatedBytes !== undefined ? (
                          <code>{formatBytes(target.estimatedBytes, i18n.locale)}</code>
                        ) : null}
                      </div>
                    </li>
                  ))}
              </ul>
            </Panel>
          ))}
        </details>
      ) : null}

      {!initialLoading && candidates.length > 0 ? (
        <div className="docker-cleanup-footer">
          <div className="docker-cleanup-footer-meta" aria-live="polite">
            <span>
              <Trans>
                {selected.length} selected · {estimated} estimated
              </Trans>
            </span>
            {deferredCount > 0 ? (
              <span className="docker-cleanup-meta">{t`${deferredCount} more after this cleanup`}</span>
            ) : null}
          </div>
          <Button
            icon="trash"
            variant="primary"
            disabled={
              !may ||
              initialLoading ||
              pendingMore ||
              selected.length === 0 ||
              preview.query.isFetching ||
              !!preview.query.error
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

function CleanupLabel({
  target,
  servers,
  namedVolume = false,
}: {
  target: DockerCleanupTarget;
  servers: readonly CleanupServer[];
  namedVolume?: boolean;
}) {
  const { t, i18n } = useLingui();
  const words = useDockerArtifactWords();
  const lastUsed = target.lastUsedAt ? formatDateTime(timestampDate(target.lastUsedAt), i18n.locale) : undefined;
  const reason = isOptInNamedVolume(target)
    ? t`Unused named volume`
    : words.reason(target.reason, target.kind, target.composeProject);
  const eligible = target.evidence !== "";
  const lastUse = eligible && lastUsed ? t`Last used ${lastUsed}` : undefined;
  const name = cleanupTargetDisplayName(target, words.kind(target.kind));
  const fullName = cleanupTargetFullName(target, words.kind(target.kind));
  const anonVol = isAnonymousVolumeTarget(target);
  const server = servers.length > 1 ? servers.find((s) => s.id === target.serverId)?.name : undefined;
  return (
    <>
      {anonVol ? (
        <span className="docker-volume-anon-name">
          <code title={fullName}>{name}</code>
          <CopyButton text={fullName} label={t`Copy volume name`} />
        </span>
      ) : (
        <code title={fullName !== name ? fullName : undefined}>{name}</code>
      )}
      {target.imageTags.length > 1 || target.imageDigests.length > 0 ? (
        <div className="docker-cleanup-meta">
          <code>{[...target.imageTags.slice(1), ...target.imageDigests].join(", ")}</code>
        </div>
      ) : null}
      <span className="docker-cleanup-meta-line">
        <span className="docker-cleanup-meta">{[reason, server, lastUse].filter(Boolean).join(" · ")}</span>
        {namedVolume ? <Tag words icon="alert">{t`Data is deleted`}</Tag> : null}
      </span>
    </>
  );
}
