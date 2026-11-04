// SPDX-License-Identifier: AGPL-3.0-only

import { ConnectError } from "@connectrpc/connect";
import { Button, StatusBadge } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";

import "./Unreachable.css";

export interface UnreachableProps {
  error: unknown;
  retrying: boolean;
  onRetry: () => void;
}

/**
 * The control plane didn't answer the panel's first question, so there is
 * nothing to show yet: say so, how to check, and retry. For a later failure,
 * with something already on screen, keep the page and say it's stale instead.
 */
export function Unreachable({ error, retrying, onRetry }: UnreachableProps) {
  const { t } = useLingui();
  const host = window.location.host;
  const err = ConnectError.from(error);
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
      <div className="unreachable-actions">
        <Button icon="refresh" loading={retrying} onClick={onRetry}>
          {retrying ? t`Retrying` : t`Retry`}
        </Button>
        <code className="unreachable-detail">{err.rawMessage || err.message}</code>
      </div>
    </>
  );
}
