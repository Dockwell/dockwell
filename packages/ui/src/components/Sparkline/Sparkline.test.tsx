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

  it("can be named by text already on the page", () => {
    render(
      <>
        <span id="spark-name">Requests on api</span>
        <Sparkline data={[1, 2]} aria-labelledby="spark-name" />
      </>,
    );
    const spark = screen.getByRole("img", { name: "Requests on api" });
    expect(spark).not.toHaveAttribute("aria-label");
  });

  // A gap is where nothing was heard: the line breaks there, never drawing it as zero, and keeps
  // each point at its time, so what comes after a gap sits where it happened.
  it("breaks the line at a gap, keeps each point at its place, and draws a lone point as a dot", () => {
    const { container } = render(
      <Sparkline data={[10, 20, null, null, 30, null, 40, 50]} min={0} max={50} height={20} />,
    );
    const d = container.querySelector(".dw-spark-line")?.getAttribute("d") ?? "";
    const runs = d.split("M").filter(Boolean);
    expect(runs).toHaveLength(3);
    // Eight points across 100 units: 30 at index 4 sits at 57.14, alone, as a dot.
    expect(runs[1]).toBe("57.14 8.40h0");
    expect(runs[2]?.startsWith("85.71 ")).toBe(true);
    expect(d).not.toContain(` ${(2 + 16).toFixed(2)}L`);
    // The area follows each run down to the bottom, and nothing under the gaps.
    expect(container.querySelectorAll(".dw-spark-area")).toHaveLength(3);
    expect(screen.getByRole("img", { name: "Trend over 5 samples, latest 50" })).toBeVisible();
  });

  it("draws nothing for a series that was never heard", () => {
    const { container } = render(<Sparkline data={[null, null, null]} label="Nothing heard" />);
    expect(container.querySelector(".dw-spark-line")).toBeNull();
    expect(container.querySelector(".dw-spark-area")).toBeNull();
    expect(screen.getByRole("img", { name: "Nothing heard" })).toBeVisible();
  });

  // An hour that averaged 20 and touched 100 doesn't read as a calm one.
  it("draws each point's peak as a band above the line, and scales to it", () => {
    const { container } = render(<Sparkline data={[20, 20, null, 20]} peaks={[20, 100, null, 40]} height={20} />);
    const bands = container.querySelectorAll(".dw-spark-peak");
    // A band for each run that has a peak above its average.
    expect(bands).toHaveLength(2);
    // Scaled to the peak, 100 is at the top: 2 px of padding.
    expect(bands[0]?.getAttribute("d")).toContain("33.33 2.00");
    // Without peaks, there's no band.
    const { container: plain } = render(<Sparkline data={[20, 20]} />);
    expect(plain.querySelector(".dw-spark-peak")).toBeNull();
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
