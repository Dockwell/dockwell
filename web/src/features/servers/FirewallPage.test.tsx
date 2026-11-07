// SPDX-License-Identifier: AGPL-3.0-only

import type { MessageInitShape } from "@bufbuild/protobuf";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import type { GetServerFirewallResponseSchema } from "@dockwell/api";
import {
  Connection,
  FirewallToolKind,
  FirewallVerdict,
  type GetServerFirewallResponse,
  ListenerReach,
  type WatchServersResponse,
} from "@dockwell/api";
import { screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { activateLocale } from "../../app/i18n";
import { expectAccessible } from "../../test/a11y";
import { type Api, feed, renderAt } from "../../test/render";
import { aServer, changed, snapshotComplete } from "../../test/servers";

const hz1 = aServer({ id: "srv_1", name: "hz-1" });

const ufw = {
  kind: FirewallToolKind.UFW,
  active: true,
  defaultInbound: FirewallVerdict.DENY,
  rules: [
    { verdict: FirewallVerdict.LIMIT, port: "22/tcp" },
    { verdict: FirewallVerdict.ALLOW, port: "80,443/tcp", comment: "web" },
    { verdict: FirewallVerdict.ALLOW, port: "5432/tcp", source: "203.0.113.0/24" },
  ],
};

// Docker lists a port published on every address once for IPv4 and once for IPv6.
const published = [
  { container: "shop-web-1", hostIp: "0.0.0.0", hostPort: 8080, protocol: "tcp", containerPort: 80 },
  { container: "shop-web-1", hostIp: "::", hostPort: 8080, protocol: "tcp", containerPort: 80 },
  { container: "metrics-1", hostIp: "127.0.0.1", hostPort: 9100, protocol: "tcp", containerPort: 9100 },
];

async function renderFirewall(
  firewall: MessageInitShape<typeof GetServerFirewallResponseSchema>["firewall"],
  server = hz1,
  api: Api = {},
) {
  const watch = feed<WatchServersResponse>();
  watch.push(changed(server), snapshotComplete);
  const getServerFirewall = vi.fn(
    () => ({ firewall: { time: timestampFromDate(new Date()), ...firewall } }) as GetServerFirewallResponse,
  );
  const rendered = renderAt("/servers/srv_1/firewall", {
    ...api,
    servers: { watchServers: watch.stream },
    firewall: { getServerFirewall },
  });
  await screen.findByRole("heading", { level: 1, name: "hz-1" });
  return { ...rendered, getServerFirewall };
}

afterEach(() => {
  activateLocale("en");
});

describe("a server's Firewall tab", () => {
  it("lists the firewall already there, read-only, with its default and its rules", async () => {
    await renderFirewall({ tools: [ufw], published });
    const panel = await screen.findByRole("region", { name: "ufw" });
    expect(panel).toHaveTextContent("On · read-only, as Dockwell didn't make it");
    expect(panel).toHaveTextContent("It drops what no rule lets in.");
    const rows = within(panel).getAllByRole("row").slice(1);
    expect(rows.map((r) => r.textContent)).toEqual([
      "Limit22/tcpAnywhere",
      "Allow80,443/tcpAnywhereweb",
      "Allow5432/tcp203.0.113.0/24",
    ]);
    expect(screen.getByRole("link", { name: "Firewall" })).toHaveAttribute("aria-current", "page");
  });

  it("says how far what listens on the server reaches past the firewalls that cover it", async () => {
    await renderFirewall({
      tools: [ufw],
      listening: [
        { address: "0.0.0.0", port: 22, protocol: "tcp", reach: ListenerReach.OPEN, decidedBy: "ufw" },
        { address: "::", port: 22, protocol: "tcp", reach: ListenerReach.OPEN, decidedBy: "ufw" },
        {
          address: "::",
          port: 5432,
          protocol: "tcp",
          reach: ListenerReach.SOME,
          decidedBy: "ufw",
          sources: ["203.0.113.0/24"],
        },
        { address: "10.0.0.5", port: 3306, protocol: "tcp", reach: ListenerReach.CLOSED, decidedBy: "ufw" },
        { address: "0.0.0.0", port: 9090, protocol: "tcp", reach: ListenerReach.UNKNOWN, decidedBy: "ufw" },
      ],
    });
    const panel = await screen.findByRole("region", { name: "What listens on hz-1" });
    expect(
      within(panel)
        .getAllByRole("row")
        .slice(1)
        .map((r) => r.textContent),
    ).toEqual([
      // sshd's IPv6 socket is apart from its IPv4 one; Postgres's on "::" alone takes IPv4 too.
      "22/tcpEvery IPv4 addressOpen, as ufw allows it",
      "22/tcpEvery IPv6 addressOpen, as ufw allows it",
      "5432/tcpEvery addressOnly from 203.0.113.0/24, as ufw says",
      "3306/tcp10.0.0.5Closed by ufw",
      "9090/tcpEvery IPv4 addressCan't tell: ufw decides by names or rules it doesn't say the ports of",
    ]);
  });

  it("lists the ports Docker publishes, and that ufw's rules don't cover them", async () => {
    await renderFirewall({ tools: [ufw], published });
    const panel = await screen.findByRole("region", { name: "Ports Docker publishes" });
    expect(panel).toHaveTextContent(
      "Docker forwards these to their containers before ufw's or firewalld's rules see them",
    );
    expect(
      within(panel)
        .getAllByRole("row")
        .slice(1)
        .map((r) => r.textContent),
    ).toEqual([
      "8080/tcpEvery IPv4 addressshop-web-180",
      "8080/tcpEvery IPv6 addressshop-web-180",
      "9100/tcp127.0.0.1metrics-19100",
    ]);
  });

  it("still says the firewalls when Docker doesn't answer, and why its ports are missing", async () => {
    await renderFirewall({
      tools: [ufw],
      publishedUnreadable: "Docker didn't say which ports it publishes: the engine is restarting",
    });
    expect(await screen.findByRole("region", { name: "ufw" })).toBeVisible();
    expect(screen.getByRole("region", { name: "Ports Docker publishes" })).toHaveTextContent(
      "Docker didn't say which ports it publishes: the engine is restarting.",
    );
  });

  it("says why it couldn't read a firewall, and when nothing filters what comes in", async () => {
    const { unmount } = await renderFirewall({
      tools: [
        {
          kind: FirewallToolKind.NFTABLES,
          unreadable: "nftables needs root to say its rules, and the agent doesn't run as root here",
        },
      ],
    });
    expect(await screen.findByRole("region", { name: "nftables" })).toHaveTextContent(
      "Nftables needs root to say its rules, and the agent doesn't run as root here.",
    );
    unmount();
    await renderFirewall({ tools: [{ kind: FirewallToolKind.UFW, active: false }] });
    expect(await screen.findByText(/Nothing filters what comes in to hz-1/)).toBeVisible();
    expect(screen.queryByRole("region", { name: "Ports Docker publishes" })).toBeNull();
  });

  it("asks nothing of a server that isn't reporting, and says so", async () => {
    const { getServerFirewall } = await renderFirewall(
      {},
      aServer({ id: "srv_1", name: "hz-1", connection: Connection.UNREACHABLE }),
    );
    expect(
      await screen.findByText("hz-1 isn't reporting, so what filters its traffic can't be read now."),
    ).toBeVisible();
    expect(getServerFirewall).not.toHaveBeenCalled();
  });

  it("passes axe, and speaks Simplified Chinese", async () => {
    const { container, unmount } = await renderFirewall({ tools: [ufw], published });
    await screen.findByRole("region", { name: "ufw" });
    await expectAccessible(container);
    unmount();
    await activateLocale("zh-CN");
    await renderFirewall({ tools: [ufw], published });
    expect(await screen.findByRole("region", { name: "Docker 发布的端口" })).toBeVisible();
  });
});
