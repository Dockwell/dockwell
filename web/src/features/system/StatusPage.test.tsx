// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError } from "@connectrpc/connect";
import { BuildInfoSchema, GetVersionResponseSchema } from "@dockwell/api";
import { act, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import { activateLocale } from "../../app/i18n";
import { renderAt } from "../../test/render";

const build = create(BuildInfoSchema, {
  version: "v0.4.0",
  commit: "3f2a9c1d4e5b6a7f8091a2b3c4d5e6f708192a3b",
  commitTime: timestampFromDate(new Date(2026, 10, 3, 21, 40)),
  modified: true,
  goVersion: "go1.27.3",
  platform: "linux/arm64",
});

const answers = () => create(GetVersionResponseSchema, { build });

describe("StatusPage", () => {
  it("shows the running build", async () => {
    renderAt("/status", { system: { getVersion: answers } });

    expect(await screen.findByRole("heading", { level: 1, name: "The control plane is running" })).toBeVisible();
    const panel = screen.getByRole("region", { name: "This control plane" });
    expect(panel).toHaveTextContent("v0.4.0");
    expect(panel).toHaveTextContent("3f2a9c1d4e5b (modified)");
    expect(panel).toHaveTextContent("3 Nov 2026, 21:40");
    expect(panel).toHaveTextContent("linux/arm64");
    expect(panel).toHaveTextContent("Connected");
    expect(screen.getByRole("link", { name: "Dockwell" })).toHaveAttribute("href", "/");
    // What it says is what's so, and the way into the panel is a button (#802).
    expect(
      screen.getByText(`Dockwell answers on ${window.location.host}. Servers appear below as they join.`),
    ).toBeVisible();
    expect(screen.queryByText(/come next/)).toBeNull();
    expect(screen.getByRole("link", { name: "Open the panel" })).toHaveAttribute("href", "/");
  });

  it("copies the health check command", async () => {
    const writeText = vi.fn(async () => {});
    vi.stubGlobal("navigator", { ...navigator, clipboard: { writeText } });
    renderAt("/status", { system: { getVersion: answers } });

    await userEvent.click(await screen.findByRole("button", { name: "Copy command" }));
    expect(writeText).toHaveBeenCalledWith(`curl -fsS ${window.location.origin}/healthz`);
    expect(await screen.findByRole("button", { name: "Command copied" })).toBeVisible();
  });

  it("explains an unreachable API and retries", async () => {
    let fail = true;
    renderAt("/status", {
      system: {
        getVersion: () => {
          if (fail) throw new ConnectError("connection refused", Code.Unavailable);
          return answers();
        },
      },
    });

    expect(await screen.findByRole("heading", { level: 1, name: "Can't reach the control plane" })).toBeVisible();
    expect(screen.getByText(/didn't answer the API/)).toHaveTextContent(
      `The panel loaded, but ${window.location.host} didn't answer the API. Check that dockwell run is still running on the server, then retry.`,
    );
    expect(screen.getByText("connection refused")).toBeVisible();

    fail = false;
    await userEvent.click(screen.getByRole("button", { name: "Retry" }));
    expect(await screen.findByRole("heading", { level: 1, name: "The control plane is running" })).toBeVisible();
  });

  it("draws the page's head while it connects, and says what the wait is only when it runs long", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      const { container } = renderAt("/status", { system: { getVersion: () => new Promise(() => {}) } });
      // The head's shape at once, and no sentence: a fast answer flashes nothing.
      await waitFor(() => expect(container.querySelector(".status-reserve .dw-skeleton.is-text-title")).not.toBeNull());
      expect(screen.queryByText("Connecting to the control plane")).toBeNull();
      await act(() => vi.advanceTimersByTimeAsync(350));
      expect(screen.getByText("Connecting to the control plane")).toHaveClass("dw-sr");
      // Past a few seconds, what the wait is, in place.
      await act(() => vi.advanceTimersByTimeAsync(3000));
      expect(screen.getByText("The control server is slow to answer")).toBeVisible();
    } finally {
      vi.useRealTimers();
    }
  });

  it("speaks Simplified Chinese", async () => {
    await activateLocale("zh-CN");
    renderAt("/status", { system: { getVersion: answers } });
    expect(await screen.findByRole("heading", { level: 1, name: "控制平面正在运行" })).toBeVisible();
    expect(screen.getByRole("region", { name: "当前控制平面" })).toHaveTextContent("健康: 已连接");
    expect(document.documentElement.lang).toBe("zh-CN");
  });
});
