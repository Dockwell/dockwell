// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import { ChangeService, type DeployOrigin, DeployProvider, type DeployStatus, DeployStatusState } from "@dockwell/api";
import { Button } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { useState } from "react";

import { sentence } from "../../lib/errors";
import { useMay } from "../auth/may";
import "./DeploySource.css";

function safeHttps(value: string): URL | undefined {
  try {
    const url = new URL(value);
    return url.protocol === "https:" && !url.username && !url.password ? url : undefined;
  } catch {
    return undefined;
  }
}

export function DeploySource({
  origin,
  status,
  changeId,
  environment,
  onRetried,
}: {
  origin: DeployOrigin;
  status?: DeployStatus;
  changeId?: string;
  environment?: string;
  onRetried?: () => void;
}) {
  const { t } = useLingui();
  const commitPage = safeHttps(origin.commitUrl);
  const run = safeHttps(origin.runUrl);
  const commit = origin.commit.slice(0, 7);
  const shortRef = origin.ref.replace(/^refs\/(heads|tags)\//, "");

  const link = commitPage ? (
    <a href={commitPage.href} target="_blank" rel="noreferrer" onClick={(event) => event.stopPropagation()}>
      {commit}
    </a>
  ) : (
    commit
  );
  return (
    <span className="ci-deploy-source">
      <span>
        <Trans>
          From {link} on {shortRef}
        </Trans>
      </span>
      {run ? (
        <a href={run.href} target="_blank" rel="noreferrer" onClick={(event) => event.stopPropagation()}>
          {t`CI run`}
        </a>
      ) : null}
      {status ? (
        <DeployStatusLine status={status} changeId={changeId} environment={environment} onRetried={onRetried} />
      ) : null}
    </span>
  );
}

function DeployStatusLine({
  status,
  changeId,
  environment,
  onRetried,
}: {
  status: DeployStatus;
  changeId?: string;
  environment?: string;
  onRetried?: () => void;
}) {
  const { t } = useLingui();
  const transport = useTransport();
  const may = useMay();
  const mayRetry = may(ChangeService.method.retryDeployStatus, environment ? { environment } : undefined);
  const [busy, setBusy] = useState(false);
  const [retriedFrom, setRetriedFrom] = useState<string>();
  const [problem, setProblem] = useState<string>();
  const statusKey = `${status.state}:${status.attempts}:${status.updateTime?.seconds ?? 0}`;

  const providerLabel =
    status.provider === DeployProvider.GITHUB_ACTIONS
      ? "GitHub"
      : status.provider === DeployProvider.GITLAB_CI
        ? "GitLab"
        : "";

  const retry = async () => {
    if (!changeId) return;
    setBusy(true);
    setProblem(undefined);
    try {
      await createClient(ChangeService, transport).retryDeployStatus({ id: changeId });
      setRetriedFrom(statusKey);
      onRetried?.();
    } catch (err) {
      setProblem(sentence(err));
    } finally {
      setBusy(false);
    }
  };

  if (retriedFrom && retriedFrom === statusKey) {
    return (
      <span className="ci-deploy-status is-pending">
        {providerLabel ? t`Reporting to ${providerLabel}` : t`Reporting status`}
      </span>
    );
  }

  switch (status.state) {
    case DeployStatusState.DELIVERED: {
      const label = providerLabel ? t`Reported to ${providerLabel}` : t`Status reported`;
      const remote = safeHttps(status.remoteUrl);
      return (
        <span className="ci-deploy-status is-delivered">
          {remote ? (
            <a href={remote.href} target="_blank" rel="noreferrer" onClick={(event) => event.stopPropagation()}>
              {label}
            </a>
          ) : (
            label
          )}
        </span>
      );
    }
    case DeployStatusState.PENDING:
      return (
        <span className="ci-deploy-status is-pending" title={status.error || undefined}>
          {status.attempts > 0
            ? providerLabel
              ? t`Retrying status on ${providerLabel}`
              : t`Retrying status`
            : providerLabel
              ? t`Reporting to ${providerLabel}`
              : t`Reporting status`}
        </span>
      );
    case DeployStatusState.FAILED:
    case DeployStatusState.SKIPPED: {
      const reason = problem || status.error || t`Couldn't report deploy status.`;
      return (
        <span className={`ci-deploy-status ${status.state === DeployStatusState.FAILED ? "is-failed" : "is-skipped"}`}>
          <span className="ci-deploy-status-reason" title={reason}>
            {reason}
          </span>
          {status.canRetry && changeId ? (
            <Button
              size="sm"
              variant="ghost"
              loading={busy}
              onClick={(event) => {
                event.stopPropagation();
                void retry();
              }}
              {...mayRetry.off}
            >
              <Trans>Retry</Trans>
            </Button>
          ) : null}
        </span>
      );
    }
    default:
      return null;
  }
}
