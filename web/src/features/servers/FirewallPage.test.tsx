// SPDX-License-Identifier: AGPL-3.0-only

import type { MessageInitShape } from "@bufbuild/protobuf";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import { Code } from "@connectrpc/connect";
import type { GetServerFirewallResponseSchema } from "@dockwell/api";
import {
  Connection,
  FirewallToolKind,
  FirewallVerdict,
  type GetServerFirewallResponse,
  ListenerReach,
  ListenerReachReason,
  type WatchServersResponse,
} from "@dockwell/api";
import { screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { activateLocale } from "../../app/i18n";
import { expectAccessible } from "../../test/a11y";
import { authError } from "../../test/auth";
import { type Api, feed, onPhone, renderAt } from "../../test/render";
import { aServer, changed, snapshotComplete } from "../../test/servers";

const hz1 = aServer({ id: "srv_1", name: "hz-1" });

const ufw = {
  kind: FirewallToolKind.UFW,
  active: true,
  defaultInbound: FirewallVerdict.DENY,
  rules: [
    { verdict: FirewallVerdict.ALLOW, port: "22/tcp", app: "OpenSSH" },
    { verdict: FirewallVerdict.ALLOW, port: "80,443/tcp", comment: "web" },
    { verdict: FirewallVerdict.ALLOW, port: "443/tcp", interface: "eth0" },
    { verdict: FirewallVerdict.ALLOW, port: "9000/tcp", destination: "203.0.113.5" },
    { verdict: FirewallVerdict.ALLOW, port: "5432/tcp", source: "203.0.113.0/24" },
  ],
};

// Docker lists a port published on every address once for IPv4 and once for IPv6;
// on a network without IPv6, its proxy takes the IPv6 one.
const published = [
  { container: "shop-web-1", hostIp: "0.0.0.0", hostPort: 8080, protocol: "tcp", containerPort: 80 },
  { container: "shop-web-1", hostIp: "::", hostPort: 8080, protocol: "tcp", containerPort: 80, proxied: true },
  { container: "metrics-1", hostIp: "127.0.0.1", hostPort: 9100, protocol: "tcp", containerPort: 9100 },
];

type Firewall = MessageInitShape<typeof GetServerFirewallResponseSchema>["firewall"];

async function renderFirewall(firewall: Firewall | Error, server = hz1, api: Api = {}) {
  const watch = feed<WatchServersResponse>();
  watch.push(changed(server), snapshotComplete);
  const getServerFirewall = vi.fn(() => {
    if (firewall instanceof Error) throw firewall;
    return { firewall: { time: timestampFromDate(new Date()), ...firewall } } as GetServerFirewallResponse;
  });
  const rendered = renderAt("/servers/srv_1/firewall", {
    ...api,
    servers: { watchServers: watch.stream },
    firewall: { getServerFirewall },
  });
  await screen.findByRole("heading", { level: 1, name: "hz-1" });
  return { ...rendered, getServerFirewall };
}

const rowsOf = (panel: HTMLElement) =>
  within(panel)
    .getAllByRole("row")
    .slice(1)
    .map((r) => r.textContent);

afterEach(() => {
  activateLocale("en");
  vi.unstubAllGlobals();
});

describe("a server's Firewall tab", () => {
  it("lists the firewall already there, read-only, with its default and its rules as ufw says them", async () => {
    await renderFirewall({ tools: [ufw], published });
    const panel = await screen.findByRole("region", { name: "ufw" });
    expect(panel).toHaveTextContent("On · read-only, as Dockwell didn't make it");
    expect(panel).toHaveTextContent("It drops what no rule lets in.");
    expect(rowsOf(panel)).toEqual([
      "Allow22/tcp (OpenSSH)Anywhere",
      "Allow80,443/tcpAnywhereweb",
      "Allow443/tcp on eth0Anywhere",
      "Allow203.0.113.5 9000/tcpAnywhere",
      "Allow5432/tcp203.0.113.0/24",
    ]);
    expect(screen.getByRole("link", { name: "Firewall" })).toHaveAttribute("aria-current", "page");
  });

  it("says how far what listens reaches past the firewalls that cover it, and why when it can't tell", async () => {
    const at = (port: number, address = "0.0.0.0") => ({ address, port, protocol: "tcp" });
    const unknown = (port: number, reason: ListenerReachReason, decidedBy: string) => ({
      ...at(port),
      reach: ListenerReach.UNKNOWN,
      decidedBy,
      reason,
    });
    await renderFirewall({
      tools: [ufw],
      listening: [
        { ...at(22), reach: ListenerReach.OPEN, decidedBy: "ufw" },
        { ...at(22, "::"), reach: ListenerReach.OPEN, decidedBy: "ufw", ipv6Only: true },
        {
          ...at(68),
          protocol: "udp",
          reach: ListenerReach.OPEN,
          decidedBy: "ufw",
          reason: ListenerReachReason.BUILT_IN,
        },
        { ...at(443, "10.0.0.5"), reach: ListenerReach.SOME, decidedBy: "ufw", interfaces: ["eth0"] },
        { ...at(5432, "::"), reach: ListenerReach.SOME, decidedBy: "ufw", sources: ["203.0.113.0/24"] },
        { ...at(3306, "10.0.0.5"), reach: ListenerReach.CLOSED, decidedBy: "ufw" },
        { ...at(6379), reach: ListenerReach.OPEN },
        unknown(7000, ListenerReachReason.APP_PROFILE, "ufw"),
        unknown(7001, ListenerReachReason.SERVICE, "firewalld"),
        unknown(7002, ListenerReachReason.RICH_RULE, "firewalld"),
        unknown(7003, ListenerReachReason.DESTINATION, "ufw"),
        unknown(7004, ListenerReachReason.ZONES, "firewalld"),
        unknown(7005, ListenerReachReason.UNREADABLE, "nftables"),
        unknown(7006, ListenerReachReason.OTHER_RULES, "nftables inet filter"),
        unknown(7007, ListenerReachReason.DOCKER_UNKNOWN, "ufw"),
      ],
    });
    const panel = await screen.findByRole("region", { name: "What listens on hz-1" });
    expect(
      within(panel)
        .getAllByRole("columnheader")
        .map((h) => h.textContent),
    ).toEqual(["Port", "Address", "Reach"]);
    expect(rowsOf(panel)).toEqual([
      // sshd's IPv6 socket is apart from its IPv4 one; Postgres's on "::" alone takes IPv4 too.
      "22/tcpEvery IPv4 addressOpen, as ufw allows it",
      "22/tcpEvery IPv6 addressOpen, as ufw allows it",
      "68/udpEvery IPv4 addressOpen to what ufw always lets in on this port: DHCP replies, or local discovery",
      "443/tcp10.0.0.5Only on eth0, as ufw says",
      "5432/tcpEvery addressOnly from 203.0.113.0/24, as ufw says",
      "3306/tcp10.0.0.5Closed by ufw",
      "6379/tcpEvery IPv4 addressOpen: nothing here closes it",
      "7000/tcpEvery IPv4 addressCan't tell: a ufw rule names an app profile without saying its ports",
      "7001/tcpEvery IPv4 addressCan't tell: firewalld lets in a service Dockwell doesn't know the ports of",
      "7002/tcpEvery IPv4 addressCan't tell: firewalld's rich rules for it run first, and Dockwell doesn't weigh them",
      "7003/tcpEvery IPv4 addressCan't tell: ufw lets it in to one of the server's addresses alone",
      "7004/tcpEvery IPv4 addressCan't tell: firewalld's zones treat it apart, by the interface traffic arrives on",
      "7005/tcpEvery IPv4 addressCan't tell: Dockwell can't read nftables",
      "7006/tcpEvery IPv4 addressCan't tell: rules in nftables inet filter may drop it, and Dockwell doesn't weigh them one by one",
      "7007/tcpEvery IPv4 addressCan't tell: Docker isn't answering, and this may be a port it publishes, which ufw doesn't cover",
    ]);
  });

  it("says a socket on :: takes IPv6 alone as the agent says, with Docker's IPv4 socket left out as published", async () => {
    await renderFirewall({
      tools: [ufw],
      published,
      // Docker's proxy on IPv6, as the agent sends it: its IPv4 socket goes past the rules, so it isn't here.
      listening: [
        {
          address: "::",
          port: 8080,
          protocol: "tcp",
          ipv6Only: true,
          reach: ListenerReach.CLOSED,
          decidedBy: "firewalld",
        },
      ],
    });
    const panel = await screen.findByRole("region", { name: "What listens on hz-1" });
    expect(rowsOf(panel)).toEqual(["8080/tcpEvery IPv6 addressClosed by firewalld"]);
  });

  it("says every port in words, in English and Chinese", async () => {
    const anyPort = {
      ...ufw,
      rules: [
        { verdict: FirewallVerdict.ALLOW, port: "Anywhere", source: "198.51.100.9" },
        { verdict: FirewallVerdict.DENY, port: "Anywhere/udp", interface: "eth1" },
      ],
    };
    const { unmount } = await renderFirewall({ tools: [anyPort] });
    expect(rowsOf(await screen.findByRole("region", { name: "ufw" }))).toEqual([
      "AllowAny port198.51.100.9",
      "DenyAny udp port on eth1Anywhere",
    ]);
    unmount();
    await activateLocale("zh-CN");
    await renderFirewall({ tools: [anyPort] });
    const rows = rowsOf(await screen.findByRole("region", { name: "ufw" }));
    expect(rows[0]).toContain("任意端口");
    expect(rows.join(" ")).not.toContain("Anywhere");
  });

  it("lists the ports Docker publishes, what its proxy takes, and the rules in DOCKER-USER", async () => {
    await renderFirewall({ tools: [ufw], published, dockerUserRules: 2 });
    const panel = await screen.findByRole("region", { name: "Ports Docker publishes" });
    expect(panel).toHaveTextContent(
      "Docker forwards these to their containers before ufw's or firewalld's rules see them, so those rules don't cover them, but for what Docker's proxy takes, as marked.",
    );
    expect(panel).toHaveTextContent(
      "Docker's DOCKER-USER chain holds 2 rules, which filter these, and which Dockwell doesn't weigh one by one.",
    );
    expect(rowsOf(panel)).toEqual([
      "8080/tcpEvery IPv4 addressshop-web-180",
      "8080/tcpEvery IPv6 addressThrough Docker's proxy, so the input rules cover it.shop-web-180",
      "9100/tcp127.0.0.1metrics-19100",
    ]);
  });

  it("reads firewalld zone by zone, with what each takes and its rich rules", async () => {
    await renderFirewall({
      tools: [
        {
          kind: FirewallToolKind.FIREWALLD,
          name: "public",
          active: true,
          defaultZone: true,
          interfaces: ["eth0"],
          defaultInbound: FirewallVerdict.REJECT,
          rules: [{ verdict: FirewallVerdict.ALLOW, port: "ssh" }],
          other: ['rule family="ipv6" source address="2001:db8::/32" port port="5432" protocol="tcp" accept'],
        },
        {
          kind: FirewallToolKind.FIREWALLD,
          name: "trusted",
          active: true,
          sources: ["10.0.0.0/8"],
          defaultInbound: FirewallVerdict.ALLOW,
        },
      ],
    });
    const pub = await screen.findByRole("region", { name: "firewalld, zone public" });
    expect(pub).toHaveTextContent("Default zone · read-only");
    expect(pub).toHaveTextContent(
      "It takes traffic on eth0. It takes what no other zone does. It turns away what no rule lets in.",
    );
    expect(pub).toHaveTextContent("Rich rules, which run before the rest");
    expect(pub).toHaveTextContent('source address="2001:db8::/32"');
    expect(screen.getByRole("region", { name: "firewalld, zone trusted" })).toHaveTextContent(
      "It takes traffic from 10.0.0.0/8. It lets in what no rule turns away.",
    );
  });

  it("puts the kernel's other rules in one panel below what listens, and never says nothing filters while one can't be read", async () => {
    const { container } = await renderFirewall({
      tools: [
        {
          kind: FirewallToolKind.NFTABLES,
          name: "inet filter",
          active: true,
          ruleCount: 3,
          inputPolicy: FirewallVerdict.DENY,
          inputRules: 3,
        },
        { kind: FirewallToolKind.IPTABLES_LEGACY, name: "IPv4", active: true, ruleCount: 2, inputRules: 1 },
        {
          kind: FirewallToolKind.IPTABLES_LEGACY,
          name: "IPv6",
          unreadable: "Dockwell can't read iptables-legacy (IPv6): it didn't answer within 10s",
        },
      ],
      listening: [
        {
          address: "0.0.0.0",
          port: 22,
          protocol: "tcp",
          reach: ListenerReach.UNKNOWN,
          decidedBy: "nftables inet filter",
          reason: ListenerReachReason.OTHER_RULES,
        },
      ],
    });
    const other = await screen.findByRole("region", { name: "The kernel's other rules" });
    expect(rowsOf(other)).toEqual([
      "nftables table inet filter3 rules. Its input chain drops what no rule lets in.",
      "iptables-legacy, IPv42 rules, 1 rule of them on what comes in.",
      "iptables-legacy, IPv6Dockwell can't read iptables-legacy (IPv6): it didn't answer within 10s.",
    ]);
    const regions = [...container.querySelectorAll("section[aria-labelledby], [role=region]")].map(
      (r) => r.getAttribute("aria-label") ?? r.querySelector("h2, h3")?.textContent,
    );
    expect(regions.indexOf("The kernel's other rules")).toBeGreaterThan(regions.indexOf("What listens on hz-1"));
    expect(screen.queryByText(/Nothing filters/)).toBeNull();
  });

  it("says why it couldn't read ufw in words that keep ufw's name, and when nothing filters what comes in", async () => {
    const { unmount } = await renderFirewall({
      tools: [
        {
          kind: FirewallToolKind.UFW,
          unreadable:
            "Dockwell can't read ufw: it needs root, and the agent doesn't run as root here. Run the agent as root, as the install script does",
        },
      ],
    });
    expect(await screen.findByRole("region", { name: "ufw" })).toHaveTextContent(
      "Dockwell can't read ufw: it needs root, and the agent doesn't run as root here. Run the agent as root, as the install script does.",
    );
    unmount();
    await renderFirewall({ tools: [{ kind: FirewallToolKind.UFW, active: false }] });
    expect(
      await screen.findByText(
        "Nothing filters what comes in to hz-1: it has no ufw, firewalld, nftables or iptables rules that could.",
      ),
    ).toBeVisible();
    expect(screen.queryByRole("region", { name: "Ports Docker publishes" })).toBeNull();
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

  it("says a role doesn't cover the firewall as every page does, and something that broke as that", async () => {
    const refused = await renderFirewall(
      authError(Code.PermissionDenied, "needs_role", "this needs the admin role on the install"),
    );
    const why = (await screen.findByText("Your role doesn't cover this")).closest(".dw-callout");
    expect(why).toHaveClass("dw-tone-warning");
    expect(why).toHaveTextContent("Your role doesn't let you do this here. Ask an admin to change your role.");
    refused.unmount();

    await renderFirewall(authError(Code.Internal, "", "the control plane couldn't complete the request"));
    const broke = (await screen.findByText("Dockwell can't read hz-1's firewall")).closest(".dw-callout");
    expect(broke).toHaveClass("dw-tone-critical");
    expect(broke).toHaveTextContent("The control plane couldn't complete the request");
  });

  it("says how to update an agent older than the control plane", async () => {
    await renderFirewall(
      authError(Code.FailedPrecondition, "agent_outdated", "the agent on hz-1 is older than the control plane"),
    );
    expect(
      await screen.findByText(
        "The agent on hz-1 is older than the control plane, so it can't read the firewall. Run the install script on hz-1 again to update it.",
      ),
    ).toBeVisible();
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

  it("keeps reach on a phone's screen, with the address under the port", async () => {
    onPhone();
    await renderFirewall({
      tools: [ufw],
      published,
      listening: [{ address: "0.0.0.0", port: 22, protocol: "tcp", reach: ListenerReach.OPEN, decidedBy: "ufw" }],
    });
    const panel = await screen.findByRole("region", { name: "What listens on hz-1" });
    expect(
      within(panel)
        .getAllByRole("columnheader")
        .map((h) => h.textContent),
    ).toEqual(["Port", "Reach"]);
    expect(rowsOf(panel)).toEqual(["22/tcpEvery IPv4 addressOpen, as ufw allows it"]);
    const ports = screen.getByRole("region", { name: "Ports Docker publishes" });
    expect(
      within(ports)
        .getAllByRole("columnheader")
        .map((h) => h.textContent),
    ).toEqual(["Port", "Container"]);
    // Docker's proxy note sits on a line of its own, under the address.
    const proxied = within(ports).getByText("Through Docker's proxy, so the input rules cover it.");
    expect(proxied).toHaveClass("firewall-line");
    expect(proxied.previousElementSibling).toHaveTextContent("Every IPv6 address");
  });

  it("passes axe, and speaks Simplified Chinese", async () => {
    const { container, unmount } = await renderFirewall({ tools: [ufw], published });
    await screen.findByRole("region", { name: "ufw" });
    await expectAccessible(container);
    unmount();
    await activateLocale("zh-CN");
    await renderFirewall({
      tools: [ufw],
      published,
      listening: [{ address: "0.0.0.0", port: 22, protocol: "tcp", reach: ListenerReach.OPEN, decidedBy: "ufw" }],
    });
    expect(await screen.findByRole("region", { name: "Docker 发布的端口" })).toBeVisible();
    expect(screen.getAllByRole("columnheader", { name: "地址" }).length).toBeGreaterThan(0);
  });
});
