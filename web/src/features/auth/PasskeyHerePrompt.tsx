// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { createConnectQueryKey, useQuery, useTransport } from "@connectrpc/connect-query";
import { AuthService } from "@dockwell/api";
import { Button, Callout, Sheet } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { useQueryClient } from "@tanstack/react-query";
import { useEffect, useRef, useState } from "react";

import { reasonOf } from "../../lib/errors";
import { createPasskey, PasskeyError, passkeysAvailable } from "../../lib/webauthn";
import { useDeviceName } from "./device";
import { useAuthError } from "./messages";
import { askPasskeyHere, offerPasskeyHereWith, type PasskeyHere } from "./passkeyHere";
import { signInAgainWithCode } from "./signInAgain";
import { useSignedIn } from "./useAuthState";
import { useSignOut } from "./useSignOut";
import "./PasskeyHerePrompt.css";

/** How long a ceremony the control plane began stays good, less a margin: 5 minutes on its side. */
const CEREMONY_GOOD_MS = 4 * 60_000;

/**
 * What every screen offers where a step-up was refused for want of a passkey for
 * this address, mounted once for the signed-in panel (#869). Passkeys belong to
 * the address they were made on, so one made through the SSH tunnel doesn't
 * work at the panel's domain.
 *
 * Within 10 minutes of a sign-in that needed no passkey here, by a code or a
 * link, it adds one, and the step-up that asked goes on with it: Add server ends
 * showing its command. Past them, it signs out, and the sign-in screen opens on
 * the code form, then comes back here. Passkeys stay what every step-up takes.
 */
export function PasskeyHerePrompt() {
  const { t } = useLingui();
  const transport = useTransport();
  const queryClient = useQueryClient();
  const auth = useSignedIn();
  const device = useDeviceName();
  const explain = useAuthError();
  const signOut = useSignOut();
  const [shown, setShown] = useState<PasskeyHere>();
  const [busy, setBusy] = useState(false);
  const [problem, setProblem] = useState<string>();
  const settle = useRef<(added: boolean) => void>(undefined);
  // Where their passkeys do work, read only while it's shown.
  const passkeys = useQuery(AuthService.method.listPasskeys, {}, { enabled: shown !== undefined });

  useEffect(() => {
    const stop = offerPasskeyHereWith(
      (first) =>
        new Promise<boolean>((resolve) => {
          settle.current = resolve;
          setProblem(undefined);
          setShown(first);
        }),
    );
    return () => {
      stop();
      // Gone, as on signing out: what asked hears no.
      settle.current?.(false);
      settle.current = undefined;
    };
  }, []);

  const done = (added: boolean) => {
    settle.current?.(added);
    settle.current = undefined;
    setShown(undefined);
  };

  const add = async () => {
    if (shown?.state !== "add") return;
    setBusy(true);
    setProblem(undefined);
    try {
      // A ceremony begun long enough ago may have ended on the control plane's side.
      const ceremony = Date.now() - shown.at < CEREMONY_GOOD_MS ? shown : await askPasskeyHere(transport);
      if (ceremony.state !== "add") {
        setShown(ceremony);
        return;
      }
      const credentialJson = await createPasskey(ceremony.optionsJson);
      await createClient(AuthService, transport).finishAddPasskey({
        ceremonyId: ceremony.ceremonyId,
        credentialJson,
        name: device,
      });
      void queryClient.invalidateQueries({
        queryKey: createConnectQueryKey({ schema: AuthService.method.listPasskeys, cardinality: undefined }),
      });
      done(true);
    } catch (err) {
      if (err instanceof PasskeyError && err.problem === "cancelled") return;
      if (reasonOf(err) === "needs_fresh_sign_in") {
        setShown({ state: "sign-in-again" });
        return;
      }
      setProblem(explain(err));
    } finally {
      setBusy(false);
    }
  };

  const again = async () => {
    signInAgainWithCode(auth?.user?.email ?? "");
    done(false);
    await signOut.run();
  };

  if (!shown) return null;
  const fresh = shown.state === "add";
  const elsewhere = passkeys.data?.passkeys ?? [];
  return (
    <Sheet
      open
      width={440}
      title={t`No passkey for this address`}
      subtitle={auth?.rpId || undefined}
      onClose={() => done(false)}
      footer={
        <>
          <Button variant="ghost" onClick={() => done(false)}>
            <Trans>Cancel</Trans>
          </Button>
          {fresh ? (
            <Button
              variant="primary"
              icon="key"
              loading={busy}
              disabled={!passkeysAvailable()}
              onClick={() => void add()}
            >
              <Trans>Add a passkey for this address</Trans>
            </Button>
          ) : (
            <Button variant="primary" icon="key" loading={signOut.busy} onClick={() => void again()}>
              <Trans>Sign in again with your code</Trans>
            </Button>
          )}
        </>
      }
    >
      <div className="passkey-here">
        <p>
          {fresh
            ? t`Your passkeys work only on the address they were made on.`
            : t`Adding one here takes a sign-in from the last 10 minutes.`}
        </p>
        {(problem ?? signOut.problem) ? <Callout tone="critical">{problem ?? signOut.problem}</Callout> : null}
        <dl className="passkey-here-facts">
          <dt>{t`This address`}</dt>
          <dd>
            <code>{auth?.rpId}</code>
          </dd>
          {elsewhere.length > 0 ? (
            <>
              <dt>{t`Your passkeys`}</dt>
              <dd>
                <ul>
                  {elsewhere.map((p) => (
                    <li key={p.id}>
                      {p.name || t`Passkey`} · <code>{p.rpId}</code>
                    </li>
                  ))}
                </ul>
              </dd>
            </>
          ) : null}
        </dl>
      </div>
    </Sheet>
  );
}
