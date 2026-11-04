// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import { AuthService } from "@dockwell/api";
import { Button, StatusBadge } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { useNavigate } from "@tanstack/react-router";
import { useEffect, useRef, useState } from "react";

import { CopyCommand } from "../system/CopyCommand";
import { StandalonePage } from "../system/StandalonePage";
import { useFragmentToken } from "./ClaimPage";
import { useAuthError } from "./messages";
import { useRefreshAuth } from "./useAuthState";
import "./auth.css";

/**
 * Opens a one-time sign-in link from dockwell recover: it signs in at once,
 * then asks for a passkey for this address.
 */
export function RecoverPage() {
  const { t } = useLingui();
  const token = useFragmentToken("/recover");
  const transport = useTransport();
  const navigate = useNavigate();
  const refreshAuth = useRefreshAuth();
  const explain = useAuthError();
  const started = useRef(false);
  const [problem, setProblem] = useState<string>();

  useEffect(() => {
    // Once only: the link works once, and strict mode runs effects twice.
    if (started.current) return;
    started.current = true;
    if (!token) {
      setProblem(t`This page needs the whole link that dockwell recover printed, including the part after “#”.`);
      return;
    }
    void (async () => {
      try {
        await createClient(AuthService, transport).signInWithLink({ token });
        await refreshAuth();
        await navigate({ to: "/profile", search: { via: "link" }, replace: true });
      } catch (err) {
        setProblem(explain(err));
      }
    })();
  }, [token, transport, refreshAuth, navigate, explain, t]);

  if (!problem) {
    return (
      <StandalonePage title={t`Sign in with a link`}>
        <header className="standalone-head">
          <StatusBadge status="deploying" label={t`Signing in`} />
          <h1 className="dw-text-title">
            <Trans>Signing you in with a one-time link</Trans>
          </h1>
        </header>
      </StandalonePage>
    );
  }
  return (
    <StandalonePage title={t`Sign in with a link`}>
      <header className="standalone-head">
        <h1 className="dw-text-title">
          <Trans>This sign-in link didn't work</Trans>
        </h1>
        <p className="standalone-lead">{problem}</p>
      </header>
      <p className="auth-small">
        <Trans>Run this on the server over SSH for a new link. It works once, for 15 min.</Trans>
      </p>
      <CopyCommand command="dockwell recover" />
      <div className="auth-actions">
        <Button icon="key" onClick={() => void navigate({ to: "/sign-in" })}>
          <Trans>Sign in another way</Trans>
        </Button>
      </div>
    </StandalonePage>
  );
}
