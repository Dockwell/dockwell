// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate } from "@bufbuild/protobuf/wkt";
import { useQuery } from "@connectrpc/connect-query";
import type { BuildInfo } from "@dockwell/api";
import { SystemService } from "@dockwell/api";
import { Panel, StatusBadge } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";

import { formatDateTime } from "../../lib/format";
import { useElapsed } from "../../lib/useElapsed";
import { AccountPanel } from "../auth/AccountPanel";
import { ServersPanel } from "../servers/ServersPanel";
import { CopyCommand } from "./CopyCommand";
import { StandalonePage } from "./StandalonePage";
import { Unreachable } from "./Unreachable";
import "./StatusPage.css";

/**
 * What the panel shows until the first-run screens exist: whether this
 * control plane answers, which build it runs, and how to check it from a
 * terminal.
 */
export function StatusPage() {
  const { t } = useLingui();
  const title = t`Control plane status`;
  const version = useQuery(SystemService.method.getVersion, {});
  const host = window.location.host;
  const slow = useElapsed(version.isPending);

  if (version.isPending) {
    return (
      <StandalonePage title={title}>
        {slow ? <Connecting /> : <div className="status-reserve" aria-busy="true" />}
      </StandalonePage>
    );
  }
  if (version.isError) {
    return (
      <StandalonePage title={title}>
        <Unreachable error={version.error} retrying={version.isFetching} onRetry={() => void version.refetch()} />
      </StandalonePage>
    );
  }
  return (
    <StandalonePage title={title}>
      <Running host={host} build={version.data.build} />
    </StandalonePage>
  );
}

function Running({ host, build }: { host: string; build: BuildInfo | undefined }) {
  const { t } = useLingui();
  return (
    <>
      <header className="standalone-head">
        <h1 className="dw-text-title">
          <Trans>The control plane is running</Trans>
        </h1>
        <p className="standalone-lead">
          <Trans>Dockwell answers on {host}. Servers appear below as they join; apps and deploys come next.</Trans>
        </p>
      </header>
      <ServersPanel />
      <Panel
        title={t`This control plane`}
        actions={<StatusBadge status="healthy" label={t`Connected`} variant="pill" />}
      >
        {build ? <BuildFacts build={build} /> : null}
      </Panel>
      <Panel title={t`Check it from a terminal`}>
        <CopyCommand command={`curl -fsS ${window.location.origin}/healthz`} />
      </Panel>
      <AccountPanel />
    </>
  );
}

function BuildFacts({ build }: { build: BuildInfo }) {
  const { i18n } = useLingui();
  const commit = build.commit.slice(0, 12);
  return (
    <dl className="status-facts">
      <dt>
        <Trans>Version</Trans>
      </dt>
      <dd>
        <code>{build.version}</code>
      </dd>
      {commit ? (
        <>
          <dt>
            <Trans>Commit</Trans>
          </dt>
          <dd>
            <code>{commit}</code>
            {build.modified ? (
              <span className="status-muted">
                {" "}
                <Trans>(modified)</Trans>
              </span>
            ) : null}
          </dd>
        </>
      ) : null}
      {build.commitTime ? (
        <>
          <dt>
            <Trans>Committed</Trans>
          </dt>
          <dd>
            <time dateTime={timestampDate(build.commitTime).toISOString()}>
              {formatDateTime(timestampDate(build.commitTime), i18n.locale)}
            </time>
          </dd>
        </>
      ) : null}
      <dt>
        <Trans>Go</Trans>
      </dt>
      <dd>
        <code>{build.goVersion}</code>
      </dd>
      <dt>
        <Trans>Platform</Trans>
      </dt>
      <dd>
        <code>{build.platform}</code>
      </dd>
    </dl>
  );
}

function Connecting() {
  const { t } = useLingui();
  return (
    <header className="standalone-head">
      <StatusBadge status="deploying" label={t`Connecting`} />
      <h1 className="dw-text-title">
        <Trans>Connecting to the control plane</Trans>
      </h1>
    </header>
  );
}
