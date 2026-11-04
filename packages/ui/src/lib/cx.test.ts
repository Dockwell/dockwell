// SPDX-License-Identifier: AGPL-3.0-only

import { createElement } from "react";
import { describe, expect, it } from "vitest";

import { cx, hasContent } from "./cx";

describe("cx", () => {
  it("joins the truthy class names", () => {
    expect(cx("dw-btn", false, null, undefined, "", "is-on")).toBe("dw-btn is-on");
  });
});

describe("hasContent", () => {
  it.each([null, undefined, false, true, ""])("finds nothing to render in %j", (node) => {
    expect(hasContent(node)).toBe(false);
  });

  it.each([0, "0", "Saved", createElement("b", null, "Saved")])("finds something in %s", (node) => {
    expect(hasContent(node)).toBe(true);
  });
});
