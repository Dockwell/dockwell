// SPDX-License-Identifier: AGPL-3.0-only

import { readdirSync, readFileSync } from "node:fs";
import { join, relative, resolve } from "node:path";
import { describe, expect, it } from "vitest";

// The panel uses only the public API, through the clients generated from
// the schema (CLAUDE.md, ADR 0003): nothing is UI-only, and nothing calls
// the control plane behind the schema's back. These tests read the panel's
// source and fail on any other way to reach the network.

// Vitest runs in the panel's package, web/.
const web = resolve("src");
const ui = resolve("../packages/ui/src");
const api = resolve("../packages/api/src");

/** The source files that ship, without tests, test helpers and generated routes. */
function sources(dir: string): string[] {
  const out: string[] = [];
  for (const entry of readdirSync(dir, { withFileTypes: true })) {
    const path = join(dir, entry.name);
    if (entry.isDirectory()) {
      if (entry.name !== "test" && entry.name !== "locales") out.push(...sources(path));
    } else if (/\.tsx?$/.test(entry.name) && !/\.test\.tsx?$/.test(entry.name) && entry.name !== "routeTree.gen.ts") {
      out.push(path);
    }
  }
  return out;
}

/** Comments say what code does, and may name what it must not do, so they are left out. */
function code(path: string): string {
  return readFileSync(path, "utf8")
    .replace(/\/\*[\s\S]*?\*\//g, "")
    .replace(/(^|[^:"'`])\/\/.*$/gm, "$1");
}

const FORBIDDEN: { what: string; pattern: RegExp }[] = [
  { what: "fetch", pattern: /(?<![\w$])fetch\s*\(/ },
  { what: "XMLHttpRequest", pattern: /\bXMLHttpRequest\b/ },
  { what: "a WebSocket", pattern: /\bWebSocket\b/ },
  { what: "an EventSource", pattern: /\bEventSource\b/ },
  { what: "sendBeacon", pattern: /\bsendBeacon\b/ },
  { what: "a REST path made by hand", pattern: /["'`]\/v1\// },
  { what: "a procedure path made by hand", pattern: /["'`]\/?dockwell\.v1\./ },
];

function offences(files: string[]): string[] {
  const out: string[] = [];
  for (const file of files) {
    const text = code(file);
    for (const { what, pattern } of FORBIDDEN) {
      if (pattern.test(text)) out.push(`${relative(web, file)} uses ${what}`);
    }
  }
  return out;
}

describe("the panel reaches the API only through the generated client", () => {
  const panel = sources(web);

  it("finds the panel's source", () => {
    expect(panel.length).toBeGreaterThan(50);
    expect(panel.some((f) => f.endsWith("app/api.ts"))).toBe(true);
  });

  it("never calls the network any other way", () => {
    expect(offences(panel)).toEqual([]);
  });

  // A browser can't send a stream's body over Connect, and a download is a link it
  // saves, so a file's bytes go over the REST routes the control plane streams (ADR
  // 0045). Those are the public API's, so they're reached the way the rest is: through
  // @dockwell/api, where one helper beside the generated client calls them, with their
  // method, path and media types generated from the OpenAPI document. That helper is
  // the one thing in the API's own source that calls the network, and the panel calls
  // it, never the routes.
  it("streams a file's bytes only through @dockwell/api's helper", () => {
    const own = sources(api).filter((f) => !relative(api, f).startsWith("gen/") && relative(api, f) !== "index.ts");
    expect(own.map((f) => relative(api, f))).toContain("streams.ts");
    expect(offences(own).map((o) => o.replace(/^.*packages\/api\/src\//, ""))).toEqual([
      "streams.ts uses XMLHttpRequest",
    ]);
    const helpers = ["uploadFilePart", "downloadFileURL"];
    const callers = panel.filter((f) => helpers.some((h) => new RegExp(`\\b${h}\\b`).test(code(f))));
    expect(callers.map((f) => relative(web, f)).sort()).toEqual([
      "features/files/FileViewer.tsx",
      "features/files/FilesPage.tsx",
      "features/files/upload.tsx",
    ]);
    for (const file of callers) {
      expect(code(file), relative(web, file)).toMatch(
        /import\s*\{[^}]*\b(uploadFilePart|downloadFileURL)\b[^}]*\}\s*from\s*"@dockwell\/api"/,
      );
    }
  });

  it("makes its transport in one place", () => {
    const transports = panel.filter((f) => /\bcreate(Connect|GrpcWeb|Grpc)Transport\b/.test(code(f)));
    expect(transports.map((f) => relative(web, f))).toEqual(["app/api.ts"]);
  });

  it("makes clients only for the generated services", () => {
    const wrong: string[] = [];
    for (const file of panel) {
      const text = code(file);
      for (const match of text.matchAll(/\bcreateClient\s*\(\s*([\w$]+)/g)) {
        const service = match[1] ?? "";
        const imported = new RegExp(`import\\s*\\{[^}]*\\b${service}\\b[^}]*\\}\\s*from\\s*"@dockwell/api"`).test(text);
        if (!imported)
          wrong.push(`${relative(web, file)} makes a client for ${service}, which isn't from @dockwell/api`);
      }
    }
    expect(wrong).toEqual([]);
  });

  it("leaves the design system without any calls at all", () => {
    const files = sources(ui);
    expect(files.length).toBeGreaterThan(20);
    expect(offences(files)).toEqual([]);
    expect(files.filter((f) => /@connectrpc|@dockwell\/api/.test(code(f))).map((f) => relative(ui, f))).toEqual([]);
  });

  it("catches what it is meant to", () => {
    const sample = [
      "await fetch('/v1/changes')",
      "window.fetch(url)",
      "new XMLHttpRequest()",
      "new WebSocket(url)",
      'const path = "/dockwell.v1.ChangeService/ListChanges"',
    ];
    for (const line of sample) {
      expect(
        FORBIDDEN.some(({ pattern }) => pattern.test(line)),
        line,
      ).toBe(true);
    }
    expect(FORBIDDEN.some(({ pattern }) => pattern.test("void tokens.refetch()"))).toBe(false);
  });
});
