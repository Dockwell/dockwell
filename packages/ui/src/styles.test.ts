// SPDX-License-Identifier: AGPL-3.0-only

import { readFileSync } from "node:fs";
import { join } from "node:path";
import { describe, expect, it } from "vitest";

const read = (path: string) => readFileSync(join(import.meta.dirname, path), "utf8");

describe("the foundations", () => {
  // Pages put dw-panel on their own elements, as the mocks do. A screen whose
  // code split holds no Panel would draw them flat if Panel brought its rules.
  it("carry Panel's rules, once", () => {
    expect(read("styles/index.css")).toContain('@import "../components/Panel/Panel.css";');
    expect(read("components/Panel/Panel.tsx")).not.toMatch(/import\s+["'][^"']*\.css["']/);
  });
});
