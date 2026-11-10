// SPDX-License-Identifier: AGPL-3.0-only

import { create, type MessageInitShape } from "@bufbuild/protobuf";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError } from "@connectrpc/connect";
import {
  type GetAuthStateResponse,
  GetAuthStateResponseSchema,
  Role,
  RoleGrantSchema,
  SignInMethod,
  UserSchema,
} from "@dockwell/api";
import { vi } from "vitest";

import { toBase64url } from "../lib/webauthn";

export const mira = create(UserSchema, {
  id: "usr_01JD6Z3R8M9QF2K7T4XW5BNC0V",
  name: "Mira Chen",
  email: "mira@acme.dev",
  role: Role.OWNER,
  createTime: timestampFromDate(new Date(2026, 10, 4, 9, 0)),
});

/** The sign-in state: claimed and signed in as Mira, unless init says otherwise. */
export function authState(init: MessageInitShape<typeof GetAuthStateResponseSchema> = {}): GetAuthStateResponse {
  const state = create(GetAuthStateResponseSchema, {
    claimed: true,
    user: mira,
    sessionId: "ses_01JD6Z3R8M9QF2K7T4XW5BNC0V",
    serverName: "hz-1",
    rpId: "localhost",
    signInMethod: SignInMethod.PASSKEY,
    ...init,
  });
  // Unless a test says what they hold, it's their role on the install, as the
  // control plane says it for someone with no project of their own.
  if (!init.grants && state.user && state.user.role !== Role.UNSPECIFIED) {
    state.grants = [create(RoleGrantSchema, { role: state.user.role })];
  }
  return state;
}

/** An AuthService error as the control plane sends it, with its reason. */
export function authError(code: Code, reason: string, message = reason, retryAfter?: number): ConnectError {
  const metadata = new Headers({ "Dockwell-Error": reason });
  if (retryAfter) metadata.set("Retry-After", String(retryAfter));
  return new ConnectError(message, code, metadata);
}

const bytes = (...values: number[]) => new Uint8Array(values).buffer;

/**
 * Stands in for the browser's WebAuthn: a secure context with
 * navigator.credentials whose create and get answer like an authenticator,
 * or fail with the DOMException name given.
 */
export function stubPasskeys(fail?: { create?: string; get?: string }) {
  vi.stubGlobal("PublicKeyCredential", function PublicKeyCredential() {});
  vi.stubGlobal("isSecureContext", true);
  const create = vi.fn(async (_options: CredentialCreationOptions) => {
    if (fail?.create) throw new DOMException("refused", fail.create);
    return {
      id: toBase64url(bytes(1, 2, 3)),
      rawId: bytes(1, 2, 3),
      type: "public-key",
      authenticatorAttachment: "platform",
      response: {
        clientDataJSON: bytes(4, 5),
        attestationObject: bytes(6, 7),
        getTransports: () => ["internal"],
      },
      getClientExtensionResults: () => ({}),
    };
  });
  const get = vi.fn(async (_options: CredentialRequestOptions) => {
    if (fail?.get) throw new DOMException("refused", fail.get);
    return {
      id: toBase64url(bytes(1, 2, 3)),
      rawId: bytes(1, 2, 3),
      type: "public-key",
      authenticatorAttachment: "platform",
      response: {
        clientDataJSON: bytes(4, 5),
        authenticatorData: bytes(8),
        signature: bytes(9),
        userHandle: bytes(10),
      },
      getClientExtensionResults: () => ({}),
    };
  });
  vi.stubGlobal("navigator", { ...navigator, credentials: { create, get } });
  return { create, get };
}

/** Creation options as the control plane sends them. */
export const creationOptions = JSON.stringify({
  rp: { id: "localhost", name: "Dockwell" },
  user: { id: "AQID", name: "mira@acme.dev", displayName: "Mira Chen" },
  challenge: "q83v",
  pubKeyCredParams: [{ type: "public-key", alg: -7 }],
  timeout: 300000,
  excludeCredentials: [{ id: "BAU", type: "public-key" }],
  authenticatorSelection: { residentKey: "required", requireResidentKey: true, userVerification: "required" },
  attestation: "none",
});

/** Request options as the control plane sends them. */
export const requestOptions = JSON.stringify({
  challenge: "q83v",
  rpId: "localhost",
  userVerification: "required",
  timeout: 300000,
});

/** A failure the control plane would send for a missing session. */
export const signedOut = () => authError(Code.Unauthenticated, "unauthenticated");
