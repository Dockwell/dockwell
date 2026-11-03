// SPDX-License-Identifier: AGPL-3.0-only

import { describe, expect, it } from "vitest";

import { artboardLink, frameUrl, isOpenMessage, OPEN_MESSAGE, parseViewerHash, viewerHash } from "./links";

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

  it("checks messages from the frame", () => {
    expect(isOpenMessage({ type: OPEN_MESSAGE, file: "A.dc.html", hash: "" })).toBe(true);
    expect(isOpenMessage({ type: OPEN_MESSAGE, file: 1 })).toBe(false);
    expect(isOpenMessage("x")).toBe(false);
  });
});
