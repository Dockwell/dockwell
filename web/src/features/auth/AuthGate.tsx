// SPDX-License-Identifier: AGPL-3.0-only

import { ConnectError } from "@connectrpc/connect";
import { Button, StatusBadge, Toast } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { useLocation, useMatches } from "@tanstack/react-router";
import type { ReactNode } from "react";

import { reasonOf } from "../../lib/errors";
import { useElapsed } from "../../lib/useElapsed";
import { StandalonePage } from "../system/StandalonePage";
import { Unreachable } from "../system/Unreachable";
import { useAuthError } from "./messages";
import { SignInPage } from "./SignInPage";
import { Unclaimed } from "./Unclaimed";
import { useAuthState } from "./useAuthState";
import "../system/Unreachable.css";

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
    // The control plane names its reason when it answers with a refusal,
    // such as for another address; with none, it didn't answer at all.
    return reasonOf(state.error) ? (
      <StandalonePage title={t`Can't check your session`}>
        <Refused error={state.error} retrying={state.isFetching} onRetry={() => void state.refetch()} />
      </StandalonePage>
    ) : (
      <StandalonePage title={t`Can't reach the control plane`}>
        <Unreachable error={state.error} retrying={state.isFetching} onRetry={() => void state.refetch()} />
      </StandalonePage>
    );
  }
  // A later check that failed leaves what the last one said in place, and
  // says quietly that it may be out of date.
  const stale = state.isError ? (
    <Stale error={state.error} retrying={state.isFetching} onRetry={() => void state.refetch()} />
  ) : null;
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

interface FailedProps {
  error: unknown;
  retrying: boolean;
  onRetry: () => void;
}

/**
 * The control plane answered the first check with a refusal, so there is
 * nothing to show yet: say why in the panel's words, and how to fix it.
 */
function Refused({ error, retrying, onRetry }: FailedProps) {
  const { t } = useLingui();
  const explain = useAuthError();
  const err = ConnectError.from(error);
  return (
    <>
      <header className="standalone-head">
        <StatusBadge status="down" label={t`Refused`} />
        <h1 className="dw-text-title">
          <Trans>Can't check your session</Trans>
        </h1>
        <p className="standalone-lead">{explain(error)}</p>
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

function Stale({ error, retrying, onRetry }: FailedProps) {
  const { t } = useLingui();
  const explain = useAuthError();
  const answered = reasonOf(error) !== "";
  let description = t`What you see may be out of date. Check that dockwell run is still running on the server, then retry.`;
  if (retrying) description = t`Checking again.`;
  else if (answered) description = t`What you see may be out of date. ${explain(error)}`;
  return (
    <div className="dw-toaster">
      <Toast
        tone="warning"
        title={answered ? t`Can't check your session` : t`Can't reach the control plane`}
        description={description}
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
