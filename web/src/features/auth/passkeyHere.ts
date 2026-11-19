// SPDX-License-Identifier: AGPL-3.0-only

import { createClient, type Transport } from "@connectrpc/connect";
import { AuthService } from "@dockwell/api";

import { reasonOf } from "../../lib/errors";

/**
 * What someone with no passkey for this address can do about it, as the
 * control plane says when asked to start adding one (#869):
 *   - add: they signed in just now in a way that needed no passkey here, by a
 *     code or a link, so the control plane began adding one with no step-up
 *     (ADR 0011's fresh sign-in). The ceremony it began is here, so the browser
 *     asks for the new passkey as the button is pressed.
 *   - sign-in-again: that sign-in was longer ago than 10 minutes, or they've
 *     added one since, so adding another here takes signing in again.
 */
export type PasskeyHere =
  | { state: "add"; ceremonyId: string; optionsJson: string; at: number }
  | { state: "sign-in-again" };

/** Asks the control plane which of the two it is, beginning to add one where it may. */
export async function askPasskeyHere(transport: Transport): Promise<PasskeyHere> {
  try {
    const begin = await createClient(AuthService, transport).beginAddPasskey({ proof: "" });
    return { state: "add", ceremonyId: begin.ceremonyId, optionsJson: begin.optionsJson, at: Date.now() };
  } catch (err) {
    if (reasonOf(err) === "needs_fresh_sign_in") return { state: "sign-in-again" };
    throw err;
  }
}

/**
 * Offers what PasskeyHere says, and settles true once a passkey for this
 * address was added, so what asked for a step-up can go on, or false once the
 * person turned it down or chose to sign in again.
 */
export type PasskeyHereOffer = (first: PasskeyHere) => Promise<boolean>;

let offer: PasskeyHereOffer | undefined;
let offering: Promise<boolean> | undefined;

/** Puts the panel's offer in place, for as long as it's shown, and gives back what takes it away. */
export function offerPasskeyHereWith(o: PasskeyHereOffer): () => void {
  offer = o;
  return () => {
    if (offer === o) offer = undefined;
  };
}

/**
 * Where a step-up was refused for want of a passkey for this address: offers to
 * add one, or to sign in again where that's what adding one takes, and settles
 * true once one was added. Two step-ups refused at once share one offer. With no
 * offer in place, or none that could be made, it settles false, and the refusal
 * stands.
 */
export function offerPasskeyHere(transport: Transport): Promise<boolean> {
  const o = offer;
  if (!o) return Promise.resolve(false);
  offering ??= askPasskeyHere(transport)
    .then(o)
    .catch(() => false)
    .finally(() => {
      offering = undefined;
    });
  return offering;
}
