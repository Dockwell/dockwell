// SPDX-License-Identifier: AGPL-3.0-only

import { ConnectError } from "@connectrpc/connect";
import { Button, StatusBadge } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { useLocation, useMatches } from "@tanstack/react-router";
import type { ReactNode } from "react";

import { useElapsed } from "../../lib/useElapsed";
import { StandalonePage } from "../system/StandalonePage";
import { SignInPage } from "./SignInPage";
import { Unclaimed } from "./Unclaimed";
import { useAuthState } from "./useAuthState";

/**
 * Shows a page only to someone signed in. Routes marked public (the claim
 * and sign-in screens) pass straight through. Everyone else sees why they
 * can't continue in place of the page, keeping its address: the install has
 * no owner yet, or the sign-in screen, which returns here afterwards.
 */
export function AuthGate({ children }: { children: ReactNode }) {
  const isPublic = useMatches({ select: (matches) => matches.some((m) => m.staticData?.public === true) });
  return isPublic ? children : <SignedInOnly>{children}</SignedInOnly>;
}

function SignedInOnly({ children }: { children: ReactNode }) {
  const { t } = useLingui();
  const state = useAuthState();
  const location = useLocation();
  const slow = useElapsed(state.isPending);

  if (state.isPending) {
    return (
      <StandalonePage title={t`Checking your session`}>
        {slow ? <Checking /> : <div className="auth-reserve" aria-busy="true" />}
      </StandalonePage>
    );
  }
  if (state.isError) {
    return (
      <StandalonePage title={t`Can't reach the control plane`}>
        <Unreachable
          error={ConnectError.from(state.error)}
          retrying={state.isFetching}
          onRetry={() => void state.refetch()}
        />
      </StandalonePage>
    );
  }
  if (!state.data.claimed) {
    return <Unclaimed serverName={state.data.serverName} />;
  }
  if (!state.data.user) {
    return <SignInPage next={location.href} />;
  }
  return children;
}

function Checking() {
  const { t } = useLingui();
  return (
    <header className="standalone-head">
      <StatusBadge status="deploying" label={t`Checking`} />
      <h1 className="dw-text-title">
        <Trans>Checking your session</Trans>
      </h1>
    </header>
  );
}

function Unreachable({ error, retrying, onRetry }: { error: ConnectError; retrying: boolean; onRetry: () => void }) {
  const { t } = useLingui();
  const host = window.location.host;
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
      <div className="auth-actions">
        <Button icon="refresh" loading={retrying} onClick={onRetry}>
          {retrying ? t`Retrying` : t`Retry`}
        </Button>
        <code className="auth-error-detail">{error.rawMessage || error.message}</code>
      </div>
    </>
  );
}
