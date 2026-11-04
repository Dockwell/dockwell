// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import { AuthService } from "@dockwell/api";
import { Button, Icon, Panel } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link } from "@tanstack/react-router";
import { useState } from "react";

import { useAuthState, useRefreshAuth } from "./useAuthState";
import "./AccountPanel.css";

/**
 * Who is signed in, with the way to their security settings and out, until
 * the panel shell has a place for them.
 */
export function AccountPanel() {
  const { t } = useLingui();
  const auth = useAuthState();
  const transport = useTransport();
  const refreshAuth = useRefreshAuth();
  const [busy, setBusy] = useState(false);
  const user = auth.data?.user;
  if (!user) return null;

  const signOut = async () => {
    setBusy(true);
    try {
      await createClient(AuthService, transport).signOut({});
    } finally {
      setBusy(false);
      await refreshAuth();
    }
  };

  return (
    <Panel
      title={t`Your account`}
      actions={
        <Button size="sm" icon="logout" loading={busy} onClick={() => void signOut()}>
          <Trans>Sign out</Trans>
        </Button>
      }
    >
      <div className="account">
        <p>
          <Trans>
            Signed in as <b>{user.name}</b> <span className="account-muted">({user.email})</span>
          </Trans>
        </p>
        <Link to="/profile" className="dw-btn dw-btn-secondary dw-btn-sm account-link">
          <Icon name="shield" size={14} />
          <span className="dw-btn-label">
            <Trans>Profile and security</Trans>
          </span>
        </Link>
      </div>
    </Panel>
  );
}
