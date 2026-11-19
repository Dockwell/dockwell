// SPDX-License-Identifier: AGPL-3.0-only

/**
 * How the sign-in screen opens after someone signs out to sign in again with
 * their code, as adding a passkey for this address can take (#869): on the code
 * form, with their email in it. It's kept in memory alone, until a sign-in
 * finishes, so a screen mounted twice reads it both times.
 */
interface SignInAgain {
  email: string;
}

let again: SignInAgain | undefined;

/** Has the sign-in screen open on the code form, with email in it, until a sign-in finishes. */
export function signInAgainWithCode(email: string): void {
  again = { email };
}

/** What signInAgainWithCode asked for, or nothing. */
export function signingInAgain(): SignInAgain | undefined {
  return again;
}

/** Forgets it, as a sign-in that finished does. */
export function signedInAgain(): void {
  again = undefined;
}
