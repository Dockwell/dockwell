// SPDX-License-Identifier: AGPL-3.0-only

import { createClient, type Transport } from "@connectrpc/connect";
import { AuthService, type StepUpAction } from "@dockwell/api";

import { reasonOf } from "../../lib/errors";
import { getPasskey } from "../../lib/webauthn";

/**
 * Confirms action on target with a passkey you have for this address, just
 * now (a step-up): the browser asks for it, and the control plane answers
 * with a proof that works once, within a minute, in this session, for that
 * action alone. Revealing a secret, changing how you sign in and making an
 * API token each take one (docs/adr/0011-claim-and-sign-in.md).
 */
export async function stepUp(transport: Transport, action: StepUpAction, target = ""): Promise<string> {
  const auth = createClient(AuthService, transport);
  const begin = await auth.beginStepUp({ action, target });
  const credentialJson = await getPasskey(begin.optionsJson);
  const { proof } = await auth.finishStepUp({ ceremonyId: begin.ceremonyId, credentialJson });
  return proof;
}

/**
 * Does something a step-up confirms, as every such action in the panel
 * does: it calls without a proof first, so the control plane refuses for
 * any other reason, such as the last way in or a missing name, before the
 * browser asks for a passkey. Only once it answers needs_step_up does the
 * browser ask, and the call go again with the proof. Adding a passkey just
 * after signing in another way may need none.
 */
export async function withStepUpIfAsked<T>(
  transport: Transport,
  action: StepUpAction,
  target: string,
  call: (proof: string) => Promise<T>,
): Promise<T> {
  try {
    return await call("");
  } catch (err) {
    if (reasonOf(err) !== "needs_step_up") throw err;
  }
  return call(await stepUp(transport, action, target));
}
