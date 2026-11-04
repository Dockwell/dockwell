// SPDX-License-Identifier: AGPL-3.0-only

import { describe, expect, it } from "vitest";

import { initials } from "./user";

describe("initials", () => {
  it.each([
    ["Mira Chen", "MC"],
    ["Mira", "M"],
    ["  ana  maría de la torre ", "AT"],
    ["陈米拉", "陈"],
    ["Élodie Ørsted", "ÉØ"],
    ["", ""],
  ])("makes %j into %j", (name, letters) => {
    expect(initials(name)).toBe(letters);
  });
});
