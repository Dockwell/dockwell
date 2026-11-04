// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { useQuery, useTransport } from "@connectrpc/connect-query";
import { AuthService, SignInMethod } from "@dockwell/api";
import { Button, Callout, Logo } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link } from "@tanstack/react-router";
import { useState } from "react";

import { passkeysAvailable } from "../../lib/webauthn";
import { PasskeysPanel, useAddPasskey } from "./PasskeysPanel";
import { SessionsPanel } from "./SessionsPanel";
import { TotpPanel } from "./TotpPanel";
import { useAgo } from "./time";
import { useAuthState, useRefreshAuth } from "./useAuthState";
import "./auth.css";
import "./ProfilePage.css";

/** How this visit arrived, when it needs a word: after a one-time link, or a recovery code. */
export type ProfileVia = "link" | "recovery-code";

/**
 * Your passkeys, authenticator app and sessions. It stands on its own until
 * the panel shell lands; the mock (Profile.dc.html) puts it under Account.
 */
export function ProfilePage({ via }: { via?: ProfileVia }) {
  const { t } = useLingui();
  const auth = useAuthState();
  const transport = useTransport();
  const refreshAuth = useRefreshAuth();
  const [signingOut, setSigningOut] = useState(false);
  const how = useSignedInWith(auth.data?.signInMethod ?? SignInMethod.UNSPECIFIED);

  const signOut = async () => {
    setSigningOut(true);
    try {
      await createClient(AuthService, transport).signOut({});
    } finally {
      setSigningOut(false);
      await refreshAuth();
    }
  };

  return (
    <div className="profile">
      <header className="profile-bar">
        <Link to="/" className="profile-home" aria-label={t`Go to the overview`}>
          <Logo variant="logotype" height={14} title="Dockwell" />
        </Link>
        <Button size="sm" variant="ghost" icon="logout" loading={signingOut} onClick={() => void signOut()}>
          <Trans>Sign out</Trans>
        </Button>
      </header>
      <main className="profile-main">
        <div className="profile-column">
          <div className="profile-head">
            <h1 className="dw-text-title">
              <Trans>Profile and security</Trans>
            </h1>
            <p>
              {auth.data?.user ? (
                <Trans>
                  {auth.data.user.name} · {auth.data.user.email} · signed in {how}
                </Trans>
              ) : null}
            </p>
          </div>
          <ProfileCallout via={via} rpId={auth.data?.rpId ?? ""} />
          <div className="profile-grid">
            <PasskeysPanel rpId={auth.data?.rpId ?? ""} />
            <TotpPanel />
            <SessionsPanel />
          </div>
        </div>
      </main>
    </div>
  );
}

function useSignedInWith(method: SignInMethod): string {
  const { t } = useLingui();
  switch (method) {
    case SignInMethod.CLAIM:
      return t({ message: "by claiming this install", comment: "Ends “signed in …”" });
    case SignInMethod.PASSKEY:
      return t({ message: "with a passkey", comment: "Ends “signed in …”" });
    case SignInMethod.AUTHENTICATOR_CODE:
      return t({ message: "with an authenticator code", comment: "Ends “signed in …”" });
    case SignInMethod.RECOVERY_CODE:
      return t({ message: "with a recovery code", comment: "Ends “signed in …”" });
    case SignInMethod.LINK:
      return t({ message: "with a one-time link", comment: "Ends “signed in …”" });
    default:
      return "";
  }
}

/** At most one callout: the most pressing thing to do on this page. */
function ProfileCallout({ via, rpId }: { via?: ProfileVia; rpId: string }) {
  const { t } = useLingui();
  const ago = useAgo();
  const transport = useTransport();
  const passkeys = useQuery(AuthService.method.listPasskeys, {});
  const totp = useQuery(AuthService.method.getTotp, {});
  const add = useAddPasskey();
  const [unlocking, setUnlocking] = useState(false);
  if (!passkeys.data || !totp.data) return null;
  const state = totp.data.totp;
  const usable = passkeys.data.passkeys.filter((p) => !p.suspectedCopy);
  const here = usable.filter((p) => p.rpId === rpId);
  const canAdd = passkeysAvailable();

  if (state?.lockTime) {
    const unlock = async () => {
      setUnlocking(true);
      try {
        await createClient(AuthService, transport).unlockCodeSignIn({});
        await totp.refetch();
      } finally {
        setUnlocking(false);
      }
    };
    const count = state.failedAttempts;
    const when = ago(state.lockTime);
    return (
      <Callout
        tone="critical"
        title={t`Code sign-in is locked`}
        actions={
          <Button size="sm" loading={unlocking} onClick={() => void unlock()}>
            <Trans>Unlock</Trans>
          </Button>
        }
      >
        <Trans>
          Someone entered {count} wrong codes in a row for your account, the last {when}. Unlock code sign-in only if
          that was you. Otherwise someone may know your email; your passkeys and recovery codes still work.
        </Trans>
      </Callout>
    );
  }
  const addButton =
    canAdd && rpId ? (
      <Button size="sm" icon="plus" loading={add.busy} onClick={() => void add.run()}>
        <Trans>Add passkey</Trans>
      </Button>
    ) : null;
  if (via === "link") {
    return (
      <Callout tone="info" title={t`You signed in with a one-time link`} actions={addButton}>
        {here.length === 0 ? (
          <Trans>Add a passkey for {rpId} now, so you can sign in here next time without another link.</Trans>
        ) : (
          <Trans>If you lost a passkey, add a new one now, then remove the lost one below.</Trans>
        )}
        {add.problem ? <span className="profile-callout-error"> {add.problem}</span> : null}
      </Callout>
    );
  }
  if (via === "recovery-code" && state) {
    const left = state.recoveryCodesLeft;
    return (
      <Callout tone="warning" title={t`You used a recovery code`}>
        <Trans>
          {left} of 10 are left, and each works once. If you lost your authenticator app, set it up again below, which
          also makes new recovery codes.
        </Trans>
      </Callout>
    );
  }
  if (rpId && here.length === 0) {
    return (
      <Callout tone="info" title={t`No passkey works at ${rpId} yet`} actions={addButton}>
        <Trans>
          Passkeys belong to the address they were made on. Add one for {rpId} so you can sign in here without a code.
        </Trans>
        {add.problem ? <span className="profile-callout-error"> {add.problem}</span> : null}
      </Callout>
    );
  }
  if (usable.length + (state?.enabled ? 1 : 0) < 2) {
    return (
      <Callout tone="info" title={t`Add a second way to sign in`}>
        <Trans>
          If you lose the device with your only passkey, you'd need <code>dockwell recover</code> on the server. Add
          another passkey, or an authenticator app.
        </Trans>
      </Callout>
    );
  }
  return null;
}
