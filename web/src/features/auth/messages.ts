// SPDX-License-Identifier: AGPL-3.0-only

import { useLingui } from "@lingui/react/macro";
import { useCallback } from "react";

import { reasonOf, retryAfter, sentence } from "../../lib/errors";
import { useTimeWords } from "../../lib/time";
import { PasskeyError } from "../../lib/webauthn";

/**
 * "32 s", "2 min": a wait the way the product writes durations, rounded up
 * to the minute from a minute on, so trying again after it works.
 */
function useWait() {
  const words = useTimeWords();
  return useCallback(
    (seconds: number) => words.span(seconds < 60 ? seconds * 1_000 : Math.ceil(seconds / 60) * 60_000),
    [words],
  );
}

/**
 * Says what went wrong with a sign-in, a claim or a passkey, in the panel's
 * language. Failures the control plane names get the panel's own words;
 * anything else falls back to the control plane's message.
 */
export function useAuthError({ claimCommand = "dockwell claim" }: { claimCommand?: string } = {}) {
  const { t } = useLingui();
  const wait = useWait();
  return useCallback(
    (error: unknown): string => {
      if (error instanceof PasskeyError) {
        switch (error.problem) {
          case "cancelled":
            return t`The passkey prompt closed before it finished. Try again when you're ready.`;
          case "exists":
            return t`This device already has a passkey for your account here. Use it, or pick another device or security key.`;
          case "unsupported":
            return t`This browser can't use passkeys at this address. Open the panel through your SSH tunnel at http://localhost, or on its domain over HTTPS.`;
          default:
            return t`The browser couldn't finish with the passkey. Try again, or use another device.`;
        }
      }
      switch (reasonOf(error)) {
        case "unauthenticated":
          return t`You're not signed in. Sign in, then try again.`;
        case "session_ended":
          return t`Your session ended, so this didn't happen. Sign in again, then try again.`;
        case "origin":
          return t`The control plane refused this because it didn't come from the panel's own address. Open the panel at its usual address and try there.`;
        case "misdirected":
          return t`The control plane doesn't answer at this address. Open the panel through your SSH tunnel at http://localhost, or on its domain.`;
        case "unavailable":
          return t`The control plane couldn't check your session just now. Try again in a moment.`;
        case "token_rejected":
          return t`That API token is unknown, revoked or expired. Check you copied all of it, or create a new one under Profile and security.`;
        case "token_scope":
          return t`That API token is read-only, so it can't make changes. Create a read-write token under Profile and security.`;
        case "needs_session":
          return t`API tokens can't remove data, cut off access, reveal a secret, reach a server's files or terminal, or change how you sign in and your tokens. Sign in and do it in the panel.`;
        case "needs_step_up":
          return t`That needs your passkey just now, and the confirmation was missing, used or expired. Try again, and confirm when your browser asks.`;
        case "no_passkey_here":
          return t`Confirming this needs a passkey for this address, and you have none here. Add one under Profile and security, then try again.`;
        case "needs_fresh_sign_in":
          return t`Adding a passkey needs one you have for this address, and you have none here. Sign out, sign in again with a code, a recovery code or a link from dockwell recover, then add it within 10 minutes.`;
        case "passkey_rejected":
          return t`That passkey isn't registered for this address, or it couldn't be verified. Try another passkey, or sign in with a code.`;
        case "passkey_copied":
          return t`That passkey reported a lower use count than before, which happens when a passkey is copied, so it no longer signs in. Sign in another way and remove it under Profile and security.`;
        case "passkey_exists":
          return t`That passkey is already registered. Use it to sign in, or pick another device or security key.`;
        case "code_rejected":
          return t`That code didn't work. Check the email and the code, then try again.`;
        case "code_reused":
          return t`That code was already used. Wait for the next one in your authenticator app.`;
        case "codes_locked":
          return t`Code sign-in is locked after 10 wrong codes in a row. Sign in with a passkey or a recovery code, or run dockwell recover on the server, then unlock it under Profile and security.`;
        case "slow_down": {
          const seconds = retryAfter(error);
          return seconds
            ? t`Too many wrong codes. Wait ${wait(seconds)}, then try again.`
            : t`Too many wrong codes. Wait a moment, then try again.`;
        }
        case "too_many_requests":
          return t`Too many requests from your address. Wait a minute, then try again.`;
        case "claim_link_rejected":
          return t`This claim link is invalid, already used, replaced by a newer one, or expired. Run ${claimCommand} on the server for a new link.`;
        case "sign_in_link_rejected":
          return t`This sign-in link is invalid, already used, replaced by a newer one, or expired. Run dockwell recover on the server for a new link.`;
        case "claimed":
          return t`This install already has an owner. Sign in instead.`;
        case "ceremony_expired":
          return t`The passkey request expired. Try again.`;
        case "last_way_in":
          return t`That would leave you no way to sign in. Add another passkey or an authenticator app first.`;
        case "needs_hostname":
          return t`Passkeys need a hostname, not an IP address. Open the panel at http://localhost on the same port.`;
        case "no_pending_totp":
          return t`This setup of an authenticator app has ended: it works for 10 minutes, in the session that began it. Close this and set it up again.`;
        case "totp_setup_ended":
          return t`That was the last try. 5 wrong codes ended this setup, and its key no longer works. Close this and set it up again, which asks for your passkey.`;
        case "totp_mismatch":
          return t`That code doesn't match. Check that your phone's clock is right, then enter the current code.`;
        default:
          return sentence(error) || t`Something went wrong. Try again.`;
      }
    },
    [t, wait, claimCommand],
  );
}
