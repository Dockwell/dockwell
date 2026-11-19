// SPDX-License-Identifier: AGPL-3.0-only

import { Protocol } from "@dockwell/api";
import { afterEach, describe, expect, it } from "vitest";

import { activateLocale } from "../../app/i18n";
import { anApp } from "../../test/apps";
import { renderHookWithApi } from "../../test/render";
import { appPortsOf, knownFor, usePortLabel } from "./appPorts";

afterEach(async () => {
  await activateLocale("en");
});

describe("an app's ports", () => {
  it("lists the web page's port first, then what it publishes, then what it exposes, each once and TCP alone", () => {
    const app = anApp(
      "gitea",
      "gitea/gitea:1.24",
      {
        ports: [
          { hostPort: 2222, containerPort: 22 },
          { hostPort: 3000, containerPort: 3000 },
          { hostPort: 5353, containerPort: 53, protocol: Protocol.UDP },
        ],
        expose: ["3000/tcp", "9000", "8125/udp", "nonsense"],
      },
      { webPort: 3000 },
    );
    expect(appPortsOf(app)).toEqual([
      { port: 3000, web: true, published: 3000 },
      { port: 22, web: false, published: 2222 },
      { port: 9000, web: false, published: 0 },
    ]);
  });

  it("lists none on the server's own network, where a port is the server's", () => {
    expect(appPortsOf(anApp("host", "nginx:1.27", { hostNetwork: true }, { webPort: 80 }))).toEqual([]);
    expect(appPortsOf(undefined)).toEqual([]);
  });

  it("labels each by what serves on it, which wins over the web port's fallback", async () => {
    await activateLocale("en");
    const label = renderHookWithApi(usePortLabel, {}).result.current;
    expect(label({ port: 3000, web: true, published: 0 })).toBe("3000 · web page");
    expect(label({ port: 22, web: false, published: 2222 })).toBe("22 · SSH");
    // Redis alone: the web port falls back to its only published port, which is no web page.
    expect(label({ port: 6379, web: true, published: 6379 })).toBe("6379 · Redis");
    expect(label({ port: 9000, web: false, published: 0 })).toBe("9000");
    expect(knownFor(5432)).toBe("PostgreSQL");
    expect(knownFor(8080)).toBe("");
    await activateLocale("zh-CN");
    expect(renderHookWithApi(usePortLabel, {}).result.current({ port: 3000, web: true, published: 0 })).toBe(
      "3000 · 网页",
    );
  });
});
