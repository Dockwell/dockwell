// SPDX-License-Identifier: AGPL-3.0-only

import { StatusBadge, Toast } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { useLocation, useMatches } from "@tanstack/react-router";
import type { ReactNode } from "react";

import { useElapsed } from "../../lib/useElapsed";
import { StandalonePage } from "../system/StandalonePage";
import { Unreachable } from "../system/Unreachable";
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
  if (!state.data) {
    return (
      <StandalonePage title={t`Can't reach the control plane`}>
        <Unreachable error={state.error} retrying={state.isFetching} onRetry={() => void state.refetch()} />
      </StandalonePage>
    );
  }
  // A later check that failed leaves what the last one said in place, and
  // says quietly that it may be out of date.
  const stale = state.isError ? <Stale retrying={state.isFetching} onRetry={() => void state.refetch()} /> : null;
  let page = children;
  if (!state.data.claimed) {
    page = <Unclaimed serverName={state.data.serverName} />;
  } else if (!state.data.user) {
    page = <SignInPage next={location.href} />;
  }
  return (
    <>
      {page}
      {stale}
    </>
  );
}

function Stale({ retrying, onRetry }: { retrying: boolean; onRetry: () => void }) {
  const { t } = useLingui();
  return (
    <div className="dw-toaster">
      <Toast
        tone="warning"
        title={t`Can't reach the control plane`}
        description={
          retrying
            ? t`Checking again.`
            : t`What you see may be out of date. Check that dockwell run is still running on the server, then retry.`
        }
        action={{ label: t`Retry`, icon: "refresh", onClick: onRetry }}
      />
    </div>
  );
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
