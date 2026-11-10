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
    servers: { watchServers: watch.stream },
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
    // 8080 and 8081 are held, so the first free port is 8082.
    await waitFor(() => expect(host.value).toBe("8082"));
    expect(host).toHaveAccessibleDescription("Free");
    expect(within(sheet).getByText("203.0.113.7:8082")).toBeVisible();
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

  it("leaves a port someone types alone when the free one lands as they type", async () => {
    const user = userEvent.setup();
    const asked: { hostPort: number; containerPort: number }[] = [];
    // The server's ports come back only when told to, as on a slow server.
    let answer = () => {};
    const read = new Promise<void>((resolve) => (answer = resolve));
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
        containers: {
          listContainers: async () => {
            await read;
            return { containers: [] };
          },
        },
      }),
    );
    const sheet = await screen.findByRole("dialog", { name: "Add a port to alone" });
    const host = within(sheet).getByLabelText<HTMLInputElement>("On the server");
    // Into the field while it's still empty, and the free port lands before the first key.
    await user.click(host);
    answer();
    await waitFor(() => expect(within(sheet).getByRole("button", { name: "Use 8081" })).toBeVisible());
    expect(host.value).toBe("");
    await user.type(host, "94");
    await user.type(within(sheet).getByLabelText("In the app"), "8080");
    expect(host.value).toBe("94");
    await user.type(host, "11");
    expect(host.value).toBe("9411");
    expect(within(sheet).queryByText("Use a port from 1 to 65535.")).toBeNull();

    await user.click(within(sheet).getByRole("button", { name: "Review plan" }));
    expect(await screen.findByRole("dialog", { name: "Review plan" })).toBeVisible();
    expect(asked.at(-1)).toMatchObject({ hostPort: 9411, containerPort: 8080 });
  });

  it("follows the free port until someone types one, as passing through the field types nothing", async () => {
    const user = userEvent.setup();
    renderAt("/apps/svc_alone?add=port", api());
    const sheet = await screen.findByRole("dialog", { name: "Add a port to alone" });
    const host = within(sheet).getByLabelText<HTMLInputElement>("On the server");
    const inApp = within(sheet).getByLabelText("In the app");
    await waitFor(() => expect(host.value).toBe("8082"));
    await user.click(host);
    await user.tab();
    await user.type(inApp, "3000");
    expect(host.value).toBe("3000");
    await user.clear(host);
    await user.type(host, "3005");
    await user.clear(inApp);
    await user.type(inApp, "3001");
    expect(host.value).toBe("3005");
  });
});
