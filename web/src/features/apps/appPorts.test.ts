// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { PortAnswer, Protocol, ServicePortCheckSchema } from "@dockwell/api";
import { waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import { activateLocale } from "../../app/i18n";
import { anApp } from "../../test/apps";
import { renderHookWithApi } from "../../test/render";
import { appPortsOf, knownFor, usePortLabel, useServicePorts } from "./appPorts";

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

  it("adds what the server found, what the image exposes and what listens, after the spec's, with each answer (#791)", async () => {
    await activateLocale("en");
    const check = (port: number, answer: PortAnswer, said: string, more: object = {}) =>
      create(ServicePortCheckSchema, { port, answer, said, ...more });
    const app = anApp(
      "shop",
      "ghcr.io/acme/shop:1.4",
      { ports: [{ hostPort: 8080, containerPort: 8080 }] },
      { webPort: 8080 },
    );
    const found = [
      check(8080, PortAnswer.HTTP, "200 OK", { listening: true, serverPort: 8080 }),
      check(9000, PortAnswer.NOTHING, "nothing listens", { exposed: true }),
      check(2222, PortAnswer.OTHER, "SSH", { listening: true }),
      check(5000, PortAnswer.OTHER, "took the connection and said nothing", { listening: true }),
    ];
    const ports = appPortsOf(app, found);
    expect(ports.map((p) => p.port)).toEqual([8080, 9000, 2222, 5000]);
    const label = renderHookWithApi(usePortLabel, {}).result.current;
    expect(ports.map(label)).toEqual([
      "8080 · web page · answers 200 OK",
      "9000 · nothing listens",
      "2222 · SSH",
      "5000 · not HTTP",
    ]);
  });

  it("drops answers read before the app restarted, and asks again (the review's L4)", async () => {
    let asked = 0;
    let shop = anApp("shop", "ghcr.io/acme/shop:1.4", {}, { changeId: "chg_deploy" });
    const { result, rerender } = renderHookWithApi(() => useServicePorts(shop), {
      services: {
        checkServicePorts: () => {
          asked += 1;
          return asked === 1
            ? { ports: [{ port: 80, answer: PortAnswer.HTTP, said: "200 OK" }] }
            : { ports: [{ port: 80, answer: PortAnswer.NOTHING, said: "nothing listens" }] };
        },
      },
    });
    await waitFor(() => expect(result.current.checks[0]?.answer).toBe(PortAnswer.HTTP));
    // A restart is a change of its own: what was read before it is gone at once.
    shop = anApp("shop", "ghcr.io/acme/shop:1.4", {}, { changeId: "chg_restart" });
    rerender();
    expect(result.current.checks).toEqual([]);
    await waitFor(() => expect(result.current.checks[0]?.answer).toBe(PortAnswer.NOTHING));
    expect(asked).toBe(2);
  });

  it("says nothing listens on a port only where the list is whole (the review's M1)", async () => {
    const shop = anApp("shop", "ghcr.io/acme/shop:1.4");
    const { result } = renderHookWithApi(() => useServicePorts(shop), {
      services: {
        checkServicePorts: () => ({
          ports: [{ port: 80, listening: true, answer: PortAnswer.HTTP, said: "200 OK" }],
          cut: true,
        }),
      },
    });
    await waitFor(() => expect(result.current.checks).toHaveLength(1));
    expect(result.current.cut).toBe(true);
    expect(result.current.readListening).toBe(false);
  });
});
