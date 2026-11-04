// SPDX-License-Identifier: AGPL-3.0-only

import { Code, ConnectError } from "@connectrpc/connect";
import { describe, expect, it } from "vitest";

import { isSessionEnded } from "./errors";

const refused = (code: Code, reason?: string) =>
  new ConnectError("refused", code, reason ? new Headers({ "Dockwell-Error": reason }) : undefined);

describe("isSessionEnded", () => {
  it("is true only when there is no session, or it ended", () => {
    expect(isSessionEnded(refused(Code.Unauthenticated, "session_ended"))).toBe(true);
    expect(isSessionEnded(refused(Code.Unauthenticated, "unauthenticated"))).toBe(true);
    for (const reason of ["code_rejected", "code_reused", "passkey_rejected", "passkey_copied", undefined]) {
      expect(isSessionEnded(refused(Code.Unauthenticated, reason)), reason).toBe(false);
    }
    expect(isSessionEnded(refused(Code.PermissionDenied, "origin"))).toBe(false);
    expect(isSessionEnded(new Error("network down"))).toBe(false);
  });
});
