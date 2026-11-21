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

  // The same for dw-table, which the audit log draws on its own rows.
  it("carry Table's rules, once", () => {
    expect(read("styles/index.css")).toContain('@import "../components/Table/Table.css";');
    expect(read("components/Table/Table.tsx")).not.toMatch(/import\s+["'][^"']*\.css["']/);
  });

  // Every screen draws skeletons while it waits, and Table draws them in its rows.
  it("carry Skeleton's rules, once", () => {
    expect(read("styles/index.css")).toContain('@import "../components/Skeleton/Skeleton.css";');
    expect(read("components/Skeleton/Skeleton.tsx")).not.toMatch(/import\s+["'][^"']*\.css["']/);
  });

  // The shapes wait as long as the region's announcement does, so neither shows for a fast answer.
  it("hold a skeleton back as long as its announcement", () => {
    expect(read("components/Skeleton/Skeleton.css")).toMatch(/dw-skeleton-in 0s linear 150ms both/);
    expect(read("components/Skeleton/Skeleton.tsx")).toContain("SKELETON_DELAY_MS = 150;");
  });

  // Nothing shimmers under reduced motion.
  it("hold a skeleton still under reduced motion", () => {
    const css = read("components/Skeleton/Skeleton.css");
    const reduced = css.slice(css.indexOf("@media (prefers-reduced-motion: reduce)"));
    expect(reduced).toMatch(/\.dw-skeleton\s*\{\s*animation-name: dw-skeleton-in;/);
    // The foundations' own rule caps every dw- animation at one instant run.
    expect(read("styles/base.css")).toMatch(/animation-iteration-count: 1 !important/);
  });
});
