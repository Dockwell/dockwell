// SPDX-License-Identifier: AGPL-3.0-only

import { fireEvent, render, screen } from "@testing-library/react";
import { useState } from "react";
import { describe, expect, it } from "vitest";

import { TimeSeriesChart } from "./TimeSeriesChart";

describe("TimeSeriesChart", () => {
  const times = [1_700_000_000_000, 1_700_000_300_000, 1_700_000_600_000, 1_700_000_900_000];

  it("synchronizes hover readout across sibling charts and explains gap states", () => {
    function Pair() {
      const [active, setActive] = useState<number | null>(null);
      return (
        <>
          <TimeSeriesChart
            title="CPU"
            timestamps={times}
            gaps={[null, "offline", null, null]}
            series={[
              {
                id: "cpu",
                label: "Avg",
                values: [20, null, 60, 40],
                peaks: [30, null, 85, 50],
                formatValue: (v) => `${v}%`,
                formatPeak: (p) => `peak ${p}%`,
              },
            ]}
            min={0}
            max={100}
            formatAxis={(v) => `${v}%`}
            formatTick={(ms) => String(ms)}
            formatHoverTime={(ms) => `T=${ms}`}
            activeIndex={active}
            onActiveIndexChange={setActive}
          />
          <TimeSeriesChart
            title="Memory"
            timestamps={times}
            gaps={[null, "offline", null, null]}
            series={[
              {
                id: "mem",
                label: "Used",
                values: [100, null, 300, 200],
                formatValue: (v) => `${v} MB`,
              },
            ]}
            min={0}
            max={500}
            formatAxis={(v) => `${v} MB`}
            formatTick={(ms) => String(ms)}
            formatHoverTime={(ms) => `T=${ms}`}
            activeIndex={active}
            onActiveIndexChange={setActive}
          />
        </>
      );
    }

    render(<Pair />);
    const svgs = screen.getAllByRole("img");
    expect(svgs).toHaveLength(2);

    // ArrowRight moves to index 0 -> both CPU and Memory readouts show T=times[0] and values at index 0.
    // L5: Only the focused chart sets aria-live="polite"; sibling stays aria-live="off".
    fireEvent.focus(svgs[0] as Element);
    const liveRegions = document.querySelectorAll(".dw-ts-readout");
    expect(liveRegions[0]).toHaveAttribute("aria-live", "polite");
    expect(liveRegions[1]).toHaveAttribute("aria-live", "off");
    expect(svgs[0]).toHaveAttribute("aria-describedby");
    const descId = svgs[0]?.getAttribute("aria-describedby") ?? "";
    const descEl = document.getElementById(descId);
    expect(descEl?.textContent).toMatch(/left and right arrows/i);

    fireEvent.keyDown(svgs[0] as Element, { key: "ArrowRight" });
    expect(screen.getAllByText(`T=${times[0]}`)).toHaveLength(2);
    expect(screen.getByText("20%")).toBeInTheDocument();
    expect(screen.getByText("100 MB")).toBeInTheDocument();

    // ArrowRight again moves to index 1 (offline gap) -> both charts show "The agent was offline".
    fireEvent.keyDown(svgs[0] as Element, { key: "ArrowRight" });
    expect(screen.getAllByText("The agent was offline")).toHaveLength(2);
  });

  it("renders emptyMessage when no bucket is covered", () => {
    render(
      <TimeSeriesChart
        title="CPU"
        timestamps={times}
        gaps={["before-join", "before-join", "before-join", "before-join"]}
        series={[{ id: "cpu", label: "Avg", values: [null, null, null, null], formatValue: (v) => `${v}%` }]}
        formatAxis={(v) => `${v}%`}
        formatTick={(ms) => String(ms)}
        formatHoverTime={(ms) => String(ms)}
        emptyMessage="No data before Dockwell joined"
        activeIndex={null}
        onActiveIndexChange={() => {}}
      />,
    );
    expect(screen.getAllByText("No data before Dockwell joined").length).toBeGreaterThanOrEqual(1);
  });
});
