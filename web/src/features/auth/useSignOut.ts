// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import { AuthService } from "@dockwell/api";
import { useState } from "react";

import { isSessionEnded } from "../../lib/errors";
import { useAuthError } from "./messages";
import { useRefreshAuth } from "./useAuthState";

/**
 * Signs this browser out, then lets the gate show sign-in. A failure stays
 * signed in and says why in problem; a session that had already ended
 * counts as signed out. run settles true once signed out.
 */
export function useSignOut() {
  const transport = useTransport();
  const refreshAuth = useRefreshAuth();
  const explain = useAuthError();
  const [busy, setBusy] = useState(false);
  const [problem, setProblem] = useState<string>();
  const run = async (): Promise<boolean> => {
    setBusy(true);
    setProblem(undefined);
    try {
      await createClient(AuthService, transport).signOut({});
      await refreshAuth();
      return true;
    } catch (err) {
      if (isSessionEnded(err)) {
        await refreshAuth();
        return true;
      }
      setProblem(explain(err));
      return false;
    } finally {
      setBusy(false);
    }
  };
  return { busy, problem, run };
}
