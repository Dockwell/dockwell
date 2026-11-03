// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate } from "@bufbuild/protobuf/wkt";
import { ConnectError } from "@connectrpc/connect";
import { useQuery } from "@connectrpc/connect-query";
import type { BuildInfo } from "@dockwell/api";
import { SystemService } from "@dockwell/api";
import { Button, Panel, StatusBadge } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { useEffect, useState } from "react";

import { formatDateTime } from "../../lib/format";
import { StandalonePage } from "./StandalonePage";
import "./StatusPage.css";

/** How long a request may run before the page says it is connecting, so fast answers never flash. */
const CONNECTING_DELAY_MS = 300;

/**
 * What the panel shows until the first-run screens exist: whether this
 * control plane answers, which build it runs, and how to check it from a
 * terminal.
 */
export function StatusPage() {
  const version = useQuery(SystemService.method.getVersion, {});
  const host = window.location.host;
  const slow = useElapsed(version.isPending, CONNECTING_DELAY_MS);

  if (version.isPending) {
    return (
      <StandalonePage>{slow ? <Connecting /> : <div className="status-reserve" aria-busy="true" />}</StandalonePage>
    );
  }
  if (version.isError) {
    return (
      <StandalonePage>
        <Unreachable
          host={host}
          error={ConnectError.from(version.error)}
          retrying={version.isFetching}
          onRetry={() => void version.refetch()}
        />
      </StandalonePage>
    );
  }
  return (
    <StandalonePage>
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
          <Trans>
            Dockwell answers on {host}. This early build has no servers or apps to show yet; the API is ready for tools
            and scripts.
          </Trans>
        </p>
      </header>
      <Panel
        title={t`This control plane`}
        actions={<StatusBadge status="healthy" label={t`Connected`} variant="pill" />}
      >
        {build ? <BuildFacts build={build} /> : null}
      </Panel>
      <Panel title={t`Check it from a terminal`}>
        <CopyCommand command={`curl -fsS ${window.location.origin}/healthz`} />
      </Panel>
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

interface UnreachableProps {
  host: string;
  error: ConnectError;
  retrying: boolean;
  onRetry: () => void;
}

function Unreachable({ host, error, retrying, onRetry }: UnreachableProps) {
  const { t } = useLingui();
  return (
    <>
      <header className="standalone-head">
        <StatusBadge status="down" label={t`Unreachable`} />
        <h1 className="dw-text-title">
          <Trans>Can't reach the control plane</Trans>
        </h1>
        <p className="standalone-lead">
          <Trans>
            The panel loaded, but {host} didn't answer the API. Check that <code>dockwell run</code> is still running on
            the server, then retry.
          </Trans>
        </p>
      </header>
      <div className="status-actions">
        <Button icon="refresh" loading={retrying} onClick={onRetry}>
          {retrying ? t`Retrying` : t`Retry`}
        </Button>
        <code className="status-error">{error.rawMessage || error.message}</code>
      </div>
    </>
  );
}

function CopyCommand({ command }: { command: string }) {
  const { t } = useLingui();
  const [copied, setCopied] = useState(false);

  useEffect(() => {
    if (!copied) return;
    const timer = setTimeout(() => setCopied(false), 2000);
    return () => clearTimeout(timer);
  }, [copied]);

  const copy = async () => {
    try {
      await navigator.clipboard.writeText(command);
      setCopied(true);
    } catch {
      // Clipboard access was refused; the command stays selectable.
    }
  };

  return (
    <div className="status-command">
      <code>{command}</code>
      <Button
        variant="ghost"
        size="sm"
        icon={copied ? "check" : "copy"}
        aria-label={copied ? t`Command copied` : t`Copy command`}
        onClick={() => void copy()}
      />
      <span className="dw-sr" aria-live="polite">
        {copied ? t`Command copied` : ""}
      </span>
    </div>
  );
}

/** True once `active` has stayed true for `ms`; false again as soon as it turns false. */
function useElapsed(active: boolean, ms: number): boolean {
  const [elapsed, setElapsed] = useState(false);
  useEffect(() => {
    if (!active) {
      setElapsed(false);
      return;
    }
    const timer = setTimeout(() => setElapsed(true), ms);
    return () => clearTimeout(timer);
  }, [active, ms]);
  return elapsed;
}
