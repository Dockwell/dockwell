// SPDX-License-Identifier: AGPL-3.0-only

import { describe, expect, it } from "vitest";

import {
  artboardLink,
  decodePart,
  frameUrl,
  isKeyMessage,
  isOpenMessage,
  KEY_MESSAGE,
  OPEN_MESSAGE,
  parseViewerHash,
  type Shortcut,
  shortcutFor,
  viewerHash,
} from "./links";

describe("artboardLink", () => {
  it("finds the artboard a link inside a mock points at", () => {
    expect(artboardLink("Apps-List.dc.html")).toEqual({ file: "Apps-List.dc.html", hash: "" });
    expect(artboardLink("./Alerts.dc.html#rules")).toEqual({ file: "Alerts.dc.html", hash: "#rules" });
    expect(artboardLink("/Main.dc.html")).toEqual({ file: "Main.dc.html", hash: "" });
  });

  it("ignores every other link", () => {
    for (const href of ["https://acme.dev", "mailto:dana@acme.dev", "#rules", "", "sub/x.html"]) {
      expect(artboardLink(href)).toBeNull();
    }
  });
});

describe("URLs", () => {
  it("builds frame URLs", () => {
    expect(frameUrl("Rail.dc.html", null)).toBe("frame.html?board=Rail.dc.html");
    expect(frameUrl("Alerts.dc.html", "dark", "#rules")).toBe("frame.html?board=Alerts.dc.html&theme=dark#rules");
  });

  it("round-trips viewer locations", () => {
    for (const route of [
      { file: null, theme: null, hash: "" },
      { file: "Overview-Single.dc.html", theme: "dark" as const, hash: "" },
      { file: "Alerts.dc.html", theme: null, hash: "#rules" },
    ]) {
      expect(parseViewerHash(viewerHash(route))).toEqual(route);
    }
    expect(viewerHash({ file: "Overview-Single.dc.html", theme: "light", hash: "" })).toBe(
      "#/Overview-Single.dc.html?theme=light",
    );
    expect(parseViewerHash("")).toEqual({ file: null, theme: null, hash: "" });
    expect(parseViewerHash("#/X.dc.html?theme=purple")).toEqual({ file: "X.dc.html", theme: null, hash: "" });
  });

  it("reads a fragment that isn't valid percent-encoding as written", () => {
    expect(parseViewerHash("#/50%off?theme=dark")).toEqual({ file: "50%off", theme: "dark", hash: "" });
    expect(parseViewerHash("#/A%20B.dc.html")).toEqual({ file: "A B.dc.html", theme: null, hash: "" });
    expect(decodePart("%E0%A4%A")).toBe("%E0%A4%A");
  });

  it("checks messages from the frame", () => {
    expect(isOpenMessage({ type: OPEN_MESSAGE, file: "A.dc.html", hash: "" })).toBe(true);
    expect(isOpenMessage({ type: OPEN_MESSAGE, file: 1 })).toBe(false);
    expect(isOpenMessage("x")).toBe(false);
    expect(isKeyMessage({ type: KEY_MESSAGE, shortcut: "next" })).toBe(true);
    expect(isKeyMessage({ type: KEY_MESSAGE, shortcut: "reload" })).toBe(false);
    expect(isKeyMessage({ type: OPEN_MESSAGE, shortcut: "back" })).toBe(false);
  });
});

describe("shortcutFor", () => {
  /** Presses a key on an element, as a person would, and returns the shortcut the document sees. */
  function press(target: Element, init: KeyboardEventInit, handled = false): Shortcut | null {
    let seen: Shortcut | null = null;
    const handle = (event: Event) => event.preventDefault();
    const read = (event: KeyboardEvent) => {
      seen = shortcutFor(event);
    };
    if (handled) target.addEventListener("keydown", handle);
    document.addEventListener("keydown", read);
    target.dispatchEvent(new KeyboardEvent("keydown", { bubbles: true, cancelable: true, ...init }));
    document.removeEventListener("keydown", read);
    target.removeEventListener("keydown", handle);
    return seen;
  }

  function place(html: string): HTMLElement {
    const box = document.createElement("div");
    box.innerHTML = html;
    document.body.append(box);
    return box;
  }

  it("turns the arrow keys and Escape into the viewer's shortcuts", () => {
    const box = place(`<button type="button">x</button>`);
    const button = box.querySelector("button") as Element;
    expect(press(button, { key: "ArrowLeft" })).toBe("previous");
    expect(press(button, { key: "ArrowRight" })).toBe("next");
    expect(press(button, { key: "Escape" })).toBe("back");
    expect(press(button, { key: "ArrowUp" })).toBeNull();
    expect(press(button, { key: "constructor" })).toBeNull();
    expect(press(document.body, { key: "Escape" })).toBe("back");
    box.remove();
  });

  it("leaves keys with a modifier, and keys the page already handled, alone", () => {
    const box = place(`<button type="button">x</button>`);
    const button = box.querySelector("button") as Element;
    expect(press(button, { key: "ArrowLeft", altKey: true })).toBeNull();
    expect(press(button, { key: "ArrowLeft", metaKey: true })).toBeNull();
    expect(press(button, { key: "ArrowRight", ctrlKey: true })).toBeNull();
    expect(press(button, { key: "ArrowRight" }, true)).toBeNull();
    box.remove();
  });

  it("leaves typing alone, in form fields and in editable content", () => {
    const box = place(
      `<input><textarea></textarea><select><option>a</option></select>` +
        `<div contenteditable="true"><b>rich</b></div><p contenteditable>plain</p>` +
        `<div contenteditable="false"><i>fixed</i></div>`,
    );
    for (const selector of ["input", "textarea", "select", "b", "p"]) {
      const target = box.querySelector(selector) as Element;
      expect({ selector, shortcut: press(target, { key: "ArrowLeft" }) }).toEqual({ selector, shortcut: null });
      expect({ selector, shortcut: press(target, { key: "Escape" }) }).toEqual({ selector, shortcut: null });
    }
    expect(press(box.querySelector("i") as Element, { key: "Escape" })).toBe("back");
    box.remove();
  });
});
