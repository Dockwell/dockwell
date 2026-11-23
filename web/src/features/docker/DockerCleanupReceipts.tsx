// SPDX-License-Identifier: AGPL-3.0-only

import { type DockerCleanupTarget, type DockerReceipt, DockerReceiptState } from "@dockwell/api";
import { Button, StatusBadge } from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { useLingui } from "@lingui/react/macro";
import { useMemo, useState } from "react";
import { formatBytes } from "../../lib/format";
import { CopyButton } from "../servers/CopyButton";
import { useDockerArtifactWords } from "./artifactWords";
import {
  cleanupTargetDisplayName,
  cleanupTargetFullName,
  cleanupTargetKey,
  isAnonymousVolumeTarget,
} from "./cleanupHelpers";
import "./DockerArtifacts.css";

const COMPACT_LIMIT = 20;

export function DockerCleanupReceipts({
  targets,
  receipts,
}: {
  targets: readonly DockerCleanupTarget[];
  receipts: readonly DockerReceipt[];
}) {
  const { t, i18n } = useLingui();
  const words = useDockerArtifactWords();
  const [expanded, setExpanded] = useState(false);

  const byEngineId = useMemo(() => {
    const map = new Map<string, DockerReceipt>();
    for (const row of receipts) {
      const id = row.target?.engineId;
      if (id) map.set(id, row);
    }
    return map;
  }, [receipts]);

  const ordered = useMemo(() => {
    if (targets.length <= COMPACT_LIMIT || receipts.length === 0) return targets;
    // Keep failures, changed items and unknown outcomes visible first in large batches.
    return [...targets].sort((a, b) => {
      const ra = byEngineId.get(a.engineId)?.state;
      const rb = byEngineId.get(b.engineId)?.state;
      const pa = ra === DockerReceiptState.REMOVED ? 1 : 0;
      const pb = rb === DockerReceiptState.REMOVED ? 1 : 0;
      return pa - pb;
    });
  }, [targets, receipts.length, byEngineId]);

  const removedCount = receipts.filter((row) => row.state === DockerReceiptState.REMOVED).length;
  const changedCount = receipts.filter((row) => row.state === DockerReceiptState.CHANGED).length;
  const failedCount = receipts.filter((row) => row.state === DockerReceiptState.FAILED).length;
  const unknownCount = receipts.filter((row) => row.state === DockerReceiptState.UNKNOWN).length;

  const totalFreedBytes = useMemo(() => {
    let sum = 0n;
    for (const row of receipts) {
      if (row.state !== DockerReceiptState.REMOVED) continue;
      if (row.beforeBytes !== undefined && row.afterBytes !== undefined && row.beforeBytes > row.afterBytes) {
        sum += row.beforeBytes - row.afterBytes;
      } else if (row.beforeBytes !== undefined && row.beforeBytes > 0n && row.afterBytes === undefined) {
        sum += row.beforeBytes;
      } else if (row.target?.estimatedBytes !== undefined && row.target.estimatedBytes > 0n) {
        sum += row.target.estimatedBytes;
      }
    }
    return sum;
  }, [receipts]);

  const summaryParts = [
    totalFreedBytes > 0n ? t`Freed ${formatBytes(totalFreedBytes, i18n.locale)}` : "",
    removedCount > 0 && (totalFreedBytes === 0n || targets.length > COMPACT_LIMIT) ? t`${removedCount} removed` : "",
    changedCount > 0 ? t`${changedCount} changed` : "",
    failedCount > 0 ? t`${failedCount} failed` : "",
    unknownCount > 0 ? t`${unknownCount} outcome unknown` : "",
  ].filter(Boolean);

  const shown = !expanded && ordered.length > COMPACT_LIMIT ? ordered.slice(0, COMPACT_LIMIT) : ordered;
  const hiddenCount = ordered.length - shown.length;

  return (
    <>
      {receipts.length > 0 && summaryParts.length > 0 ? (
        <div className="docker-cleanup-receipt-summary">{summaryParts.join(" · ")}</div>
      ) : null}
      <ul className="docker-cleanup-list">
        {shown.map((target) => {
          const receipt = byEngineId.get(target.engineId);
          const reason = words.receiptReason(receipt?.reason);
          const name = cleanupTargetDisplayName(target);
          const fullName = cleanupTargetFullName(target);
          const anonVol = isAnonymousVolumeTarget(target);
          const display = formatBytes(target.estimatedBytes ?? 0n, i18n.locale);
          const label =
            receipt?.state === DockerReceiptState.REMOVED
              ? t`Removed`
              : receipt?.state === DockerReceiptState.CHANGED
                ? t`Review again`
                : receipt?.state === DockerReceiptState.FAILED
                  ? t`Failed`
                  : t`Outcome unknown`;
          const status =
            receipt?.state === DockerReceiptState.REMOVED
              ? "healthy"
              : receipt?.state === DockerReceiptState.FAILED
                ? "down"
                : receipt?.state === DockerReceiptState.CHANGED
                  ? "degraded"
                  : "unknown";
          const metaParts = [
            words.kind(target.kind),
            words.reason(target.reason, target.kind, target.composeProject),
            !receipt && target.estimatedBytes !== undefined ? t`${display} estimated` : "",
          ].filter(Boolean);
          return (
            <li className="docker-cleanup-item docker-cleanup-receipt-row" key={cleanupTargetKey(target)}>
              <div className="docker-cleanup-row-main">
                <div className="docker-cleanup-receipt-line">
                  {anonVol ? (
                    <span className="docker-volume-anon-name">
                      <code title={fullName}>{name}</code>
                      <CopyButton text={fullName} label={t`Copy volume name`} />
                    </span>
                  ) : (
                    <code title={fullName !== name ? fullName : undefined}>{name}</code>
                  )}
                  <span className="docker-cleanup-meta">{metaParts.join(" · ")}</span>
                </div>
                {target.imageTags.length > 1 || target.imageDigests.length > 0 ? (
                  <div className="docker-cleanup-meta">
                    <code>{[...target.imageTags.slice(1), ...target.imageDigests].join(", ")}</code>
                  </div>
                ) : null}
                {reason ? <div className="docker-cleanup-meta">{reason}</div> : null}
              </div>
              {receipt ? <StatusBadge status={status} label={label} labelIsState /> : null}
            </li>
          );
        })}
      </ul>
      {hiddenCount > 0 ? (
        <div className="docker-cleanup-expand">
          <Button size="sm" variant="ghost" onClick={() => setExpanded(true)}>
            {plural(hiddenCount, {
              one: "Show # more artifact",
              other: "Show # more artifacts",
            })}
          </Button>
        </div>
      ) : null}
    </>
  );
}
