// SPDX-License-Identifier: AGPL-3.0-only

import { describe, expect, it } from "vitest";

import { unfitOn } from "./architectures";

describe("unfitOn", () => {
  it("lets an app that names no architecture run anywhere", () => {
    expect(unfitOn([], { architecture: "arm64" })).toBeUndefined();
    expect(unfitOn([], {})).toBeUndefined();
  });
  it("says a server of another architecture, and one that hasn't said", () => {
    expect(unfitOn(["amd64"], { architecture: "amd64" })).toBeUndefined();
    expect(unfitOn(["amd64"], { architecture: "arm64" })).toBe("arm64");
    expect(unfitOn(["amd64"], { architecture: "" })).toBe("unknown");
    expect(unfitOn(["amd64"], {})).toBe("unknown");
  });
});
