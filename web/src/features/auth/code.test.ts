// SPDX-License-Identifier: AGPL-3.0-only

import { describe, expect, it } from "vitest";

import { normalizeCode } from "./code";

describe("normalizeCode", () => {
  it("reads a code from an authenticator app, spaced or not", () => {
    expect(normalizeCode("123456")).toEqual({ code: "123456", kind: "totp" });
    expect(normalizeCode(" 123 456 ")).toEqual({ code: "123456", kind: "totp" });
    // A non-breaking space, as some apps copy it.
    expect(normalizeCode("123 456")).toEqual({ code: "123456", kind: "totp" });
  });

  it("reads a recovery code however it was pasted", () => {
    expect(normalizeCode("7KQ2-M9XD-4TPA-ZR3C")).toEqual({ code: "7KQ2M9XD4TPAZR3C", kind: "recovery" });
    expect(normalizeCode("7kq2 m9xd 4tpa zr3c")).toEqual({ code: "7KQ2M9XD4TPAZR3C", kind: "recovery" });
    // An en dash and a line break, as from a document or a note.
    expect(normalizeCode("7KQ2–M9XD\n4TPA ZR3C")).toEqual({ code: "7KQ2M9XD4TPAZR3C", kind: "recovery" });
  });

  it("reads I, L and O as the digits they look like, as the control plane does", () => {
    expect(normalizeCode("7KQ2-M9XD-4TPA-ZRIO")).toEqual({ code: "7KQ2M9XD4TPAZR10", kind: "recovery" });
    expect(normalizeCode("l23456")).toEqual({ code: "123456", kind: "totp" });
  });

  it("refuses what can't be a code", () => {
    for (const bad of [
      "12345",
      "1234567",
      "12345a",
      "7KQ2-M9XD-4TPA-ZR3U",
      "7KQ2-M9XD-4TPA-ZR3",
      "7KQ2_M9XD_4TPA_ZR3C",
      "",
    ]) {
      expect(normalizeCode(bad)).toBeUndefined();
    }
  });
});
