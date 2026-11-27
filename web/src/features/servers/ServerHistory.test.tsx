// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { timestampFromMs } from "@bufbuild/protobuf/wkt";
import {
  Connection,
  ContainerHistoryPointSchema,
  GetContainerHistoryResponseSchema,
  GetServerHistoryResponseSchema,
  HistoryPointState,
  HistoryRange,
  ServerHistoryPointSchema,
  ServerInventorySchema,
  ServerSchema,
} from "@dockwell/api";
import { fireEvent, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import { renderWithApi } from "../../test/render";
import { ContainerHistory } from "./ContainerHistory";
import { ServerHistory } from "./ServerHistory";

describe("ServerHistory", () => {
  it("renders all five charts, switches ranges and filters, and synchronizes hover across charts", async () => {
    const user = userEvent.setup();
    const seenRequests: { range: HistoryRange; disk: string; iface: string }[] = [];
    const t0 = Date.UTC(2026, 11, 20, 12, 0, 0);

    const server = create(ServerSchema, {
      id: "srv_1",
      name: "web-1",
      connection: Connection.CONNECTED,
      inventory: create(ServerInventorySchema, {
        cpuCount: 4,
        memoryBytes: 8n * 1024n * 1024n * 1024n,
      }),
    });

    renderWithApi(<ServerHistory server={server} />, {
      servers: {
        getServerHistory: (req) => {
          seenRequests.push({ range: req.range, disk: req.disk, iface: req.networkInterface });
          return create(GetServerHistoryResponseSchema, {
            since: timestampFromMs(t0),
            until: timestampFromMs(t0 + 180_000),
            disks: ["nvme0n1", "sda"],
            interfaces: ["eth0", "eth1"],
            points: [
              create(ServerHistoryPointSchema, {
                time: timestampFromMs(t0),
                state: HistoryPointState.BEFORE_JOIN,
              }),
              create(ServerHistoryPointSchema, {
                time: timestampFromMs(t0 + 60_000),
                state: HistoryPointState.OFFLINE,
              }),
              create(ServerHistoryPointSchema, {
                time: timestampFromMs(t0 + 120_000),
                state: HistoryPointState.COVERED,
                cpuPercent: 42.5,
                cpuPeakPercent: 78.0,
                memoryUsedBytes: 4n * 1024n * 1024n * 1024n,
                memoryPeakBytes: 5n * 1024n * 1024n * 1024n,
                memoryTotalBytes: 8n * 1024n * 1024n * 1024n,
                load1: 1.25,
                load1Peak: 2.1,
                diskReadBytesPerSecond: 10n * 1024n * 1024n,
                diskWriteBytesPerSecond: 4n * 1024n * 1024n,
                diskReadIops: 240,
                diskWriteIops: 95,
                networkRxBytesPerSecond: 2n * 1024n * 1024n,
                networkTxBytesPerSecond: 1n * 1024n * 1024n,
              }),
            ],
          });
        },
      },
    });

    await screen.findByRole("heading", { name: "CPU" });
    expect(screen.getByRole("heading", { name: "Memory" })).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "Load" })).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "Disk I/O" })).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "Network" })).toBeInTheDocument();
    expect(seenRequests.some((r) => r.range === HistoryRange.HISTORY_RANGE_24H)).toBe(true);

    // Keyboard stepping across the CPU chart synchronizes all 5 charts to the gap states and values.
    const charts = screen.getAllByRole("img");
    expect(charts).toHaveLength(5);
    fireEvent.keyDown(charts[0] as Element, { key: "ArrowRight" });
    expect(screen.getAllByText("No data before Dockwell joined")).toHaveLength(5);
    fireEvent.keyDown(charts[0] as Element, { key: "ArrowRight" });
    expect(screen.getAllByText("The agent was offline")).toHaveLength(5);
    fireEvent.keyDown(charts[0] as Element, { key: "ArrowRight" });
    expect(screen.getByText("42.5%")).toBeInTheDocument();

    // Switching Disk I/O metric to IOPS shows IOPS values.
    await user.click(screen.getByRole("radio", { name: "IOPS" }));
    expect(screen.getByText("240 IOPS")).toBeInTheDocument();

    // Picking a specific disk and network interface passes them in the RPC.
    await user.selectOptions(screen.getByRole("combobox", { name: "Disk" }), "nvme0n1");
    await vi.waitFor(() => expect(seenRequests.some((r) => r.disk === "nvme0n1")).toBe(true));
    await user.selectOptions(screen.getByRole("combobox", { name: "Network interface" }), "eth1");
    await vi.waitFor(() => expect(seenRequests.some((r) => r.iface === "eth1")).toBe(true));

    // Switching to 7 d issues a new query with HISTORY_RANGE_7D.
    await user.click(screen.getByRole("radio", { name: "7 d" }));
    await vi.waitFor(() => expect(seenRequests.some((r) => r.range === HistoryRange.HISTORY_RANGE_7D)).toBe(true));
  });

  it("collapses an all-offline range into one compact panel instead of five empty charts", async () => {
    const t0 = Date.UTC(2026, 11, 20, 12, 0, 0);
    const server = create(ServerSchema, {
      id: "srv_off",
      name: "db-3",
      connection: Connection.UNREACHABLE,
    });
    renderWithApi(<ServerHistory server={server} />, {
      servers: {
        getServerHistory: () =>
          create(GetServerHistoryResponseSchema, {
            since: timestampFromMs(t0),
            until: timestampFromMs(t0 + 120_000),
            points: [
              create(ServerHistoryPointSchema, { time: timestampFromMs(t0), state: HistoryPointState.OFFLINE }),
              create(ServerHistoryPointSchema, {
                time: timestampFromMs(t0 + 60_000),
                state: HistoryPointState.OFFLINE,
              }),
            ],
          }),
      },
    });
    expect(await screen.findByText("The agent was offline")).toBeVisible();
    expect(screen.getAllByText("The agent was offline")).toHaveLength(1);
    expect(screen.queryAllByRole("img")).toHaveLength(0);
    expect(screen.getByRole("radiogroup", { name: "Time range" })).toBeVisible();
  });
});

describe("ContainerHistory", () => {
  it("renders CPU and Memory charts with range switching and synchronized hover", async () => {
    const user = userEvent.setup();
    const seenRanges: HistoryRange[] = [];
    const t0 = Date.UTC(2026, 11, 20, 12, 0, 0);

    renderWithApi(<ContainerHistory serverId="srv_1" containerId={"a".repeat(64)} />, {
      servers: {
        getContainerHistory: (req) => {
          seenRanges.push(req.range);
          return create(GetContainerHistoryResponseSchema, {
            since: timestampFromMs(t0),
            until: timestampFromMs(t0 + 120_000),
            points: [
              create(ContainerHistoryPointSchema, {
                time: timestampFromMs(t0),
                state: HistoryPointState.NO_CONTROL_PLANE,
              }),
              create(ContainerHistoryPointSchema, {
                time: timestampFromMs(t0 + 60_000),
                state: HistoryPointState.COVERED,
                cpuPercent: 18.5,
                cpuPeakPercent: 32.0,
                memoryBytes: 256n * 1024n * 1024n,
                memoryPeakBytes: 310n * 1024n * 1024n,
                memoryLimitBytes: 512n * 1024n * 1024n,
              }),
            ],
          });
        },
      },
    });

    await screen.findByRole("heading", { name: "CPU" });
    expect(screen.getByRole("heading", { name: "Memory" })).toBeInTheDocument();
    const charts = screen.getAllByRole("img");
    expect(charts).toHaveLength(2);
    fireEvent.keyDown(charts[0] as Element, { key: "ArrowRight" });
    expect(screen.getAllByText("The control plane was offline")).toHaveLength(2);
    fireEvent.keyDown(charts[0] as Element, { key: "ArrowRight" });
    expect(screen.getByText("18.5%")).toBeInTheDocument();

    await user.click(screen.getByRole("radio", { name: "30 d" }));
    await vi.waitFor(() => expect(seenRanges).toContain(HistoryRange.HISTORY_RANGE_30D));
  });

  it("explains container not-running, before-create, and not-tracked gap states honestly", async () => {
    const t0 = Date.UTC(2026, 11, 20, 12, 0, 0);
    renderWithApi(<ContainerHistory serverId="srv_1" containerId={"b".repeat(64)} />, {
      servers: {
        getContainerHistory: () =>
          create(GetContainerHistoryResponseSchema, {
            since: timestampFromMs(t0),
            until: timestampFromMs(t0 + 240_000),
            points: [
              create(ContainerHistoryPointSchema, {
                time: timestampFromMs(t0),
                state: HistoryPointState.BEFORE_CREATE,
              }),
              create(ContainerHistoryPointSchema, {
                time: timestampFromMs(t0 + 60_000),
                state: HistoryPointState.NOT_RUNNING,
              }),
              create(ContainerHistoryPointSchema, {
                time: timestampFromMs(t0 + 120_000),
                state: HistoryPointState.NOT_TRACKED,
              }),
              create(ContainerHistoryPointSchema, {
                time: timestampFromMs(t0 + 180_000),
                state: HistoryPointState.COVERED,
                cpuPercent: 9.0,
                memoryBytes: 64n * 1024n * 1024n,
              }),
            ],
          }),
      },
    });

    await screen.findByRole("heading", { name: "CPU" });
    const charts = screen.getAllByRole("img");
    fireEvent.keyDown(charts[0] as Element, { key: "ArrowRight" });
    expect(screen.getAllByText("The container wasn't created yet")).toHaveLength(2);
    fireEvent.keyDown(charts[0] as Element, { key: "ArrowRight" });
    expect(screen.getAllByText("The container wasn't running")).toHaveLength(2);
    fireEvent.keyDown(charts[0] as Element, { key: "ArrowRight" });
    expect(screen.getAllByText("Not tracked (past the 128-container limit)")).toHaveLength(2);
  });

  it("reads stopped containers past the limit as wasn't running instead of untracked (N4)", async () => {
    const t0 = Date.UTC(2026, 11, 20, 12, 0, 0);
    renderWithApi(<ContainerHistory serverId="srv_1" containerId={"c".repeat(64)} stopped />, {
      servers: {
        getContainerHistory: () =>
          create(GetContainerHistoryResponseSchema, {
            since: timestampFromMs(t0),
            until: timestampFromMs(t0 + 120_000),
            untracked: true,
            points: [
              create(ContainerHistoryPointSchema, {
                time: timestampFromMs(t0),
                state: HistoryPointState.NOT_TRACKED,
              }),
              create(ContainerHistoryPointSchema, {
                time: timestampFromMs(t0 + 60_000),
                state: HistoryPointState.COVERED,
                cpuPercent: 4.5,
                memoryBytes: 32n * 1024n * 1024n,
              }),
            ],
          }),
      },
    });

    await screen.findByRole("heading", { name: "CPU" });
    const charts = screen.getAllByRole("img");
    fireEvent.keyDown(charts[0] as Element, { key: "ArrowRight" });
    expect(screen.getAllByText("The container wasn't running")).toHaveLength(2);
    expect(screen.queryByText("Not tracked (past the 128-container limit)")).not.toBeInTheDocument();
  });
});
