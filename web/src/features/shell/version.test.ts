// SPDX-License-Identifier: AGPL-3.0-only

import { describe, expect, it } from "vitest";

import { shortVersion } from "./version";

describe("shortVersion", () => {
  it.each([
    ["v0.4.0", "0.4.0"],
    ["v1.2.3-rc.1", "1.2.3-rc.1"],
    ["v0.4.1-0.20261103214000-3f2a9c1d4e5b", "0.4.1-dev"],
    ["v0.4.1-rc.1.0.20261103214000-3f2a9c1d4e5b", "0.4.1-dev"],
    ["v0.0.0-20261104093000-589add0abcde+dirty", "dev"],
    ["devel", "devel"],
  ])("shows %s as %s", (version, short) => {
    expect(shortVersion(version)).toBe(short);
  });
});
