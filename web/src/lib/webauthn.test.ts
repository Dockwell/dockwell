// SPDX-License-Identifier: AGPL-3.0-only

import { describe, expect, it } from "vitest";

import { creationOptions, requestOptions, stubPasskeys } from "../test/auth";
import {
  createPasskey,
  fromBase64url,
  getPasskey,
  PasskeyError,
  parseCreationOptions,
  parseRequestOptions,
  passkeysAvailable,
  toBase64url,
} from "./webauthn";

describe("base64url", () => {
  it("round-trips every byte without padding", () => {
    const all = new Uint8Array(256).map((_, i) => i);
    const text = toBase64url(all);
    expect(text).not.toMatch(/[+/=]/);
    expect(Array.from(fromBase64url(text))).toEqual(Array.from(all));
  });

  it("reads padded and unpadded input alike", () => {
    expect(Array.from(fromBase64url("AQID"))).toEqual([1, 2, 3]);
    expect(Array.from(fromBase64url("AQ"))).toEqual([1]);
    expect(Array.from(fromBase64url("AQ=="))).toEqual([1]);
    expect(toBase64url(new Uint8Array([251, 255]))).toBe("-_8");
  });
});

describe("WebAuthn options", () => {
  it("decodes the binary fields of creation options", () => {
    const o = parseCreationOptions(creationOptions);
    expect(Array.from(new Uint8Array(o.challenge as ArrayBuffer))).toEqual([171, 205, 239]);
    expect(Array.from(new Uint8Array(o.user.id as ArrayBuffer))).toEqual([1, 2, 3]);
    expect(Array.from(new Uint8Array(o.excludeCredentials?.[0]?.id as ArrayBuffer))).toEqual([4, 5]);
    expect(o.authenticatorSelection?.userVerification).toBe("required");
    expect(o.rp.id).toBe("localhost");
  });

  it("decodes request options", () => {
    const o = parseRequestOptions(requestOptions);
    expect(o.rpId).toBe("localhost");
    expect(o.allowCredentials).toEqual([]);
  });
});

describe("passkeys", () => {
  it("are unavailable outside a secure context", () => {
    expect(passkeysAvailable()).toBe(false);
  });

  it("create a credential and answer with RegistrationResponseJSON", async () => {
    const { create } = stubPasskeys();
    const json = JSON.parse(await createPasskey(creationOptions));
    expect(create).toHaveBeenCalledOnce();
    expect(json).toMatchObject({
      id: "AQID",
      rawId: "AQID",
      type: "public-key",
      response: { clientDataJSON: "BAU", attestationObject: "Bgc", transports: ["internal"] },
      authenticatorAttachment: "platform",
    });
  });

  it("sign in with hints and answer with AuthenticationResponseJSON", async () => {
    const { get } = stubPasskeys();
    const json = JSON.parse(await getPasskey(requestOptions, ["security-key", "hybrid"]));
    const publicKey = get.mock.calls[0]?.[0].publicKey as PublicKeyCredentialRequestOptions & { hints?: string[] };
    expect(publicKey.hints).toEqual(["security-key", "hybrid"]);
    expect(json.response).toEqual({
      clientDataJSON: "BAU",
      authenticatorData: "CA",
      signature: "CQ",
      userHandle: "Cg",
    });
  });

  it("name why a ceremony stopped", async () => {
    stubPasskeys({ create: "NotAllowedError", get: "InvalidStateError" });
    await expect(createPasskey(creationOptions)).rejects.toMatchObject({ problem: "cancelled" });
    await expect(getPasskey(requestOptions)).rejects.toMatchObject({ problem: "exists" });
    stubPasskeys({ create: "SecurityError" });
    await expect(createPasskey(creationOptions)).rejects.toBeInstanceOf(PasskeyError);
    await expect(createPasskey(creationOptions)).rejects.toMatchObject({ problem: "unsupported" });
  });
});
