// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate } from "@bufbuild/protobuf/wkt";
import {
  ChangeState,
  DockerCleanupKind,
  type DockerCleanupPause,
  DockerCleanupReason,
  type DockerCleanupTarget,
  DockerReadState,
  DockerService,
} from "@dockwell/api";
import { Button, Callout, Icon, Loading, Panel, Sheet, Skeleton, SkeletonCard, SkeletonRows, Tag } from "@dockwell/ui";
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

export interface DockerPruneSheetProps {
  kind: DockerCleanupKind;
  servers: readonly CleanupServer[];
  /** Optional single-target focus, such as removing one unused volume from its detail page. */
  focusEngineId?: string | undefined;
  onClose: () => void;
  onApplied?: (() => void) | undefined;
}

export function DockerPruneSheet({ kind, servers, focusEngineId, onClose, onApplied }: DockerPruneSheetProps) {
  const { t, i18n } = useLingui();
  const words = useDockerArtifactWords();
  const sheet = useChangeSheet();
  const may = useMay()(DockerService.method.planDockerMaintenance).allowed;
  const [overrides, setOverrides] = useState<ReadonlyMap<string, boolean>>(() => new Map());

  const filters = useMemo<CleanupFilterSpec>(
    () => ({
      taggedImages: kind === DockerCleanupKind.IMAGE,
      networks: kind === DockerCleanupKind.NETWORK,
      volumes: kind === DockerCleanupKind.VOLUME,
      kinds: [kind],
    }),
    [kind],
  );

  const preview = useCleanupPreview(servers, filters, true);
  const candidates = useMemo(
    () =>
      focusEngineId
        ? preview.candidates.filter((row) => row.engineId === focusEngineId)
        : preview.candidates.filter((row) => row.kind === kind),
    [preview.candidates, focusEngineId, kind],
  );
  const protectedRows = useMemo(
    () =>
      focusEngineId
        ? preview.protectedRows.filter((row) => row.engineId === focusEngineId)
        : preview.protectedRows.filter((row) => row.kind === kind),
    [preview.protectedRows, focusEngineId, kind],
  );

  const bounded = useMemo(
    () => computeBoundedDefaults(candidates, preview.serverLimits, preview.maxTargetsTotal, focusEngineId),
    [candidates, preview.serverLimits, preview.maxTargetsTotal, focusEngineId],
  );
  const { selected, selectedKeys, deferredCount, canSelectTarget } = useMemo(
    () => resolveBoundedSelection(candidates, bounded, overrides),
    [candidates, bounded, overrides],
  );

  const isChecked = (target: DockerCleanupTarget) => selectedKeys.has(cleanupTargetKey(target));
  const safeCandidates = candidates.filter((row) => !isOptInTaggedImage(row) && !isOptInNamedVolume(row));
  const optInCandidates = candidates.filter((row) => isOptInTaggedImage(row) || isOptInNamedVolume(row));

  const estimatedBytes = selected.reduce((sum, target) => sum + (target.estimatedBytes ?? 0n), 0n);
  const hasMeasuredBytes = selected.some((target) => target.estimatedBytes !== undefined);
  const estimated = hasMeasuredBytes ? formatBytes(estimatedBytes, i18n.locale) : undefined;

  const setOne = (target: DockerCleanupTarget, checked: boolean) => {
    setOverrides((prev) => {
      const next = new Map(prev);
      next.set(cleanupTargetKey(target), checked);
      return next;
    });
  };

  const setGroup = (group: readonly DockerCleanupTarget[], checked: boolean) => {
    setOverrides((prev) => {
      const next = new Map(prev);
      for (const row of group) next.set(cleanupTargetKey(row), checked);
      return next;
    });
  };

  const title = focusEngineId
    ? t`Remove unused volume`
    : kind === DockerCleanupKind.IMAGE
      ? t`Remove unused images`
      : kind === DockerCleanupKind.VOLUME
        ? t`Remove unused volumes`
        : kind === DockerCleanupKind.NETWORK
          ? t`Remove unused networks`
          : t`Remove build cache`;

  const emptyTitle = focusEngineId
    ? t`This volume can't be removed here`
    : kind === DockerCleanupKind.IMAGE
      ? t`No unused images to remove`
      : kind === DockerCleanupKind.VOLUME
        ? t`No unused volumes to remove`
        : kind === DockerCleanupKind.NETWORK
          ? t`No unused networks to remove`
          : t`No build cache to remove`;

  const slowLine =
    servers.length > 1
      ? t`Docker is measuring disk use on these servers`
      : t`Docker is measuring disk use on this server`;

  const initialLoading = preview.query.isPending || ((preview.measuring || preview.loadingMore) && !preview.hasAnyData);
  const pendingMore = !initialLoading && (preview.measuring || preview.loadingMore);

  const review = () => {
    if (!preview.snapshot || selected.length === 0 || pendingMore) return;
    const targets = selected;
    const expectedSnapshot = preview.snapshot;
    const serverIds = preview.serverIds;
    onClose();
    sheet.open({
      kind: "plan",
      retryLabel: t`Review again`,
      make: () =>
        preview.client.planDockerMaintenance({
          serverIds,
          expectedSnapshot,
          operation: {
            case: "cleanup",
            value: {
              filters: {
                taggedImages: filters.taggedImages,
                networks: filters.networks,
                volumes: filters.volumes,
                kinds: [...filters.kinds],
              },
              targets,
            },
          },
        }),
      onClose: (change) => {
        preview.invalidateAfterCleanup();
        if (change?.state === ChangeState.APPLIED) onApplied?.();
      },
    });
  };

  const reviewResume = (pause: DockerCleanupPause) => {
    onClose();
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

  const allOptInSelected = optInCandidates.length > 0 && optInCandidates.every(isChecked);
  const allSafeSelected = safeCandidates.length > 0 && safeCandidates.every(isChecked);
  const readableServers =
    preview.unavailable.filter((row) => row.state !== DockerReadState.UNSPECIFIED).length + preview.pauses.length <
    servers.length;
  const unsupportedVolumeNames =
    kind === DockerCleanupKind.VOLUME
      ? bounded.unsupportedVolumeServers.map((id) => servers.find((s) => s.id === id)?.name ?? id)
      : [];
  const isEmptyFinished = !initialLoading && !pendingMore && candidates.length === 0;
  const stale = reasonOf(preview.query.error) === "docker_cleanup_changed";

  const subtitleParts = [
    estimated ? t`${selected.length} selected · ${estimated} estimated` : t`${selected.length} selected`,
    deferredCount > 0 ? t`${deferredCount} more after this cleanup` : "",
  ].filter(Boolean);

  return (
    <Sheet
      open
      className="docker-prune-sheet"
      title={title}
      subtitle={!initialLoading && candidates.length > 0 ? subtitleParts.join(" · ") : undefined}
      onClose={onClose}
      footer={
        isEmptyFinished ? (
          <Button variant="secondary" onClick={onClose}>
            <Trans>Close</Trans>
          </Button>
        ) : (
          <>
            <Button variant="secondary" onClick={onClose}>
              <Trans>Cancel</Trans>
            </Button>
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
          </>
        )
      }
    >
      <div className="docker-prune-body">
        <Unread
          values={preview.unavailable.filter((row) => row.state !== DockerReadState.UNSPECIFIED)}
          servers={servers}
          retry={preview.reviewAgain}
        />
        {!initialLoading && unsupportedVolumeNames.length > 0 ? (
          <Callout tone="info" title={t`Volume cleanup needs an agent update`}>
            {t`Update the agent on ${unsupportedVolumeNames.join(", ")} to remove unused Docker volumes.`}
          </Callout>
        ) : null}
        {preview.pauses.map((pause) => {
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
        {initialLoading && !preview.query.error ? (
          <Loading label={t`Checking Docker artifacts`} slow={slowLine}>
            <SkeletonCard title={<Skeleton text="heading" width={140} />} flush>
              <SkeletonRows rows={4} lines={1} end="value" dense />
            </SkeletonCard>
          </Loading>
        ) : null}
        {isEmptyFinished &&
        !preview.query.error &&
        readableServers &&
        unsupportedVolumeNames.length < servers.length ? (
          <EmptyState icon={words.icon(kind)} title={emptyTitle} />
        ) : null}
        {!initialLoading && safeCandidates.length > 0 ? (
          <Panel
            title={
              kind === DockerCleanupKind.IMAGE
                ? t`Dangling images`
                : kind === DockerCleanupKind.VOLUME
                  ? t`Anonymous volumes`
                  : words.kind(kind)
            }
            actions={
              !focusEngineId ? (
                <Button size="sm" variant="ghost" onClick={() => setGroup(safeCandidates, !allSafeSelected)}>
                  {allSafeSelected ? t`Deselect all` : t`Select all`}
                </Button>
              ) : undefined
            }
          >
            <ul className="docker-cleanup-list">
              {safeCandidates.map((target) => {
                const checked = isChecked(target);
                return (
                  <PruneCandidateRow
                    key={cleanupTargetKey(target)}
                    target={target}
                    servers={servers}
                    checked={checked}
                    disabled={!checked && !canSelectTarget(target)}
                    onChange={(next) => setOne(target, next)}
                  />
                );
              })}
            </ul>
          </Panel>
        ) : null}
        {!initialLoading && optInCandidates.length > 0 ? (
          <Panel
            title={kind === DockerCleanupKind.VOLUME ? t`Named volumes` : t`Unused tagged images`}
            actions={
              !focusEngineId ? (
                <label className="docker-cleanup-group-toggle">
                  <input
                    type="checkbox"
                    checked={allOptInSelected}
                    onChange={(event) => setGroup(optInCandidates, event.target.checked)}
                  />
                  <span>{t`Include all (${optInCandidates.length})`}</span>
                </label>
              ) : undefined
            }
          >
            <ul className="docker-cleanup-list">
              {optInCandidates.map((target) => {
                const checked = isChecked(target);
                return (
                  <PruneCandidateRow
                    key={cleanupTargetKey(target)}
                    target={target}
                    servers={servers}
                    checked={checked}
                    disabled={!checked && !canSelectTarget(target)}
                    onChange={(next) => setOne(target, next)}
                  />
                );
              })}
            </ul>
          </Panel>
        ) : null}
        {pendingMore && !preview.query.error ? (
          <Loading label={t`Checking remaining Docker artifacts`} slow={slowLine}>
            <SkeletonCard title={<Skeleton text="heading" width={140} />} flush>
              <SkeletonRows rows={2} lines={1} end="value" dense />
            </SkeletonCard>
          </Loading>
        ) : null}
        {!initialLoading && protectedRows.length > 0 ? (
          <details className="docker-cleanup-disclosure" open={candidates.length === 0 || undefined}>
            <summary>
              <span className="docker-disclosure-label">
                <Icon name="lock" size={14} />{" "}
                {plural(protectedRows.length, {
                  one: "Protected (# item)",
                  other: "Protected (# items)",
                })}
              </span>
            </summary>
            <Panel title={words.kind(kind)}>
              <ul className="docker-cleanup-list">
                {protectedRows.map((target) => (
                  <li className="docker-cleanup-item" key={cleanupTargetKey(target)}>
                    <div className="docker-cleanup-row-main">
                      <PruneTargetLabel target={target} servers={servers} />
                    </div>
                    <div className="docker-cleanup-item-end">
                      {target.reason === DockerCleanupReason.COMPOSE && target.composeId ? (
                        <Link
                          className="dw-link docker-row-link"
                          to="/servers/$serverId/docker/composes/$composeId"
                          params={{ serverId: target.serverId, composeId: target.composeId }}
                          onClick={onClose}
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
          </details>
        ) : null}
      </div>
    </Sheet>
  );
}

function PruneCandidateRow({
  target,
  servers,
  checked,
  disabled,
  onChange,
}: {
  target: DockerCleanupTarget;
  servers: readonly CleanupServer[];
  checked: boolean;
  disabled: boolean;
  onChange: (checked: boolean) => void;
}) {
  const { t, i18n } = useLingui();
  const namedVolume = isOptInNamedVolume(target);
  return (
    <li className="docker-cleanup-item">
      <label className="docker-cleanup-choice">
        <input
          type="checkbox"
          checked={checked}
          disabled={disabled}
          onChange={(event) => onChange(event.target.checked)}
        />
        <span className="docker-cleanup-row-main">
          <PruneTargetLabel target={target} servers={servers} namedVolume={namedVolume} />
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
}

function PruneTargetLabel({
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
