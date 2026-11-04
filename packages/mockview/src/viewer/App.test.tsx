// SPDX-License-Identifier: AGPL-3.0-only

import { act } from "react";
import { createRoot, type Root } from "react-dom/client";
import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from "vitest";

import { KEY_MESSAGE, type KeyMessage, viewerHash } from "../links";
import { boards } from "../mocks";
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

describe("shortcut keys", () => {
  const [first, second] = boards;
  const at = (file: string | undefined) => viewerHash({ file: file ?? null, theme: null, hash: "" });

  /** What the frame posts when a shortcut key is pressed inside the mock. */
  function fromFrame(message: KeyMessage, origin = location.origin) {
    act(() => window.dispatchEvent(new MessageEvent("message", { data: message, origin })));
  }

  it("step through the artboards and go back to the list from the viewer's own window", async () => {
    await open(at(first?.file));
    act(() => window.dispatchEvent(new KeyboardEvent("keydown", { key: "ArrowRight" })));
    expect(location.hash).toBe(at(second?.file));
    await open(at(second?.file));
    act(() => window.dispatchEvent(new KeyboardEvent("keydown", { key: "Escape" })));
    expect(location.hash).toBe("#/");
  });

  it("work while the mock in the frame has focus", async () => {
    await open(at(first?.file));
    fromFrame({ type: KEY_MESSAGE, shortcut: "next" });
    expect(location.hash).toBe(at(second?.file));
    await open(at(second?.file));
    fromFrame({ type: KEY_MESSAGE, shortcut: "previous" });
    expect(location.hash).toBe(at(first?.file));
    await open(at(first?.file));
    fromFrame({ type: KEY_MESSAGE, shortcut: "back" });
    expect(location.hash).toBe("#/");
  });

  it("ignore messages from another origin", async () => {
    await open(at(first?.file));
    fromFrame({ type: KEY_MESSAGE, shortcut: "next" }, "https://example.com");
    expect(location.hash).toBe(at(first?.file));
  });
});
