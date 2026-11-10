// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { Protocol, ServerContainerSchema, ServiceInfoSchema } from "@dockwell/api";
import { describe, expect, it } from "vitest";

import { heldPorts, portsFrom } from "./ports";

const app = (id: string, name: string, server: string, ports: { host: number; udp?: boolean }[]) =>
  create(ServiceInfoSchema, {
    id,
    resource: { kind: "service", name },
    spec: {
      runs: {
        case: "app",
        value: {
          image: "nginx:1.27",
          ports: ports.map((p) => ({
            hostPort: p.host,
            containerPort: 80,
            protocol: p.udp ? Protocol.UDP : Protocol.TCP,
          })),
        },
      },
    },
    deployments: [{ serverId: server, serverName: server }],
  });

const services = [
  app("svc_web", "web", "srv_1", [{ host: 8080 }, { host: 8081 }]),
  app("svc_dns", "dns", "srv_1", [{ host: 5353, udp: true }]),
  app("svc_far", "far", "srv_2", [{ host: 9000 }]),
];
const containers = [
  [
    create(ServerContainerSchema, {
      name: "legacy-site",
      ports: [{ serverPort: 8090, containerPort: 80, protocol: "tcp" }],
    }),
  ],
];
const name = (h: { name: string; kind: string }) => (h.kind === "proxy" ? "the proxy" : h.name);

describe("the ports held on a server", () => {
  it("are its apps', the containers Dockwell didn't make, and the proxy's", () => {
    const ports = portsFrom(heldPorts(["srv_1"], services, containers), true);
    expect(ports.holder(8080)?.name).toBe("web");
    expect(ports.holder(8090)?.name).toBe("legacy-site");
    expect(ports.holder(443)?.kind).toBe("proxy");
    expect(ports.holder(443, true)?.kind).toBe("proxy");
    expect(ports.holder(5353)).toBeUndefined();
    expect(ports.holder(5353, true)?.name).toBe("dns");
    // An app on another server holds its port there, not here.
    expect(ports.holder(9000)).toBeUndefined();
  });

  it("leave out the app a form edits, as its own ports are its to keep", () => {
    const ports = portsFrom(heldPorts(["srv_1"], services, containers, "svc_web"), true);
    expect(ports.holder(8080)).toBeUndefined();
  });

  it("offer the first free one, from where the form starts", () => {
    const ports = portsFrom(heldPorts(["srv_1"], services, containers), true);
    expect(ports.free()).toBe(8082);
    expect(ports.free(3000)).toBe(3000);
    // The port an app listens on in its container, when that's a system's, isn't where to start.
    expect(ports.free(80)).toBe(8082);
    expect(ports.free(undefined, false, [8082, 8083])).toBe(8084);
    expect(ports.free(5353, true)).toBe(5354);
  });

  it("say of a typed port whether it's free, what holds it, or that it's still being read", () => {
    const ports = portsFrom(heldPorts(["srv_1"], services, containers), true);
    expect(ports.check("8080", false, name)).toEqual({ state: "taken", by: "web" });
    expect(ports.check("80", false, name)).toEqual({ state: "taken", by: "the proxy" });
    expect(ports.check("8083", false, name)).toEqual({ state: "free" });
    expect(ports.check("8083", false, name, [8083])).toEqual({ state: "taken" });
    expect(ports.check("", false, name)).toEqual({ state: "unknown" });
    expect(portsFrom([], false).check("8083", false, name)).toEqual({ state: "checking" });
  });
});
