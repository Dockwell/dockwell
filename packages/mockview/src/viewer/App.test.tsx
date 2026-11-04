// SPDX-License-Identifier: AGPL-3.0-only

import { act } from "react";
import { createRoot, type Root } from "react-dom/client";
import { afterEach, beforeAll, beforeEach, expect, it, vi } from "vitest";

import { App } from "./App";

declare global {
  var IS_REACT_ACT_ENVIRONMENT: boolean | undefined;
}

let container: HTMLElement;
let root: Root;

beforeAll(() => {
  globalThis.IS_REACT_ACT_ENVIRONMENT = true;
});

beforeEach(() => {
  // jsdom has no ResizeObserver, which the artboard view uses to fit the frame to the window.
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      disconnect() {}
    },
  );
  container = document.createElement("div");
  document.body.append(container);
  root = createRoot(container);
});

afterEach(() => {
  act(() => root.unmount());
  container.remove();
  location.hash = "";
});

/** Opens the viewer at a fragment, as a link or the address bar would. */
async function open(hash: string) {
  location.hash = hash;
  await act(async () => root.render(<App />));
}

it("shows the not-found page for a fragment that isn't valid percent-encoding", async () => {
  await open("#/50%off");
  expect(container.querySelector("h1")?.textContent).toBe("No artboard called 50%off");
});
