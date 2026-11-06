// SPDX-License-Identifier: AGPL-3.0-only

// Renders every artboard in design/mocks, light and dark, and fails on any
// problem the renderer reports, anything logged as an error or a warning, and
// any thrown error.

import { act } from "react";
import { createRoot } from "react-dom/client";
import { beforeAll, describe, expect, it, vi } from "vitest";

import { MockLibrary, type Problem } from "./format/render";
import { boards, loadMock, mockFiles } from "./mocks";
import { installDesignSystem } from "./runtime/design-system";

declare global {
  var IS_REACT_ACT_ENVIRONMENT: boolean | undefined;
}

beforeAll(() => {
  globalThis.IS_REACT_ACT_ENVIRONMENT = true;
  installDesignSystem();
});

it("canvas.json lists every mock file", () => {
  expect(boards.map((b) => b.file).sort()).toEqual(mockFiles);
  expect(mockFiles.length).toBeGreaterThan(0);
});

/** What an artboard draws, as text. */
async function textOf(file: string): Promise<string> {
  const library = new MockLibrary({ load: loadMock, onProblem: () => {} });
  await library.load(file);
  const container = document.createElement("div");
  document.body.append(container);
  const root = createRoot(container);
  try {
    await act(async () => root.render(library.element(file, {})));
    return container.textContent ?? "";
  } finally {
    act(() => root.unmount());
    container.remove();
  }
}

// The panel draws each of these, so a mock that leaves one out leaves it undesigned.
it("draws all seven of how an app runs", async () => {
  const text = await textOf("Apps-List.dc.html");
  for (const word of ["Healthy", "Running", "Starting", "Unhealthy", "Not running", "Paused", "Unknown"]) {
    expect(text).toContain(word);
  }
});

it.each([
  ["Apps-List-Unreachable.dc.html", ["can’t reach the control plane", "Unknown"]],
  ["Apps-List-Offline.dc.html", ["You're offline", "This browser is offline"]],
  ["App-Overview-Docker.dc.html", ["1 can't read Docker", "Nobody can say how api runs", "systemctl status docker"]],
])("draws what %s is for", async (file, words) => {
  const text = await textOf(file);
  for (const word of words) expect(text).toContain(word);
});

describe.each(["light", "dark"] as const)("%s theme", (theme) => {
  it.each(boards.map((b) => b.file))("renders %s", async (file) => {
    const problems: Problem[] = [];
    const logged: unknown[][] = [];
    for (const level of ["error", "warn"] as const) {
      vi.spyOn(console, level).mockImplementation((...args: unknown[]) => {
        logged.push([level, ...args]);
      });
    }
    const library = new MockLibrary({ load: loadMock, onProblem: (p) => problems.push(p) });
    const artboard = await library.load(file);
    const themed = "theme" in artboard.props;

    const container = document.createElement("div");
    document.body.append(container);
    const root = createRoot(container);
    try {
      await act(async () => root.render(library.element(file, themed ? { theme } : {})));

      expect(problems.map((p) => `${p.file}: ${p.message}`)).toEqual([]);
      expect(logged.map((args) => args.map(String).join(" "))).toEqual([]);
      expect(container.querySelector("[data-mockview-problem]")).toBeNull();
      expect(container.textContent?.trim()).not.toBe("");
      if (themed) expect(container.querySelector(`[data-theme="${theme}"]`)).not.toBeNull();
    } finally {
      act(() => root.unmount());
      container.remove();
    }
  });
});
