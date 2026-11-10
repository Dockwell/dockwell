// SPDX-License-Identifier: AGPL-3.0-only

import { Protocol, type WatchChangeResponse, type WatchServersResponse } from "@dockwell/api";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { expectAccessible } from "../../test/a11y";
import { anApp, places, SERVER } from "../../test/apps";
import { webPlanned } from "../../test/changes";
import { type Api, feed, renderAt } from "../../test/render";
import { aServer, changed, snapshotComplete } from "../../test/servers";

const server = aServer({ id: SERVER, name: "hz-1", inventory: { addresses: ["203.0.113.7"] } });

function api(more: Api = {}): Api {
  const watch = feed<WatchServersResponse>();
  watch.push(changed(server), snapshotComplete);
  const alone = anApp("alone", "nginx:1.27");
  // Another app publishes 8080, and a container run by hand 8081.
  const holder = anApp("holder", "redis:7.4", { ports: [{ hostPort: 8080, containerPort: 6379 }] });
  return {
    resources: places(),
    services: { listServices: () => ({ services: [alone, holder] }), listServiceRevisions: () => ({}) },
    servers: {
      watchServers: watch.stream,
      // sshd holds 8082, outside Docker, which only the server can say.
      checkServerPorts: (req) => ({
        ports: req.ports.map((p) => ({
          port: p.port,
          udp: p.udp,
          held: p.port === 8082,
          by: p.port === 8082 ? "sshd" : "",
        })),
      }),
    },
    containers: {
      listContainers: () => ({
        containers: [{ name: "legacy-site", ports: [{ serverPort: 8081, containerPort: 80, protocol: "tcp" }] }],
      }),
    },
    changes: { watchChange: feed<WatchChangeResponse>().stream, discardChange: () => ({}) },
    ...more,
  };
}

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("Add a port", () => {
  it("asks about the free port it offers, past the ports first asked, and a UDP port over UDP", async () => {
    const user = userEvent.setup();
    const asked: { port: number; udp: boolean }[] = [];
    renderAt(
      "/apps/svc_alone?add=port",
      api({
        servers: {
          watchServers: api().servers?.watchServers,
          // Every TCP port below 8100 is held, and 5353 over UDP.
          checkServerPorts: (req) => ({
            ports: req.ports.map((p) => {
              asked.push({ port: p.port, udp: p.udp });
              const held = p.udp ? p.port === 5353 : p.port < 8100;
              return { port: p.port, udp: p.udp, held, by: held ? (p.udp ? "dnsmasq" : "sshd") : "" };
            }),
          }),
        },
      }),
    );
    const sheet = await screen.findByRole("dialog", { name: "Add a port to alone" });
    const host = within(sheet).getByLabelText<HTMLInputElement>("On the server");
    await waitFor(() => expect(host.value).toBe("8100"), { timeout: 5000 });
    // Then 8100 is asked about too, and stays offered, as it's free.
    await waitFor(() => expect(asked).toContainEqual({ port: 8100, udp: false }), { timeout: 5000 });
    expect(host.value).toBe("8100");
    await user.click(within(sheet).getByRole("button", { name: "Advanced" }));
    await user.click(within(sheet).getByRole("radio", { name: "UDP" }));
    await user.clear(host);
    await user.type(host, "5353");
    await waitFor(() => expect(host).toHaveAccessibleDescription(/dnsmasq uses it/), { timeout: 5000 });
    expect(asked).toContainEqual({ port: 5353, udp: true });
  });

  it("picks a free port on the server, says what holds one that isn't, and where it answers", async () => {
    const user = userEvent.setup();
    const asked: unknown[] = [];
    renderAt(
      "/apps/svc_alone?add=port",
      api({
        services: {
          listServices: api().services?.listServices,
          listServiceRevisions: () => ({}),
          addServicePort: (req) => {
            asked.push(req);
            return { change: webPlanned() };
          },
        },
      }),
    );
    const sheet = await screen.findByRole("dialog", { name: "Add a port to alone" });
    const host = within(sheet).getByLabelText<HTMLInputElement>("On the server");
    // 8080 and 8081 are held by what Dockwell knows of, and 8082 by sshd, as the server says.
    await waitFor(() => expect(host.value).toBe("8083"));
    expect(host).toHaveAccessibleDescription("Free");
    expect(within(sheet).getByText("203.0.113.7:8083")).toBeVisible();
    await expectAccessible(document.body);

    // A port the app listens on that's free on the server is the one it offers.
    await user.type(within(sheet).getByLabelText("In the app"), "3000");
    expect(host.value).toBe("3000");
    expect(within(sheet).getByText("203.0.113.7:3000")).toBeVisible();

    // A typed port someone holds says who, and offers a free one.
    await user.clear(host);
    await user.type(host, "8080");
    expect(within(sheet).getByRole("alert")).toHaveTextContent("holder uses it · Use 3000");
    await user.clear(host);
    await user.type(host, "8081");
    expect(within(sheet).getByRole("alert")).toHaveTextContent("legacy-site uses it");
    await user.clear(host);
    await user.type(host, "443");
    expect(within(sheet).getByRole("alert")).toHaveTextContent("Dockwell's proxy uses it");
    await user.click(within(sheet).getByRole("button", { name: "Use 3000" }));
    expect(host.value).toBe("3000");
    expect(host).toHaveAccessibleDescription("Free");
    // What holds a port outside Docker, the server says by name.
    await user.clear(host);
    await user.type(host, "8082");
    await waitFor(() => expect(within(sheet).getByRole("alert")).toHaveTextContent("sshd uses it · Use 3000"));
    await user.click(within(sheet).getByRole("button", { name: "Use 3000" }));

    // The protocol, and who reaches it, start at TCP from anywhere, and say so while closed.
    const advanced = within(sheet).getByRole("button", { name: "Advanced" });
    expect(advanced).toHaveAccessibleDescription("TCP · from anywhere");
    await user.click(advanced);
    await user.click(within(sheet).getByRole("radio", { name: "Only the server" }));
    expect(within(sheet).getByText("127.0.0.1:3000")).toBeVisible();
    await user.click(within(sheet).getByRole("button", { name: "Show it" }));
    expect(within(sheet).getByText(/^dockwell services/)).toHaveTextContent(
      "dockwell services add-port svc_alone --host-port 3000 --container-port 3000 --host-ip 127.0.0.1",
    );

    await user.click(within(sheet).getByRole("button", { name: "Review plan" }));
    expect(await screen.findByRole("dialog", { name: "Review plan" })).toBeVisible();
    expect(asked.at(-1)).toMatchObject({
      id: "svc_alone",
      hostPort: 3000,
      containerPort: 3000,
      protocol: Protocol.TCP,
      hostIp: "127.0.0.1",
    });
  });
});
