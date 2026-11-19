// SPDX-License-Identifier: AGPL-3.0-only

import { useQuery } from "@connectrpc/connect-query";
import { AuthService } from "@dockwell/api";

import { signedInAgain, signInAgainWithCode } from "./signInAgain";
import { useSignedIn } from "./useAuthState";
import { useSignOut } from "./useSignOut";

/**
 * How someone signs in again, as adding a passkey for this address takes once their
 * sign-in is older than 10 minutes (#869). With code sign-in on for them, an
 * authenticator code or a recovery code, the panel signs them out and opens the code
 * form with their email in it (codes true). Without, a code form can't sign them in,
 * so they get their own way back in instead: recover, the command that prints a
 * sign-in link on the control server, which is a fresh sign-in too (codes false).
 * Codes is undefined while it's being read. A sign-in provider isn't offered: a
 * session from one isn't a fresh sign-in (ADR 0011), so it couldn't add a passkey
 * either (#869's review, M2).
 */
export function useSignInAgain(asking: boolean) {
  const auth = useSignedIn();
  const signOut = useSignOut();
  const totp = useQuery(AuthService.method.getTotp, {}, { enabled: asking, retry: false });
  const state = totp.data?.totp;
  const codes = totp.isError
    ? false
    : totp.data
      ? state?.enabled === true || (state?.recoveryCodesLeft ?? 0) > 0
      : undefined;
  /** Signs out to sign in again with the code. A sign-out that fails leaves nothing behind for the sign-in screen. */
  const run = async (): Promise<boolean> => {
    signInAgainWithCode(auth?.user?.email ?? "");
    const out = await signOut.run();
    if (!out) signedInAgain();
    return out;
  };
  return {
    codes,
    recover: auth?.recoverCommand || "dockwell recover",
    busy: signOut.busy,
    problem: signOut.problem,
    run,
  };
}
