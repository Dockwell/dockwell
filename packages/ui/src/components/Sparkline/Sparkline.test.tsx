// SPDX-License-Identifier: AGPL-3.0-only

import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import { englishStrings, UIStringsProvider } from "../../lib/strings";
import { expectAccessible } from "../../test/a11y";
import { Sparkline } from "./Sparkline";

describe("Sparkline", () => {
  it("is an image named after its shape by default", async () => {
    const { container } = render(<Sparkline data={[12, 18, 15, 23.456]} />);
    const img = screen.getByRole("img", { name: "Trend over 4 samples, latest 23.46" });
    expect(img).toHaveAttribute("width", "120");
    expect(img).toHaveAttribute("height", "32");
    expect(img).toHaveAttribute("viewBox", "0 0 100 32");
    expect(img).toHaveClass("dw-spark", "dw-tone-ink");
    expect(container.querySelector(".dw-spark-area")).not.toBeNull();
    expect(container.querySelector(".dw-spark-threshold")).toBeNull();
    await expectAccessible(container);
  });

  it("takes a label, a tone, a size and a threshold, and can drop the area", () => {
    const { container } = render(
      <Sparkline
        data={[60, 64, 91, 93]}
        label="Memory on fra-1-07, now 93%"
        tone="warning"
        width="100%"
        height={20}
        min={0}
        max={100}
        threshold={85}
        area={false}
      />,
    );
    const img = screen.getByRole("img", { name: "Memory on fra-1-07, now 93%" });
    expect(img).toHaveAttribute("width", "100%");
    expect(img).toHaveClass("dw-tone-warning");
    expect(container.querySelector(".dw-spark-area")).toBeNull();
    const threshold = container.querySelector(".dw-spark-threshold");
    // 85 on a 0–100 scale, 2 px padding in a 20 px box: 2 + 0.15 × 16.
    expect(Number(threshold?.getAttribute("y1"))).toBeCloseTo(4.4, 5);
    const line = container.querySelector(".dw-spark-line")?.getAttribute("d");
    expect(line?.startsWith("M0.00 ")).toBe(true);
    expect(line).toContain("L100.00 ");
  });

  it("draws a flat line for one sample or none", () => {
    const { container } = render(<Sparkline data={[5]} />);
    expect(container.querySelector(".dw-spark-line")?.getAttribute("d")).toBe("M0.00 16.00L100.00 16.00");
    expect(screen.getByRole("img", { name: "Trend over 1 sample, latest 5" })).toBeVisible();
    render(<Sparkline data={[]} label="No data yet" />);
    expect(screen.getByRole("img", { name: "No data yet" })).toBeVisible();
  });

  it("uses the translated default name", () => {
    render(
      <UIStringsProvider
        strings={{ ...englishStrings, sparkline: (n, latest) => `${n} 个样本的趋势，最新值 ${latest}` }}
      >
        <Sparkline data={[1, 2]} />
      </UIStringsProvider>,
    );
    expect(screen.getByRole("img", { name: "2 个样本的趋势，最新值 2" })).toBeVisible();
  });
});
