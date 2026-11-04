// SPDX-License-Identifier: AGPL-3.0-only

import { Button, Panel } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";

import { RouterButtonLink } from "../../app/RouterButtonLink";
import { useAuthState } from "./useAuthState";
import { useSignOut } from "./useSignOut";
import "./AccountPanel.css";

/**
 * Who is signed in, with the way to their security settings and out, until
 * the panel shell has a place for them.
 */
export function AccountPanel() {
  const { t } = useLingui();
  const auth = useAuthState();
  const signOut = useSignOut();
  const user = auth.data?.user;
  if (!user) return null;

  return (
    <Panel
      title={t`Your account`}
      actions={
        <Button size="sm" icon="logout" loading={signOut.busy} onClick={() => void signOut.run()}>
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
        <RouterButtonLink to="/profile" size="sm" icon="shield">
          <Trans>Profile and security</Trans>
        </RouterButtonLink>
      </div>
      {signOut.problem ? (
        <p className="account-error" role="alert">
          {signOut.problem}
        </p>
      ) : null}
    </Panel>
  );
}
