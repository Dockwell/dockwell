// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate } from "@bufbuild/protobuf/wkt";
import { createClient } from "@connectrpc/connect";
import { useQuery, useTransport } from "@connectrpc/connect-query";
import { AuthService, type Role } from "@dockwell/api";
import { Button, Callout, Panel, StatusBadge, TextField } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { useNavigate } from "@tanstack/react-router";
import { type FormEvent, useState } from "react";

import { formatDateTime } from "../../lib/format";
import { createPasskey, passkeysAvailable } from "../../lib/webauthn";
import { StandalonePage } from "../system/StandalonePage";
import { useFragmentToken } from "./ClaimPage";
import { useDeviceName } from "./device";
import { useAuthError } from "./messages";
import { useAuthState, useRefreshAuth } from "./useAuthState";
import { useRolePhrase } from "./user";
import { useSignOut } from "./useSignOut";
import "./auth.css";

/**
 * Accepts an invite from /invite#secret: says who invited the person, as what
 * and where, then makes them with a passkey for this address and signs them
 * in. Opening it spends nothing; only the passkey does (ADR 0049).
 */
export function AcceptInvitePage() {
  const { t } = useLingui();
  const secret = useFragmentToken("/invite");
  const auth = useAuthState();
  const check = useQuery(AuthService.method.checkInvite, { secret }, { enabled: secret !== "", retry: false });
  const explain = useAuthError();
  const signedIn = auth.data?.user;

  if (!secret) {
    return (
      <Problem title={t`This page needs the whole link from your invite`}>
        <Trans>
          Open the link you were sent, including the part after “#”. Whoever invited you can make a new one.
        </Trans>
      </Problem>
    );
  }
  if (signedIn) {
    return <SignedInElsewhere name={signedIn.name} />;
  }
  if (check.error) {
    return <Problem title={t`This invite didn't work`}>{explain(check.error)}</Problem>;
  }
  if (!check.data) {
    return (
      <StandalonePage title={t`Accept an invite`}>
        <header className="standalone-head">
          <StatusBadge status="deploying" label={t`Reading the invite`} />
        </header>
      </StandalonePage>
    );
  }
  return <AcceptForm secret={secret} invite={check.data} />;
}

function Problem({ title, children }: { title: string; children: React.ReactNode }) {
  const { t } = useLingui();
  return (
    <StandalonePage title={t`Accept an invite`}>
      <header className="standalone-head">
        <h1 className="dw-text-title">{title}</h1>
        <p className="standalone-lead">{children}</p>
      </header>
    </StandalonePage>
  );
}

/** Accepting only ever makes a new person, so someone signed in signs out first. */
function SignedInElsewhere({ name }: { name: string }) {
  const { t } = useLingui();
  const signOut = useSignOut();
  return (
    <StandalonePage title={t`Accept an invite`}>
      <header className="standalone-head">
        <h1 className="dw-text-title">
          <Trans>You're signed in as {name}</Trans>
        </h1>
        <p className="standalone-lead">
          <Trans>An invite makes someone new. Sign out, or open the link in a private window.</Trans>
        </p>
      </header>
      <div className="auth-actions">
        <Button icon="logout" onClick={() => void signOut.run()} loading={signOut.busy}>
          <Trans>Sign out</Trans>
        </Button>
      </div>
    </StandalonePage>
  );
}

interface Offer {
  email: string;
  role: Role;
  projectName: string;
  invitedBy: string;
  expireTime?: Parameters<typeof timestampDate>[0];
}

function AcceptForm({ secret, invite }: { secret: string; invite: Offer }) {
  const { t, i18n } = useLingui();
  const transport = useTransport();
  const navigate = useNavigate();
  const refreshAuth = useRefreshAuth();
  const explain = useAuthError();
  const device = useDeviceName();
  const rolePhrase = useRolePhrase();
  const [name, setName] = useState("");
  const [invalid, setInvalid] = useState<string>();
  const [busy, setBusy] = useState(false);
  const [problem, setProblem] = useState<unknown>();
  const canUsePasskeys = passkeysAvailable();
  const where = rolePhrase(invite.role, invite.projectName);

  const submit = async (event: FormEvent) => {
    event.preventDefault();
    if (!name.trim()) {
      setInvalid(t`Enter your name.`);
      return;
    }
    setInvalid(undefined);
    setBusy(true);
    setProblem(undefined);
    try {
      const client = createClient(AuthService, transport);
      const begin = await client.beginAcceptInvite({ secret, name });
      const credentialJson = await createPasskey(begin.optionsJson);
      await client.finishAcceptInvite({ ceremonyId: begin.ceremonyId, secret, credentialJson, passkeyName: device });
      await refreshAuth();
      // The invite is spent: Back mustn't return to it.
      await navigate({ to: "/", replace: true });
    } catch (err) {
      setProblem(err);
    } finally {
      setBusy(false);
    }
  };

  return (
    <StandalonePage title={t`Accept an invite`}>
      <header className="standalone-head">
        <h1 className="dw-text-title">
          <Trans>
            {invite.invitedBy} invited you as {where}
          </Trans>
        </h1>
        <p className="standalone-lead">
          {invite.expireTime ? (
            <Trans>
              For {invite.email}, until {formatDateTime(timestampDate(invite.expireTime), i18n.locale)}. You sign in
              with a passkey this browser, phone or security key makes for this address.
            </Trans>
          ) : null}
        </p>
      </header>
      <form className="auth-form" onSubmit={(e) => void submit(e)} noValidate>
        <Panel title={t`Your account`}>
          <TextField
            label={t`Your name`}
            value={name}
            onChange={(e) => setName(e.target.value)}
            autoComplete="name"
            size="lg"
            error={invalid}
          />
          {problem ? (
            <Callout tone="critical" title={t`Your account wasn't created`}>
              {explain(problem)}
            </Callout>
          ) : null}
          {canUsePasskeys ? null : (
            <Callout tone="warning" title={t`Passkeys don't work in this browser`}>
              <Trans>Open the link in a browser that makes passkeys, such as a recent Chrome, Safari or Firefox.</Trans>
            </Callout>
          )}
          <div className="auth-actions">
            <Button type="submit" variant="primary" size="lg" icon="key" loading={busy} disabled={!canUsePasskeys}>
              {busy ? t`Creating passkey` : t`Create passkey and join`}
            </Button>
          </div>
        </Panel>
      </form>
    </StandalonePage>
  );
}
