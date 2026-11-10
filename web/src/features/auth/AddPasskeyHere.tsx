// SPDX-License-Identifier: AGPL-3.0-only

import { Button, Callout } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import type { ReactNode } from "react";

import { passkeysAvailable } from "../../lib/webauthn";
import { useAddPasskey } from "./PasskeysPanel";
import { useSignOut } from "./useSignOut";

/**
 * Where an action took a passkey and there's none for this address: the state, and
 * the button that adds one here, which then does what was asked (onAdded). Adding
 * one with none here needs a sign-in just now by code or link (ADR 0011); when that's
 * what's missing, the button signs out, and sign-in brings the person back here.
 */
export function AddPasskeyHere({ title, onAdded }: { title?: ReactNode; onAdded?: () => void }) {
  const { t } = useLingui();
  const add = useAddPasskey();
  const signOut = useSignOut();
  const fresh = add.reason === "needs_fresh_sign_in";
  return (
    <Callout
      tone="warning"
      title={title ?? t`No passkey for this address yet`}
      actions={
        fresh ? (
          <Button size="sm" variant="primary" icon="key" loading={signOut.busy} onClick={() => void signOut.run()}>
            <Trans>Sign in again</Trans>
          </Button>
        ) : (
          <Button
            size="sm"
            variant="primary"
            icon="key"
            loading={add.busy}
            disabled={!passkeysAvailable()}
            onClick={() => void add.run(onAdded)}
          >
            <Trans>Add a passkey here</Trans>
          </Button>
        )
      }
    >
      {fresh
        ? t`Adding one here needs a sign-in just now, with a code or a link.`
        : (signOut.problem ?? add.problem ?? null)}
    </Callout>
  );
}
