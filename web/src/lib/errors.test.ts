// SPDX-License-Identifier: AGPL-3.0-only

import { Code, ConnectError } from "@connectrpc/connect";
import { describe, expect, it } from "vitest";

import { isSessionEnded, sentence } from "./errors";

const refused = (code: Code, reason?: string) =>
  new ConnectError("refused", code, reason ? new Headers({ "Dockwell-Error": reason }) : undefined);

describe("sentence", () => {
  const said = (message: string) => sentence(new ConnectError(message));

  it("capitalises a sentence, and keeps a name it starts with as it's written", () => {
    expect(said("a route needs a domain")).toBe("A route needs a domain.");
    expect(said("can't reach the server")).toBe("Can't reach the server.");
    expect(said("bad-db-1 runs with read_only")).toBe("bad-db-1 runs with read_only.");
    expect(said("legacy_api is already what the service api runs")).toBe(
      "legacy_api is already what the service api runs.",
    );
    expect(said("shop2 runs a service web already")).toBe("shop2 runs a service web already.");
    expect(said("app.example.com doesn't lead here")).toBe("app.example.com doesn't lead here.");
  });
});

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
