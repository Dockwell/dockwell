// SPDX-License-Identifier: AGPL-3.0-only

import { Button, Callout } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import type { ReactNode } from "react";

import { passkeysAvailable } from "../../lib/webauthn";
import { CopyCommand } from "../system/CopyCommand";
import { useAddPasskey } from "./PasskeysPanel";
import { useSignInAgain } from "./useSignInAgain";

/**
 * Where an action took a passkey and there's none for this address: the state, and
 * the button that adds one here, which then does what was asked (onAdded). Adding
 * one with none here needs a sign-in just now by code or link (ADR 0011); when that's
 * what's missing, the button signs out, and sign-in, on the code form, brings the
 * person back here; someone with no code gets their recover command instead. It says
 * what the shared step-up's offer says (#869).
 */
export function AddPasskeyHere({ title, onAdded }: { title?: ReactNode; onAdded?: () => void }) {
  const { t } = useLingui();
  const add = useAddPasskey();
  const fresh = add.reason === "needs_fresh_sign_in";
  const again = useSignInAgain(fresh);
  return (
    <Callout
      tone="warning"
      title={title ?? t`No passkey for this address yet`}
      actions={
        !fresh ? (
          <Button
            size="sm"
            variant="primary"
            icon="key"
            loading={add.busy}
            disabled={!passkeysAvailable()}
            onClick={() => void add.run(onAdded)}
          >
            <Trans>Add a passkey for this address</Trans>
          </Button>
        ) : again.codes !== false ? (
          <Button
            size="sm"
            variant="primary"
            icon="key"
            loading={again.busy || again.codes === undefined}
            onClick={() => void again.run()}
          >
            <Trans>Sign in again with your code</Trans>
          </Button>
        ) : undefined
      }
    >
      {!fresh ? (
        (add.problem ?? null)
      ) : (
        <>
          {t`Adding one here takes a sign-in from the last 10 minutes.`}
          {again.problem ? ` ${t`Couldn't sign out, so you're still signed in here. ${again.problem}`}` : null}
          {again.codes === false ? <CopyCommand command={again.recover} /> : null}
        </>
      )}
    </Callout>
  );
}
