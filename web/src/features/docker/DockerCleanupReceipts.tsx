// SPDX-License-Identifier: AGPL-3.0-only

import { DockerCleanupKind, type DockerCleanupTarget, type DockerReceipt, DockerReceiptState } from "@dockwell/api";
import { StatusBadge } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { formatBytes } from "../../lib/format";
import { useDockerArtifactWords } from "./artifactWords";
import "./DockerArtifacts.css";

export function DockerCleanupReceipts({
  targets,
  receipts,
}: {
  targets: readonly DockerCleanupTarget[];
  receipts: readonly DockerReceipt[];
}) {
  const { t, i18n } = useLingui();
  const words = useDockerArtifactWords();
  return (
    <ul className="docker-cleanup-list">
      {targets.map((target) => {
        const receipt = receipts.find((row) => row.target?.engineId === target.engineId);
        const reason = words.receiptReason(receipt?.reason);
        const name =
          target.kind === DockerCleanupKind.IMAGE && target.imageTags.length === 0
            ? target.engineId.slice(7, 19)
            : target.name || target.engineId;
        const display = formatBytes(target.estimatedBytes ?? 0n, i18n.locale);
        const before = receipt?.beforeBytes === undefined ? t`Unknown` : formatBytes(receipt.beforeBytes, i18n.locale);
        const after = receipt?.afterBytes === undefined ? t`Unknown` : formatBytes(receipt.afterBytes, i18n.locale);
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
        return (
          <li className="docker-cleanup-item" key={target.engineId}>
            <div>
              <code>{name}</code>
              <div className="docker-cleanup-meta">
                {words.kind(target.kind)} · {words.reason(target.reason, target.kind)}
              </div>
              {target.imageTags.length > 1 || target.imageDigests.length > 0 ? (
                <div className="docker-cleanup-meta">
                  <code>{[...target.imageTags.slice(1), ...target.imageDigests].join(", ")}</code>
                </div>
              ) : null}
              {receipt?.recheckedEngineId ? (
                <div className="docker-cleanup-meta">
                  <Trans>
                    Verified ID <code>{receipt.recheckedEngineId}</code>
                  </Trans>
                </div>
              ) : null}
              {reason ? <div className="docker-cleanup-meta">{reason}</div> : null}
              {receipt && (receipt.beforeBytes !== undefined || receipt.afterBytes !== undefined) ? (
                <div className="docker-cleanup-meta">
                  <Trans>
                    Before {before} · after {after}
                  </Trans>
                </div>
              ) : !receipt && target.estimatedBytes !== undefined ? (
                <div className="docker-cleanup-meta">{t`${display} estimated`}</div>
              ) : null}
            </div>
            {receipt ? <StatusBadge status={status} label={label} labelIsState /> : null}
          </li>
        );
      })}
    </ul>
  );
}
