// SPDX-License-Identifier: AGPL-3.0-only

import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import { englishStrings, UIStringsProvider } from "../../lib/strings";
import { expectAccessible } from "../../test/a11y";
import { Metric } from "./Metric";

describe("Metric", () => {
  it("shows the value with its unit, a named sparkline and a caption", async () => {
    const { container } = render(
      <Metric
        label="Error rate"
        value="0.42"
        unit="%"
        icon="activity"
        delta="from 0.10%"
        deltaTone="warning"
        data={[0.1, 0.1, 0.2, 0.42]}
        tone="warning"
        min={0}
        caption="Rising since 13:58"
      />,
    );
    expect(container.querySelector(".dw-metric-value")).toHaveTextContent("0.42");
    expect(container.querySelector(".dw-metric-unit")).toHaveTextContent("%");
    expect(screen.getByText("from 0.10%")).toHaveClass("dw-metric-delta", "dw-tone-warning");
    const spark = screen.getByRole("img", { name: "Error rate trend" });
    expect(spark).toHaveClass("dw-tone-warning");
    expect(spark).toHaveAttribute("width", "100%");
    expect(spark).toHaveAttribute("height", "36");
    expect(screen.getByText("Rising since 13:58")).toHaveClass("dw-metric-caption");
    await expectAccessible(container);
  });

  it("leaves out what it isn't given, and keeps a neutral delta muted", () => {
    const { container } = render(<Metric label="Memory" value="7.4" delta="+0.2" />);
    expect(container.querySelector(".dw-metric-unit")).toBeNull();
    expect(container.querySelector("svg")).toBeNull();
    expect(container.querySelector(".dw-metric-caption")).toBeNull();
    expect(screen.getByText("+0.2").className).toBe("dw-metric-delta");
  });

  it("names the sparkline with the translated pattern, or the default when the label isn't text", () => {
    render(
      <UIStringsProvider strings={{ ...englishStrings, trend: (label) => `${label}趋势` }}>
        <Metric label="CPU" value="23.4" unit="%" data={[20, 23.4]} />
        <Metric label={<b>Disk</b>} value="61" data={[60, 61]} />
      </UIStringsProvider>,
    );
    expect(screen.getByRole("img", { name: "CPU趋势" })).toBeVisible();
    expect(screen.getByRole("img", { name: "Trend over 2 samples, latest 61" })).toBeVisible();
  });
});
