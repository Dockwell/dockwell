// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { useQuery, useTransport } from "@connectrpc/connect-query";
import { AuthService, SystemService } from "@dockwell/api";
import { Button, Callout, Logo, TextField } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { useNavigate } from "@tanstack/react-router";
import { type FormEvent, useEffect, useId, useState } from "react";

import { reasonOf } from "../../lib/errors";
import { useDocumentTitle } from "../../lib/useDocumentTitle";
import { getPasskey, PasskeyError, type PasskeyHint, passkeysAvailable } from "../../lib/webauthn";
import { CopyCommand } from "../system/CopyCommand";
import { normalizeCode } from "./code";
import { useAuthError } from "./messages";
import { Unclaimed } from "./Unclaimed";
import { safeNext, useAuthState, useRefreshAuth } from "./useAuthState";
import "./auth.css";
import "./SignInPage.css";

/** Signing in: a passkey first, a code from an authenticator app as the fallback. */
export function SignInPage({ next }: { next?: string }) {
  const auth = useAuthState();
  const version = useQuery(SystemService.method.getVersion, {});
  const [mode, setMode] = useState<"passkey" | "code">("passkey");
  // Set once a sign-in here succeeds: from then on, finishing it decides
  // where to go, and GoTo stays out of its way.
  const [signingIn, setSigningIn] = useState(false);
  const finish = useFinishSignIn(next, () => setSigningIn(true));

  if (auth.data?.user && !signingIn) {
    return <GoTo href={safeNext(next)} />;
  }
  if (auth.data && !auth.data.claimed) {
    return <Unclaimed serverName={auth.data.serverName} />;
  }
  // Empty until the control plane says, so a placeholder never flashes. The install's
  // name, once someone has given it one, and the server's until then, heads the page;
  // the way back in names the server, which is where to run it.
  const name = auth.data?.installName || auth.data?.serverName || "";
  const server = auth.data?.serverName ?? "";
  return (
    <div className="signin">
      <main className="signin-main">
        <div className="signin-column">
          <div className="signin-logo">
            <Logo variant="logotype" height={17} title="Dockwell" />
          </div>
          {mode === "passkey" ? (
            <PasskeySignIn
              name={name}
              finish={finish}
              onDomain={Boolean(auth.data?.domain) && auth.data?.rpId === auth.data?.domain}
              onUseCode={() => setMode("code")}
            />
          ) : (
            <CodeSignIn name={name} finish={finish} onUsePasskey={() => setMode("passkey")} />
          )}
          <section className="signin-recover" aria-labelledby="signin-recover-title">
            <div className="signin-recover-text">
              <h2 id="signin-recover-title">
                <Trans>Lost your passkey?</Trans>
              </h2>
              <p>
                {server ? (
                  <Trans>Run this on {server} over SSH. It prints a sign-in link that works once.</Trans>
                ) : (
                  <Trans>Run this on the server over SSH. It prints a sign-in link that works once.</Trans>
                )}
              </p>
            </div>
            <CopyCommand command="dockwell recover" />
          </section>
        </div>
      </main>
      {version.data?.build ? (
        <footer className="signin-footer">
          <span>
            <Trans>
              Version <code>{version.data.build.version}</code>
            </Trans>
          </span>
        </footer>
      ) : null}
    </div>
  );
}

/** Already signed in on arrival: on to where the visit was going. */
function GoTo({ href }: { href: string }) {
  const navigate = useNavigate();
  useEffect(() => {
    void navigate({ href, replace: true });
  }, [navigate, href]);
  return null;
}

function SignInHead({ name }: { name: string }) {
  const { t } = useLingui();
  useDocumentTitle(t`Sign in`);
  return (
    <div className="signin-head">
      <h1 id="signin-title" className="dw-text-title">
        <Trans>Sign in to Dockwell</Trans>
      </h1>
      <p>
        {name ? (
          <Trans>
            <code>{window.location.host}</code> · the panel for {name}
          </Trans>
        ) : (
          <code>{window.location.host}</code>
        )}
      </p>
    </div>
  );
}

/**
 * After a sign-in succeeds: reads the sign-in state again, then goes where
 * the visit was headed, or to the profile after a recovery code, which is
 * worth a look at what's left. onStart runs first, so the page knows not to
 * send the visit anywhere itself once the new state arrives.
 */
type FinishSignIn = (usedRecoveryCode?: boolean) => Promise<void>;

function useFinishSignIn(next: string | undefined, onStart: () => void): FinishSignIn {
  const navigate = useNavigate();
  const refreshAuth = useRefreshAuth();
  return async (usedRecoveryCode = false) => {
    onStart();
    await refreshAuth();
    if (usedRecoveryCode) await navigate({ to: "/profile", search: { via: "recovery-code" }, replace: true });
    else await navigate({ href: safeNext(next), replace: true });
  };
}

interface PasskeySignInProps {
  name: string;
  finish: FinishSignIn;
  /** True on the panel's domain, where passkeys made through the tunnel don't work. */
  onDomain: boolean;
  onUseCode: () => void;
}

function PasskeySignIn({ name, finish, onDomain, onUseCode }: PasskeySignInProps) {
  const { t } = useLingui();
  const transport = useTransport();
  const explain = useAuthError();
  const [busy, setBusy] = useState<PasskeyHint | undefined>();
  const [problem, setProblem] = useState<unknown>();
  const canUsePasskeys = passkeysAvailable();

  const signIn = async (hint: PasskeyHint) => {
    setBusy(hint);
    setProblem(undefined);
    try {
      const client = createClient(AuthService, transport);
      const begin = await client.beginPasskeySignIn({});
      const hints: PasskeyHint[] = hint === "client-device" ? ["client-device"] : ["security-key", "hybrid"];
      const credentialJson = await getPasskey(begin.optionsJson, hints);
      await client.finishPasskeySignIn({ ceremonyId: begin.ceremonyId, credentialJson });
      await finish();
    } catch (err) {
      setProblem(err);
    } finally {
      setBusy(undefined);
    }
  };

  const cancelled = problem instanceof PasskeyError && problem.problem === "cancelled";
  return (
    <section className="dw-panel signin-panel" aria-labelledby="signin-title">
      <SignInHead name={name} />
      {problem ? (
        <Callout tone={cancelled ? "info" : "critical"} title={cancelled ? undefined : t`You're not signed in`}>
          {cancelled && onDomain ? (
            <Trans>
              No passkey for this address? Passkeys belong to the address they were made on, so one made through the SSH
              tunnel doesn't work here. Sign in with a code, or run <code>dockwell recover</code> on the server for a
              one-time link, then add a passkey here.
            </Trans>
          ) : (
            explain(problem)
          )}
        </Callout>
      ) : null}
      {canUsePasskeys ? null : (
        <Callout tone="warning" title={t`Passkeys don't work at this address`}>
          <Trans>
            Open the panel through your SSH tunnel at <code>http://localhost</code>, or on its domain over HTTPS. You
            can still sign in with a code.
          </Trans>
        </Callout>
      )}
      <div className="signin-buttons">
        <Button
          variant="primary"
          size="lg"
          icon="key"
          loading={busy === "client-device"}
          disabled={!canUsePasskeys || busy !== undefined}
          onClick={() => void signIn("client-device")}
        >
          <Trans>Sign in with a passkey</Trans>
        </Button>
        <Button
          size="lg"
          loading={busy === "security-key"}
          disabled={!canUsePasskeys || busy !== undefined}
          onClick={() => void signIn("security-key")}
        >
          <Trans>Use a security key or another device</Trans>
        </Button>
        <Button variant="ghost" size="sm" className="signin-switch" onClick={onUseCode}>
          <Trans>Use a code from your authenticator app</Trans>
        </Button>
      </div>
    </section>
  );
}

function CodeSignIn({ name, finish, onUsePasskey }: { name: string; finish: FinishSignIn; onUsePasskey: () => void }) {
  const { t } = useLingui();
  const transport = useTransport();
  const explain = useAuthError();
  const formId = useId();
  const [email, setEmail] = useState("");
  const [code, setCode] = useState("");
  const [busy, setBusy] = useState(false);
  const [problem, setProblem] = useState<unknown>();
  const [invalid, setInvalid] = useState<{ email?: string; code?: string }>({});

  const submit = async (event: FormEvent) => {
    event.preventDefault();
    const typed = normalizeCode(code);
    const next = {
      email: /^[^\s@]+@[^\s@]+$/.test(email.trim()) ? undefined : t`Enter the email of your account.`,
      code: typed ? undefined : t`Enter the 6-digit code from your authenticator app, or a recovery code.`,
    };
    setInvalid(next);
    if (next.email || !typed) return;
    setBusy(true);
    setProblem(undefined);
    try {
      // The code as checked here, so the control plane never refuses one the panel let through.
      const res = await createClient(AuthService, transport).signInWithCode({ email, code: typed.code });
      await finish(res.usedRecoveryCode);
    } catch (err) {
      setProblem(err);
      if (reasonOf(err) === "code_rejected" || reasonOf(err) === "code_reused") setCode("");
    } finally {
      setBusy(false);
    }
  };

  return (
    <section className="dw-panel signin-panel" aria-labelledby="signin-title">
      <SignInHead name={name} />
      {problem ? (
        <Callout tone="critical" title={t`You're not signed in`}>
          {explain(problem)}
        </Callout>
      ) : null}
      <form id={formId} className="signin-form" onSubmit={(e) => void submit(e)} noValidate>
        <TextField
          label={t`Email`}
          type="email"
          value={email}
          onChange={(e) => setEmail(e.target.value)}
          autoComplete="username"
          size="lg"
          error={invalid.email}
        />
        <TextField
          label={t`Code`}
          value={code}
          onChange={(e) => setCode(e.target.value)}
          autoComplete="one-time-code"
          inputMode="text"
          mono
          size="lg"
          error={invalid.code}
          hint={t`6 digits from your authenticator app, or one of your recovery codes.`}
        />
        <div className="signin-buttons">
          <Button type="submit" variant="primary" size="lg" loading={busy}>
            {busy ? t`Signing in` : t`Sign in`}
          </Button>
          <Button variant="ghost" size="sm" className="signin-switch" onClick={onUsePasskey}>
            <Trans>Use a passkey instead</Trans>
          </Button>
        </div>
      </form>
    </section>
  );
}
