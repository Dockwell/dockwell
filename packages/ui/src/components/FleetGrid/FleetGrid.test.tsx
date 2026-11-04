// SPDX-License-Identifier: AGPL-3.0-only

import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import { englishStrings, UIStringsProvider } from "../../lib/strings";
import { expectAccessible } from "../../test/a11y";
import { FleetGrid, type FleetGroup } from "./FleetGrid";

const groups: FleetGroup[] = [
  {
    label: "fra-1",
    meta: "Frankfurt · 3 servers",
    servers: [
      { id: "fra-1-01", name: "fra-1-01", status: "healthy", cpu: 31, mem: 52, ip: "10.210.4.11" },
      {
        id: "fra-1-07",
        name: "fra-1-07",
        status: "degraded",
        cpu: 92,
        mem: 93,
        ip: "10.210.4.17",
        note: "memory climbing",
      },
      { id: "fra-1-09", name: "fra-1-09", status: "deploying" },
    ],
  },
  {
    label: "ams-1",
    servers: [
      { id: "ams-1-03", name: "ams-1-03", status: "down", cpu: null },
      { id: "ams-1-04", name: "ams-1-04", status: "stopped" },
    ],
  },
];

describe("FleetGrid", () => {
  it("counts servers per state in the legend, with shapes", async () => {
    const { container } = render(<FleetGrid groups={groups} />);
    const legend = container.querySelector(".dw-fleet-legend");
    expect(legend?.textContent).toBe("5 servers1 healthy1 degraded1 down1 deploying1 stopped");
    expect(legend?.querySelectorAll("svg")).toHaveLength(5);
    await expectAccessible(container);
  });

  it("lists each group's tiles as buttons named with the server and its state", () => {
    render(<FleetGrid groups={groups} />);
    const fra = screen.getByRole("list", { name: "fra-1" });
    expect(
      within(fra)
        .getAllByRole("button")
        .map((b) => b.getAttribute("aria-label")),
    ).toEqual(["fra-1-01, Healthy, CPU 31%", "fra-1-07, Degraded, CPU 92%", "fra-1-09, Deploying"]);
    expect(screen.getByText("Frankfurt · 3 servers")).toHaveClass("dw-fleet-gmeta");
    expect(screen.getByText("2 servers")).toHaveClass("dw-fleet-gmeta");
    // Healthy tiles stay quiet; every other state carries its shape.
    expect(screen.getByRole("button", { name: /^fra-1-01/ }).querySelector("svg")).toBeNull();
    expect(screen.getByRole("button", { name: /^ams-1-04/ }).querySelector("svg")).not.toBeNull();
    expect(screen.getByRole("button", { name: /^ams-1-03/ })).toHaveClass("dw-tile", "dw-tile-down");
  });

  it("reads out the focused or hovered server, then the selected one", async () => {
    const user = userEvent.setup();
    const { container } = render(<FleetGrid groups={groups} selectedId="ams-1-03" />);
    const readout = container.querySelector(".dw-fleet-readout");
    expect(readout).toHaveAttribute("aria-live", "polite");
    expect(readout).toHaveTextContent("ams-1-03");
    expect(screen.getByRole("button", { name: /^ams-1-03/ })).toHaveAttribute("aria-current", "true");
    await user.tab();
    await user.tab();
    expect(screen.getByRole("button", { name: /^fra-1-07/ })).toHaveFocus();
    expect(readout?.textContent).toBe("Degraded: fra-1-07DegradedCPU 92%Memory 93%10.210.4.17memory climbing");
    await user.hover(screen.getByRole("button", { name: /^fra-1-09/ }));
    expect(readout).toHaveTextContent("fra-1-09");
    await user.unhover(screen.getByRole("button", { name: /^fra-1-09/ }));
    expect(readout).toHaveTextContent("ams-1-03");
  });

  it("keeps the readout on the latest data, and lets go of a server that goes away", async () => {
    const user = userEvent.setup();
    const live = (cpu: number): FleetGroup[] => [
      {
        label: "fra-1",
        servers: [
          { id: "fra-1-01", name: "fra-1-01", status: "healthy", cpu: 31 },
          { id: "fra-1-07", name: "fra-1-07", status: "degraded", cpu },
        ],
      },
    ];
    const { container, rerender } = render(<FleetGrid groups={live(92)} />);
    const readout = container.querySelector(".dw-fleet-readout");
    await user.hover(screen.getByRole("button", { name: /^fra-1-07/ }));
    expect(readout).toHaveTextContent("CPU 92%");
    rerender(<FleetGrid groups={live(97)} />);
    expect(readout).toHaveTextContent("CPU 97%");

    screen.getByRole("button", { name: /^fra-1-07/ }).focus();
    rerender(<FleetGrid groups={[{ servers: live(97)[0]?.servers.slice(0, 1) ?? [] }]} />);
    expect(screen.queryByRole("button", { name: /^fra-1-07/ })).toBeNull();
    expect(readout).toHaveTextContent("Hover or focus a server to inspect it.");
    // Back again, it isn't hovered or focused.
    rerender(<FleetGrid groups={live(97)} />);
    expect(readout).toHaveTextContent("Hover or focus a server to inspect it.");
  });

  it("redraws only the readout on hover, and only the tiles that change on a render", async () => {
    const user = userEvent.setup();
    const tile = vi.fn(englishStrings.fleetGrid.tile);
    const strings = { ...englishStrings, fleetGrid: { ...englishStrings.fleetGrid, tile } };
    const grid = (selectedId?: string) => (
      <UIStringsProvider strings={strings}>
        <FleetGrid groups={groups} selectedId={selectedId} />
      </UIStringsProvider>
    );
    const { container, rerender } = render(grid());
    expect(tile).toHaveBeenCalledTimes(5);
    tile.mockClear();
    await user.hover(screen.getByRole("button", { name: /^fra-1-07/ }));
    expect(container.querySelector(".dw-fleet-readout")).toHaveTextContent("fra-1-07");
    await user.unhover(screen.getByRole("button", { name: /^fra-1-07/ }));
    await user.tab();
    expect(tile).not.toHaveBeenCalled();
    rerender(grid("ams-1-03"));
    expect(tile.mock.calls.map(([name]) => name)).toEqual(["ams-1-03"]);
  });

  it("opens a server on click", async () => {
    const user = userEvent.setup();
    const onSelect = vi.fn();
    render(<FleetGrid groups={groups} onSelect={onSelect} />);
    await user.click(screen.getByRole("button", { name: /^ams-1-03/ }));
    expect(onSelect).toHaveBeenCalledWith(groups[1]?.servers[0]);
  });

  it("takes a flat list, a small size, a hint, and can drop the legend and readout", async () => {
    const servers = groups.flatMap((group) => group.servers);
    const { container, rerender } = render(
      <FleetGrid servers={servers} size="sm" hint="Hover a server to see its step." />,
    );
    expect(container.firstChild).toHaveClass("dw-fleet-sm");
    expect(screen.getByRole("list", { name: "Servers" })).toBeVisible();
    expect(screen.getByText("Hover a server to see its step.")).toBeVisible();
    await expectAccessible(container);
    rerender(<FleetGrid servers={servers} legend={false} readout={false} />);
    expect(container.querySelector(".dw-fleet-legend")).toBeNull();
    expect(container.querySelector(".dw-fleet-readout")).toBeNull();
    rerender(<FleetGrid servers={servers.slice(0, 1)} />);
    expect(screen.getByText("1 server")).toHaveClass("dw-fleet-total");
    expect(screen.getByText("Hover or focus a server to inspect it.")).toBeVisible();
  });

  it("speaks the app's language", () => {
    render(
      <UIStringsProvider
        strings={{
          ...englishStrings,
          status: { ...englishStrings.status, degraded: "降级" },
          fleetGrid: {
            ...englishStrings.fleetGrid,
            servers: (n) => `${n} 台服务器`,
            count: (n, status) => `${n} ${status === "degraded" ? "降级" : status}`,
            tile: (name, status) => `${name}，${status}`,
            group: "服务器",
          },
        }}
      >
        <FleetGrid servers={groups[0]?.servers ?? []} />
      </UIStringsProvider>,
    );
    expect(screen.getByText("3 台服务器")).toBeVisible();
    expect(screen.getByText("1 降级")).toBeVisible();
    expect(screen.getByRole("list", { name: "服务器" })).toBeVisible();
    expect(screen.getByRole("button", { name: "fra-1-07，降级" })).toBeVisible();
  });
});
