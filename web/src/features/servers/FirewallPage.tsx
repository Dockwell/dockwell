// SPDX-License-Identifier: AGPL-3.0-only

import { useQuery } from "@connectrpc/connect-query";
import {
  Connection,
  type FirewallSeenRule,
  FirewallService,
  type FirewallTool,
  FirewallToolKind,
  FirewallVerdict,
  type HostListener,
  ListenerReach,
  ListenerReachReason,
  type PublishedPort,
  type Server,
  type ServerFirewall,
} from "@dockwell/api";
import { Callout, Panel, PHONE_QUERY, Table, type TableColumn, useMediaQuery } from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";

import { reasonOf, sentence } from "../../lib/errors";
import { ServerFrame } from "./ServerFrame";
import "./FirewallPage.css";

/** How often the page reads the firewall again while it's open. */
const READ_EVERY_MS = 30_000;

/**
 * A server's Firewall tab, as Server-Firewall.dc.html has it, as far as reading
 * goes: the firewalls already there, ufw and firewalld's zones with their rules,
 * how far what listens reaches past them, the ports Docker publishes, which reach
 * their containers past those, and the kernel's other rules, which Dockwell
 * doesn't weigh one by one. Changing them comes with Dockwell's own rules
 * (ADR 0052).
 */
export function FirewallPage({ id }: { id: string }) {
  return <ServerFrame id={id}>{(server) => <Firewall server={server} />}</ServerFrame>;
}

/** ufw and firewalld, which the tab reads rule by rule. */
function weighed(tool: FirewallTool): boolean {
  return tool.kind === FirewallToolKind.UFW || tool.kind === FirewallToolKind.FIREWALLD;
}

function Firewall({ server }: { server: Server }) {
  const { t } = useLingui();
  const connected = server.connection === Connection.CONNECTED;
  const read = useQuery(
    FirewallService.method.getServerFirewall,
    { serverId: server.id },
    { enabled: connected, refetchInterval: READ_EVERY_MS, retry: false },
  );
  if (!connected) {
    return (
      <Callout tone="info">{t`${server.name} isn't reporting, so what filters its traffic can't be read now.`}</Callout>
    );
  }
  if (read.error) {
    if (reasonOf(read.error) === "agent_outdated") {
      return (
        <Callout tone="warning">
          <Trans>
            The agent on {server.name} is older than the control plane, so it can't read the firewall. Run the install
            script on {server.name} again to update it.
          </Trans>
        </Callout>
      );
    }
    return <Callout tone="critical">{sentence(read.error)}</Callout>;
  }
  const fw = read.data?.firewall;
  if (!fw) {
    return (
      <p className="firewall-note" aria-busy="true">
        <Trans>Reading what filters {server.name}'s traffic</Trans>
      </p>
    );
  }
  const there = fw.tools.filter((tool) => tool.active || tool.unreadable);
  const primary = there.filter(weighed);
  const others = there.filter((tool) => !weighed(tool));
  const filtered = primary.some((tool) => tool.active && !tool.unreadable);
  return (
    <div className="firewall">
      {there.length === 0 ? (
        <Panel title={t`Inbound traffic`}>
          <p className="firewall-note">
            <Trans>
              Nothing filters what comes in to {server.name}: it has no ufw, firewalld, nftables or iptables rules that
              could.
            </Trans>
          </p>
        </Panel>
      ) : (
        primary.map((tool) => <ToolPanel key={`${tool.kind}-${tool.name}`} tool={tool} />)
      )}
      <ListeningPanel server={server} listening={fw.listening} />
      <PublishedPanel fw={fw} filtered={filtered} />
      {others.length > 0 ? <OtherRulesPanel tools={others} /> : null}
    </div>
  );
}

interface RuleRow {
  id: string;
  rule: FirewallSeenRule;
}

/** A ufw rule's "To" as ufw says it: an address, a port, its app profile, and the interface. */
function ruleTo(rule: FirewallSeenRule): string {
  const port = rule.port === "Anywhere" && rule.destination ? "" : rule.port;
  return [rule.destination, port, rule.app ? `(${rule.app})` : "", rule.interface ? `on ${rule.interface}` : ""]
    .filter(Boolean)
    .join(" ");
}

/** One firewall Dockwell didn't make: what it is, its default, and its rules, read-only. */
function ToolPanel({ tool }: { tool: FirewallTool }) {
  const { t } = useLingui();
  const words = useVerdictWords();
  const title =
    tool.kind === FirewallToolKind.UFW ? t`ufw` : tool.name ? t`firewalld, zone ${tool.name}` : t`firewalld`;
  const rows: RuleRow[] = tool.rules.map((rule, i) => ({ id: `${i}`, rule }));
  const columns: TableColumn<RuleRow>[] = [
    {
      key: "verdict",
      header: t`Action`,
      width: 96,
      render: ({ rule }) => <strong>{words[rule.verdict] ?? "–"}</strong>,
    },
    { key: "port", header: t`Port`, mono: true, render: ({ rule }) => ruleTo(rule) },
    {
      key: "source",
      header: t`From`,
      render: ({ rule }) => (rule.source ? <code>{rule.source}</code> : rule.ipv6 ? t`Anywhere, IPv6` : t`Anywhere`),
    },
    { key: "comment", header: t`Note`, muted: true, render: ({ rule }) => rule.comment },
  ];
  return (
    <Panel
      title={title}
      actions={
        tool.unreadable ? null : (
          <span className="firewall-meta">
            {tool.defaultZone ? t`Default zone · read-only` : t`On · read-only, as Dockwell didn't make it`}
          </span>
        )
      }
      flush
      className="firewall-tool"
    >
      {tool.unreadable ? (
        <p className="firewall-note">{sentence(new Error(tool.unreadable))}</p>
      ) : (
        <>
          <p className="firewall-note">
            <ZoneWords tool={tool} /> <DefaultWords tool={tool} />
          </p>
          {tool.rules.length > 0 ? (
            <Table columns={columns} rows={rows} rowKey="id" caption={t`Rules of ${title}`} density="compact" />
          ) : null}
          {tool.other.length > 0 ? (
            <div className="firewall-other">
              <span className="firewall-other-label">{t`Rich rules, which run before the rest`}</span>
              {tool.other.map((rule) => (
                <code key={rule}>{rule}</code>
              ))}
            </div>
          ) : null}
        </>
      )}
    </Panel>
  );
}

/** What traffic a firewalld zone takes. */
function ZoneWords({ tool }: { tool: FirewallTool }) {
  const { t } = useLingui();
  if (tool.kind !== FirewallToolKind.FIREWALLD) return null;
  const parts: string[] = [];
  if (tool.sources.length > 0) {
    const sources = tool.sources.join(", ");
    parts.push(t`It takes traffic from ${sources}.`);
  }
  if (tool.interfaces.length > 0) {
    const ifaces = tool.interfaces.join(", ");
    parts.push(t`It takes traffic on ${ifaces}.`);
  }
  if (tool.defaultZone) parts.push(t`It takes what no other zone does.`);
  return <>{parts.join(" ")}</>;
}

/** What a firewall does with inbound traffic no rule matches, in a sentence. */
function DefaultWords({ tool }: { tool: FirewallTool }) {
  const { t } = useLingui();
  switch (tool.defaultInbound) {
    case FirewallVerdict.ALLOW:
      return t`It lets in what no rule turns away.`;
    case FirewallVerdict.DENY:
      return t`It drops what no rule lets in.`;
    case FirewallVerdict.REJECT:
      return t`It turns away what no rule lets in.`;
    default:
      return t`It doesn't say what it does with what no rule matches.`;
  }
}

function useVerdictWords(): Partial<Record<FirewallVerdict, string>> {
  const { t } = useLingui();
  return {
    [FirewallVerdict.ALLOW]: t`Allow`,
    [FirewallVerdict.DENY]: t`Deny`,
    [FirewallVerdict.REJECT]: t`Reject`,
    [FirewallVerdict.LIMIT]: t`Limit`,
  };
}

/**
 * The address a port is bound to, in words when it's every address. A socket on
 * "::" takes IPv4 too, as Linux has it by default, unless the same port is also
 * bound on "0.0.0.0", which then takes IPv4.
 */
function BoundTo({ address, ipv4Apart }: { address: string; ipv4Apart: boolean }) {
  const { t } = useLingui();
  if (address === "") return t`Every address`;
  if (address === "0.0.0.0") return t`Every IPv4 address`;
  if (address === "::") return ipv4Apart ? t`Every IPv6 address` : t`Every address`;
  return <code>{address}</code>;
}

/** The ports, by protocol, bound on every IPv4 address apart from IPv6. */
function boundOnIPv4(ports: readonly { address: string; port: number; protocol: string }[]): Set<string> {
  return new Set(ports.filter((p) => p.address === "0.0.0.0").map((p) => `${p.protocol} ${p.port}`));
}

/** How far a listener reaches, and why, in words. */
function useReachWords(): (l: HostListener) => string {
  const { t } = useLingui();
  return (l) => {
    const by = l.decidedBy;
    switch (l.reach) {
      case ListenerReach.OPEN:
        if (l.reason === ListenerReachReason.BUILT_IN) {
          return t`Open to what ${by} always lets in on this port: DHCP replies, or local discovery`;
        }
        return by ? t`Open, as ${by} allows it` : t`Open: nothing here closes it`;
      case ListenerReach.SOME: {
        const sources = l.sources.join(", ");
        const ifaces = l.interfaces.join(", ");
        if (sources && ifaces) return t`Only from ${sources}, or on ${ifaces}, as ${by} says`;
        if (ifaces) return t`Only on ${ifaces}, as ${by} says`;
        return t`Only from ${sources}, as ${by} says`;
      }
      case ListenerReach.CLOSED:
        return t`Closed by ${by}`;
    }
    switch (l.reason) {
      case ListenerReachReason.APP_PROFILE:
        return t`Can't tell: a ${by} rule names an app profile without saying its ports`;
      case ListenerReachReason.SERVICE:
        return t`Can't tell: ${by} lets in a service Dockwell doesn't know the ports of`;
      case ListenerReachReason.RICH_RULE:
        return t`Can't tell: ${by}'s rich rules for it run first, and Dockwell doesn't weigh them`;
      case ListenerReachReason.INTERFACE:
        return t`Can't tell: a ${by} rule for it covers one interface alone`;
      case ListenerReachReason.DESTINATION:
        return t`Can't tell: ${by} lets it in to one of the server's addresses alone`;
      case ListenerReachReason.ZONES:
        return t`Can't tell: ${by}'s zones treat it apart, by the interface traffic arrives on`;
      case ListenerReachReason.UNREADABLE:
        return t`Can't tell: Dockwell can't read ${by}`;
      case ListenerReachReason.OTHER_RULES:
        return t`Can't tell: rules in ${by} may drop it, and Dockwell doesn't weigh them one by one`;
      case ListenerReachReason.DOCKER_UNKNOWN:
        return t`Can't tell: Docker isn't answering, and this may be a port it publishes, which ${by} doesn't cover`;
      default:
        return t`Can't tell`;
    }
  };
}

interface ListenerRow {
  id: string;
  listener: HostListener;
}

/** What listens on the server itself, and how far each reaches past the firewalls that cover it. */
function ListeningPanel({ server, listening }: { server: Server; listening: readonly HostListener[] }) {
  const { t } = useLingui();
  const phone = useMediaQuery(PHONE_QUERY);
  const reach = useReachWords();
  if (listening.length === 0) return null;
  const rows: ListenerRow[] = listening.map((l) => ({ id: `${l.protocol}-${l.address}-${l.port}`, listener: l }));
  const ipv4 = boundOnIPv4(listening);
  const where = (l: HostListener) => <BoundTo address={l.address} ipv4Apart={ipv4.has(`${l.protocol} ${l.port}`)} />;
  const columns: TableColumn<ListenerRow>[] = phone
    ? [
        {
          key: "port",
          header: t`Port`,
          render: ({ listener: l }) => (
            <span className="firewall-stack">
              <code>{`${l.port}/${l.protocol}`}</code>
              <span className="firewall-sub">{where(l)}</span>
            </span>
          ),
        },
        { key: "reach", header: t`Reach`, render: ({ listener: l }) => reach(l) },
      ]
    : [
        {
          key: "port",
          header: t`Port`,
          width: 120,
          mono: true,
          render: ({ listener: l }) => `${l.port}/${l.protocol}`,
        },
        { key: "address", header: t`Address`, width: 180, render: ({ listener: l }) => where(l) },
        { key: "reach", header: t`Reach`, render: ({ listener: l }) => reach(l) },
      ];
  return (
    <Panel
      title={t`What listens on ${server.name}`}
      actions={<span className="firewall-meta">{plural(listening.length, { one: "# port", other: "# ports" })}</span>}
      flush
    >
      <Table columns={columns} rows={rows} rowKey="id" caption={t`What listens on ${server.name}`} density="compact" />
    </Panel>
  );
}

interface PortRow {
  id: string;
  port: PublishedPort;
}

/**
 * The ports Docker publishes, which reach their containers before ufw's and
 * firewalld's input rules see them, but for those Docker's proxy takes, and the
 * rules in DOCKER-USER, which filter them.
 */
function PublishedPanel({ fw, filtered }: { fw: ServerFirewall; filtered: boolean }) {
  const { t } = useLingui();
  const phone = useMediaQuery(PHONE_QUERY);
  const ports = fw.published;
  if (fw.publishedUnreadable) {
    return (
      <Panel title={t`Ports Docker publishes`}>
        <p className="firewall-note">{sentence(new Error(fw.publishedUnreadable))}</p>
      </Panel>
    );
  }
  if (ports.length === 0) return null;
  const rows: PortRow[] = ports.map((port) => ({ id: `${port.hostIp}-${port.hostPort}-${port.protocol}`, port }));
  const ipv4 = boundOnIPv4(ports.map((p) => ({ address: p.hostIp, port: p.hostPort, protocol: p.protocol })));
  const where = (p: PublishedPort) => (
    <>
      <BoundTo address={p.hostIp} ipv4Apart={ipv4.has(`${p.protocol} ${p.hostPort}`)} />
      {p.proxied ? (
        <span className="firewall-sub">{t`through Docker's proxy, so the input rules cover it`}</span>
      ) : null}
    </>
  );
  const columns: TableColumn<PortRow>[] = phone
    ? [
        {
          key: "port",
          header: t`Port`,
          render: ({ port: p }) => (
            <span className="firewall-stack">
              <code>{`${p.hostPort}/${p.protocol}`}</code>
              <span className="firewall-sub">{where(p)}</span>
            </span>
          ),
        },
        {
          key: "container",
          header: t`Container`,
          render: ({ port: p }) => <code>{`${p.container}:${p.containerPort}`}</code>,
        },
      ]
    : [
        {
          key: "port",
          header: t`Port`,
          width: 120,
          mono: true,
          render: ({ port: p }) => `${p.hostPort}/${p.protocol}`,
        },
        {
          key: "address",
          header: t`Address`,
          width: 220,
          render: ({ port: p }) => <span className="firewall-stack">{where(p)}</span>,
        },
        { key: "container", header: t`Container`, mono: true, render: ({ port: p }) => p.container },
        {
          key: "to",
          header: t`Its port`,
          width: 96,
          mono: true,
          align: "right",
          render: ({ port: p }) => `${p.containerPort}`,
        },
      ];
  return (
    <Panel
      title={t`Ports Docker publishes`}
      actions={<span className="firewall-meta">{plural(ports.length, { one: "# port", other: "# ports" })}</span>}
      flush
    >
      {filtered ? (
        <p className="firewall-note">
          <Trans>
            Docker forwards these to their containers before ufw's or firewalld's rules see them, so those rules don't
            cover them, but for what Docker's proxy takes, as marked.
          </Trans>
        </p>
      ) : null}
      {fw.dockerUserRules > 0 ? (
        <p className="firewall-note">
          {plural(fw.dockerUserRules, {
            one: "Docker's DOCKER-USER chain holds # rule, which filters these, and which Dockwell doesn't weigh.",
            other:
              "Docker's DOCKER-USER chain holds # rules, which filter these, and which Dockwell doesn't weigh one by one.",
          })}
        </p>
      ) : null}
      <Table columns={columns} rows={rows} rowKey="id" caption={t`Ports Docker publishes`} density="compact" />
    </Panel>
  );
}

interface OtherRow {
  id: string;
  tool: FirewallTool;
}

/** The kernel's other rules, which Dockwell doesn't weigh one by one, in one panel below the rest. */
function OtherRulesPanel({ tools }: { tools: readonly FirewallTool[] }) {
  const { t } = useLingui();
  const name = (tool: FirewallTool) => {
    switch (tool.kind) {
      case FirewallToolKind.IPTABLES_LEGACY:
        return tool.name ? t`iptables-legacy, ${tool.name}` : t`iptables-legacy`;
      case FirewallToolKind.IPTABLES:
        return tool.name ? t`iptables, ${tool.name}` : t`iptables`;
      default:
        return tool.name ? t`nftables table ${tool.name}` : t`nftables`;
    }
  };
  const what = (tool: FirewallTool) => {
    if (tool.unreadable) return sentence(new Error(tool.unreadable));
    const count = plural(tool.ruleCount, { one: "# rule", other: "# rules" });
    if (tool.inputPolicy === FirewallVerdict.DENY) {
      return t`${count}. Its input chain drops what no rule lets in.`;
    }
    if (tool.inputRules > 0) {
      const input = plural(tool.inputRules, { one: "# rule", other: "# rules" });
      return t`${count}, ${input} of them on what comes in.`;
    }
    return t`${count}, none on what comes in.`;
  };
  const rows: OtherRow[] = tools.map((tool) => ({ id: `${tool.kind}-${tool.name}`, tool }));
  const columns: TableColumn<OtherRow>[] = [
    { key: "name", header: t`Rules`, render: ({ tool }) => <code>{name(tool)}</code> },
    { key: "what", header: t`What they hold`, render: ({ tool }) => what(tool) },
  ];
  return (
    <Panel title={t`The kernel's other rules`} flush className="firewall-tool">
      <p className="firewall-note">
        <Trans>
          Rules ufw, firewalld and Docker didn't make. A drop in any of them wins, and Dockwell doesn't weigh them one
          by one, so where they may drop what listens above, it says it can't tell.
        </Trans>
      </p>
      <Table columns={columns} rows={rows} rowKey="id" caption={t`The kernel's other rules`} density="compact" />
    </Panel>
  );
}
