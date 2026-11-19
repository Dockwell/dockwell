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
import { CopyCommand } from "../system/CopyCommand";
import { useDeviceName } from "./device";
import { useAuthError } from "./messages";
import { askPasskeyHere, offerPasskeyHereWith, type PasskeyHere } from "./passkeyHere";
import { useSignedIn } from "./useAuthState";
import { useSignInAgain } from "./useSignInAgain";
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
 * the code form, then comes back here; or, for someone with no code, it gives
 * their own recover command. Passkeys stay what every step-up takes.
 *
 * The ceremony the button uses is begun before the click, so the browser's prompt
 * opens straight from it, as Safari wants. It's begun again before it ends, and at
 * once after a save the control plane refused, as a ceremony works once (#869's
 * review, M1).
 */
export function PasskeyHerePrompt() {
  const { t } = useLingui();
  const transport = useTransport();
  const queryClient = useQueryClient();
  const auth = useSignedIn();
  const device = useDeviceName();
  const explain = useAuthError();
  const [shown, setShown] = useState<PasskeyHere>();
  const [busy, setBusy] = useState(false);
  const [problem, setProblem] = useState<string>();
  const settle = useRef<(added: boolean) => void>(undefined);
  const again = useSignInAgain(shown?.state === "sign-in-again");
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

  /** Begins the ceremony again, for an offer still showing one: the next click opens the browser's prompt with it. */
  const renew = () =>
    void askPasskeyHere(transport)
      .then((next) => setShown((now) => (now?.state === "add" ? next : now)))
      .catch(() => {});

  // Before the ceremony ends on the control plane's side, a new one.
  useEffect(() => {
    if (shown?.state !== "add") return;
    const timer = setTimeout(renew, Math.max(0, CEREMONY_GOOD_MS - (Date.now() - shown.at)));
    return () => clearTimeout(timer);
  });

  const done = (added: boolean) => {
    settle.current?.(added);
    settle.current = undefined;
    setShown(undefined);
  };

  const add = async () => {
    if (shown?.state !== "add") return;
    setBusy(true);
    setProblem(undefined);
    let spent = false;
    try {
      const credentialJson = await createPasskey(shown.optionsJson);
      spent = true;
      await createClient(AuthService, transport).finishAddPasskey({
        ceremonyId: shown.ceremonyId,
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
      // The control plane takes a ceremony once, whatever it answers.
      if (spent) renew();
    } finally {
      setBusy(false);
    }
  };

  const signInAgain = async () => {
    if (await again.run()) done(false);
  };

  if (!shown) return null;
  const fresh = shown.state === "add";
  // Where they do work: a passkey here flagged as copied works nowhere, so it isn't one (#869's review, L3).
  const elsewhere = (passkeys.data?.passkeys ?? []).filter((p) => p.rpId !== auth?.rpId);
  const withCode = !fresh && again.codes !== false;
  return (
    <Sheet
      open
      width={440}
      title={t`No passkey for this address`}
      onClose={() => done(false)}
      footer={
        <>
          <Button variant="ghost" onClick={() => done(false)}>
            {fresh || withCode ? <Trans>Cancel</Trans> : <Trans>Done</Trans>}
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
          ) : withCode ? (
            <Button
              variant="primary"
              icon="key"
              loading={again.busy || again.codes === undefined}
              onClick={() => void signInAgain()}
            >
              <Trans>Sign in again with your code</Trans>
            </Button>
          ) : null}
        </>
      }
    >
      <div className="passkey-here">
        <p>
          {fresh
            ? t`Your passkeys work only on the address they were made on.`
            : t`Adding one here takes a sign-in from the last 10 minutes.`}
        </p>
        {problem ? <Callout tone="critical">{problem}</Callout> : null}
        {/* Fact, consequence, fix (#869's review, L1). */}
        {!fresh && again.problem ? (
          <Callout tone="critical" title={t`Couldn't sign out`}>
            {t`You're still signed in here. ${again.problem}`}
          </Callout>
        ) : null}
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
          {!fresh && again.codes === false ? (
            <>
              <dt>{t`Sign in again`}</dt>
              <dd className="passkey-here-recover">
                <CopyCommand command={again.recover} />
                <span>{t`On the control server, it prints a link that signs you in here.`}</span>
              </dd>
            </>
          ) : null}
        </dl>
      </div>
    </Sheet>
  );
}
