// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate } from "@bufbuild/protobuf/wkt";
import { createClient } from "@connectrpc/connect";
import { useQuery, useTransport } from "@connectrpc/connect-query";
import { AuthService, SignInMethod } from "@dockwell/api";
import { Button, Callout } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { useState } from "react";

import { useTimeWords } from "../../lib/time";
import { passkeysAvailable } from "../../lib/webauthn";
import { useControlPlaneNow } from "../servers/useFleet";
import { TokensPointer } from "../settings/TokensPointer";
import { Page } from "../shell/Page";
import { useAuthError } from "./messages";
import { PasskeysPanel, useAddPasskey } from "./PasskeysPanel";
import { SessionsPanel } from "./SessionsPanel";
import { TotpPanel } from "./TotpPanel";
import { useSignedIn } from "./useAuthState";
import { useSignOut } from "./useSignOut";
import "./auth.css";
import "./ProfilePage.css";

/** How this visit arrived, when it needs a word: after a one-time link, or a recovery code. */
export type ProfileVia = "link" | "recovery-code";

/**
 * Your passkeys, authenticator app and sessions, and where your API tokens are, in the shell. Sign out sits
 * with the title on wide screens; on phones it is on the More tab, which
 * this page's header leads back to.
 */
export function ProfilePage({ via }: { via?: ProfileVia }) {
  const { t } = useLingui();
  const auth = useSignedIn();
  const signOut = useSignOut();
  const how = useSignedInWith(auth?.signInMethod ?? SignInMethod.UNSPECIFIED);
  const user = auth?.user;

  return (
    <Page
      title={t`Profile and security`}
      lead={user ? [user.name, user.email, how].filter(Boolean).join(" · ") : null}
      phoneSub={user?.email}
      actions={
        <Button size="sm" icon="logout" loading={signOut.busy} onClick={() => void signOut.run()}>
          <Trans>Sign out</Trans>
        </Button>
      }
      back="/settings"
    >
      {signOut.problem ? (
        <p className="profile-error" role="alert">
          {signOut.problem}
        </p>
      ) : null}
      <ProfileCallout via={via} rpId={auth?.rpId ?? ""} />
      <div className="profile-grid">
        <PasskeysPanel rpId={auth?.rpId ?? ""} />
        <TotpPanel />
        <SessionsPanel />
        <TokensPointer />
      </div>
    </Page>
  );
}

/** How this session signed in, after your name and email; empty for a way the panel doesn't know. */
function useSignedInWith(method: SignInMethod): string {
  const { t } = useLingui();
  switch (method) {
    case SignInMethod.CLAIM:
      return t`signed in by claiming this install`;
    case SignInMethod.PASSKEY:
      return t`signed in with a passkey`;
    case SignInMethod.AUTHENTICATOR_CODE:
      return t`signed in with an authenticator code`;
    case SignInMethod.RECOVERY_CODE:
      return t`signed in with a recovery code`;
    case SignInMethod.LINK:
      return t`signed in with a one-time link`;
    default:
      return "";
  }
}

/** At most one callout: the most pressing thing to do on this page. */
function ProfileCallout({ via, rpId }: { via?: ProfileVia; rpId: string }) {
  const { t } = useLingui();
  const words = useTimeWords();
  const now = useControlPlaneNow();
  const transport = useTransport();
  const passkeys = useQuery(AuthService.method.listPasskeys, {});
  const totp = useQuery(AuthService.method.getTotp, {});
  const add = useAddPasskey();
  const explain = useAuthError();
  const [unlocking, setUnlocking] = useState(false);
  const [unlockProblem, setUnlockProblem] = useState<string>();
  if (!passkeys.data || !totp.data) return null;
  const state = totp.data.totp;
  const usable = passkeys.data.passkeys.filter((p) => !p.suspectedCopy);
  const here = usable.filter((p) => p.rpId === rpId);
  const canAdd = passkeysAvailable();

  if (state?.lockTime) {
    const unlock = async () => {
      setUnlocking(true);
      setUnlockProblem(undefined);
      try {
        await createClient(AuthService, transport).unlockCodeSignIn({});
        await totp.refetch();
      } catch (err) {
        setUnlockProblem(explain(err));
      } finally {
        setUnlocking(false);
      }
    };
    const count = state.failedAttempts;
    const when = words.ago(timestampDate(state.lockTime), now);
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
        {unlockProblem ? <span className="profile-callout-error"> {unlockProblem}</span> : null}
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
          If you came here after adding a domain, run <code>dockwell recover</code> on the server: its link signs you in
          here, so you can add a passkey with no code.
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
