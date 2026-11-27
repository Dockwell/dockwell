// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import {
  ChangeService,
  type DeployOrigin,
  DeployProvider,
  type DeployStatus,
  DeployStatusReason,
  DeployStatusState,
  DeployStatusTransition,
} from "@dockwell/api";
import { Button } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link, useRouter } from "@tanstack/react-router";
import type { ReactNode } from "react";
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

function shortRepoLabel(repoUrl: string): string {
  const parsed = safeHttps(repoUrl);
  if (!parsed) return repoUrl;
  const path = parsed.pathname.replace(/^\/+|\/+$/g, "").replace(/\.git$/i, "");
  return path ? `${parsed.host}/${path}` : parsed.host;
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

function PanelLink({
  to,
  label,
  onClick,
}: {
  to: "/settings/integrations" | "/settings/api-tokens";
  label: string;
  onClick: (event: { stopPropagation: () => void }) => void;
}) {
  const router = useRouter({ warn: false });
  if (!router) {
    return (
      <a className="cp-link" href={to} onClick={onClick}>
        {label}
      </a>
    );
  }
  return (
    <Link className="cp-link" to={to} onClick={onClick}>
      {label}
    </Link>
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

  const stop = (event: { stopPropagation: () => void }) => event.stopPropagation();
  const integrationsLink = (label: string) => <PanelLink to="/settings/integrations" label={label} onClick={stop} />;
  const tokensLink = (label: string) => <PanelLink to="/settings/api-tokens" label={label} onClick={stop} />;

  const reasonView = (): { text: string; action?: ReactNode } => {
    switch (status.reason) {
      case DeployStatusReason.NO_TOKEN_REPOSITORY:
        return {
          text: t`Not reported: no repository set on token`,
          action: tokensLink(t`Set repository`),
        };
      case DeployStatusReason.REPOSITORY_MISMATCH: {
        const repoLabel = shortRepoLabel(status.tokenRepository);
        return {
          text: status.tokenRepository
            ? t`Not reported: this token reports to ${repoLabel}`
            : t`Not reported: repository doesn't match token`,
          action: status.tokenRepository ? tokensLink(t`API tokens`) : undefined,
        };
      }
      case DeployStatusReason.PROVIDER_UNSUPPORTED:
        return { text: t`Not reported: provider not supported` };
      case DeployStatusReason.NO_GITHUB_APP:
        return {
          text: t`Not reported: GitHub isn't connected`,
          action: integrationsLink(t`Connect GitHub`),
        };
      case DeployStatusReason.MISSING_CHECKS_PERMISSION: {
        const grantUrl = safeHttps(status.actionUrl);
        return {
          text: t`Not reported: GitHub App needs Checks permission`,
          action: grantUrl ? (
            <a href={grantUrl.href} target="_blank" rel="noreferrer" onClick={stop}>
              {t`Grant on GitHub`}
            </a>
          ) : (
            integrationsLink(t`Integrations`)
          ),
        };
      }
      case DeployStatusReason.GITHUB_NOT_REACHED:
        return {
          text: t`Not reported: GitHub couldn't be reached`,
          action: integrationsLink(t`Integrations`),
        };
      case DeployStatusReason.GITHUB_AUTH_FAILED:
        return {
          text: t`GitHub refused status update`,
          action: integrationsLink(t`Integrations`),
        };
      case DeployStatusReason.NO_GITLAB_CONNECTION:
        return {
          text: t`Not reported: GitLab isn't connected`,
          action: integrationsLink(t`Connect GitLab`),
        };
      case DeployStatusReason.GITLAB_NOT_REACHED:
        return {
          text: t`Not reported: GitLab couldn't be reached`,
          action: integrationsLink(t`Integrations`),
        };
      case DeployStatusReason.GITLAB_AUTH_FAILED:
        return {
          text: t`GitLab refused token`,
          action: integrationsLink(t`Integrations`),
        };
      case DeployStatusReason.GITLAB_MISSING_SCOPE:
        return {
          text: t`GitLab token needs api scope`,
          action: integrationsLink(t`Integrations`),
        };
      case DeployStatusReason.NETGUARD_BLOCKED:
        return {
          text: t`GitLab address is blocked`,
          action: integrationsLink(t`Integrations`),
        };
      case DeployStatusReason.GAVE_UP:
        return {
          text: providerLabel ? t`Stopped retrying status on ${providerLabel}` : t`Stopped retrying deploy status`,
        };
      default:
        return { text: t`Couldn't report deploy status` };
    }
  };

  switch (status.state) {
    case DeployStatusState.DELIVERED: {
      const discarded = status.transition === DeployStatusTransition.DISCARDED;
      const label = discarded
        ? providerLabel
          ? t`Reported discarded to ${providerLabel}`
          : t`Reported discarded`
        : providerLabel
          ? t`Reported to ${providerLabel}`
          : t`Status reported`;
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
      const { text, action } = reasonView();
      const display = problem || text;
      return (
        <span className={`ci-deploy-status ${status.state === DeployStatusState.FAILED ? "is-failed" : "is-skipped"}`}>
          <span className="ci-deploy-status-reason" title={status.error || display}>
            {display}
          </span>
          {action}
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
