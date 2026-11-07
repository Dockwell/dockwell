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
  type PublishedPort,
  type Server,
} from "@dockwell/api";
import { Callout, Panel, Table, type TableColumn } from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";

import { sentence } from "../../lib/errors";
import { ServerFrame } from "./ServerFrame";
import "./FirewallPage.css";

/** How often the page reads the firewall again while it's open. */
const READ_EVERY_MS = 30_000;

/**
 * A server's Firewall tab, as Server-Firewall.dc.html has it, as far as reading
 * goes: the firewalls already there, ufw, firewalld and nftables's tables, with
 * what each allows and drops, and the ports Docker publishes, which reach their
 * containers past those. Changing them comes with Dockwell's own rules (ADR 0052).
 */
export function FirewallPage({ id }: { id: string }) {
  return <ServerFrame id={id}>{(server) => <Firewall server={server} />}</ServerFrame>;
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
  if (read.error) return <Callout tone="critical">{sentence(read.error)}</Callout>;
  const fw = read.data?.firewall;
  if (!fw) {
    return (
      <p className="firewall-note" aria-busy="true">
        <Trans>Reading what filters {server.name}'s traffic</Trans>
      </p>
    );
  }
  const tools = fw.tools.filter((tool) => tool.active || tool.unreadable);
  return (
    <div className="firewall">
      {tools.length === 0 ? (
        <Panel title={t`Inbound traffic`}>
          <p className="firewall-note">
            <Trans>
              Nothing filters what comes in to {server.name}: it has no ufw, firewalld or nftables rules turned on.
            </Trans>
          </p>
        </Panel>
      ) : (
        tools.map((tool) => <ToolPanel key={`${tool.kind}-${tool.name}`} tool={tool} />)
      )}
      <ListeningPanel server={server} listening={fw.listening} />
      <PublishedPanel
        ports={fw.published}
        unreadable={fw.publishedUnreadable}
        filtered={tools.some((tool) => tool.kind === FirewallToolKind.UFW || tool.kind === FirewallToolKind.FIREWALLD)}
      />
    </div>
  );
}

interface RuleRow {
  id: string;
  rule: FirewallSeenRule;
}

interface PortRow {
  id: string;
  port: PublishedPort;
}

/** One firewall Dockwell didn't make: what it is, its default, and its rules, read-only. */
function ToolPanel({ tool }: { tool: FirewallTool }) {
  const { t } = useLingui();
  const words = useVerdictWords();
  let title: string;
  switch (tool.kind) {
    case FirewallToolKind.UFW:
      title = t`ufw`;
      break;
    case FirewallToolKind.FIREWALLD:
      title = tool.name ? t`firewalld, zone ${tool.name}` : t`firewalld`;
      break;
    case FirewallToolKind.IPTABLES_LEGACY:
      title = t`iptables-legacy`;
      break;
    default:
      title = tool.name ? t`nftables table ${tool.name}` : t`nftables`;
  }
  const rows: RuleRow[] = tool.rules.map((rule, i) => ({ id: `${i}`, rule }));
  const columns: TableColumn<RuleRow>[] = [
    {
      key: "verdict",
      header: t`Action`,
      width: 104,
      render: ({ rule }) => <strong>{words[rule.verdict] ?? "–"}</strong>,
    },
    { key: "port", header: t`Port`, width: 160, mono: true, render: ({ rule }) => rule.port },
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
        tool.unreadable ? null : <span className="firewall-meta">{t`On · read-only, as Dockwell didn't make it`}</span>
      }
      flush
      className="firewall-tool"
    >
      {tool.unreadable ? (
        <p className="firewall-note">{sentence(new Error(tool.unreadable))}</p>
      ) : (
        <>
          <p className="firewall-note">
            <DefaultWords tool={tool} />
            {tool.kind === FirewallToolKind.NFTABLES || tool.kind === FirewallToolKind.IPTABLES_LEGACY
              ? ` ${plural(tool.ruleCount, { one: "It holds # rule, which ufw, Docker or a person may have made. Dockwell doesn't weigh it, so what listens below may reach less far than it says.", other: "It holds # rules, which ufw, Docker or a person may have made. Dockwell doesn't weigh them one by one, so what listens below may reach less far than it says." })}`
              : ""}
          </p>
          {tool.rules.length > 0 ? (
            <Table columns={columns} rows={rows} rowKey="id" caption={t`Rules of ${title}`} density="compact" />
          ) : null}
          {tool.other.length > 0 ? (
            <div className="firewall-other">
              <span className="firewall-other-label">{t`Rules as it says them`}</span>
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
      return tool.kind === FirewallToolKind.NFTABLES ? "" : t`It doesn't say what it does with what no rule matches.`;
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

interface ListenerRow {
  id: string;
  listener: HostListener;
}

/** What listens on the server itself, and how far each reaches past the firewalls that cover it. */
function ListeningPanel({ server, listening }: { server: Server; listening: readonly HostListener[] }) {
  const { t } = useLingui();
  if (listening.length === 0) return null;
  const rows: ListenerRow[] = listening.map((l) => ({ id: `${l.protocol}-${l.address}-${l.port}`, listener: l }));
  const ipv4 = boundOnIPv4(listening);
  const reach = (l: HostListener): string => {
    switch (l.reach) {
      case ListenerReach.OPEN:
        return l.decidedBy ? t`Open, as ${l.decidedBy} allows it` : t`Open: nothing here closes it`;
      case ListenerReach.SOME:
        return t`Only from ${l.sources.join(", ")}, as ${l.decidedBy} says`;
      case ListenerReach.CLOSED:
        return t`Closed by ${l.decidedBy}`;
      default:
        return l.decidedBy
          ? t`Can't tell: ${l.decidedBy} decides by names or rules it doesn't say the ports of`
          : t`Can't tell`;
    }
  };
  const columns: TableColumn<ListenerRow>[] = [
    { key: "port", header: t`Port`, width: 140, mono: true, render: ({ listener: l }) => `${l.port}/${l.protocol}` },
    {
      key: "address",
      header: t`On`,
      width: 180,
      render: ({ listener: l }) => <BoundTo address={l.address} ipv4Apart={ipv4.has(`${l.protocol} ${l.port}`)} />,
    },
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

/** The ports Docker publishes, which reach their containers before a firewall's input rules see them. */
function PublishedPanel({
  ports,
  unreadable,
  filtered,
}: {
  ports: readonly PublishedPort[];
  unreadable: string;
  filtered: boolean;
}) {
  const { t } = useLingui();
  if (unreadable) {
    return (
      <Panel title={t`Ports Docker publishes`}>
        <p className="firewall-note">{sentence(new Error(unreadable))}</p>
      </Panel>
    );
  }
  if (ports.length === 0) return null;
  const rows: PortRow[] = ports.map((port) => ({ id: `${port.hostIp}-${port.hostPort}-${port.protocol}`, port }));
  const ipv4 = boundOnIPv4(ports.map((p) => ({ address: p.hostIp, port: p.hostPort, protocol: p.protocol })));
  const columns: TableColumn<PortRow>[] = [
    { key: "port", header: t`Port`, width: 140, mono: true, render: ({ port: p }) => `${p.hostPort}/${p.protocol}` },
    {
      key: "address",
      header: t`On`,
      width: 180,
      render: ({ port: p }) => <BoundTo address={p.hostIp} ipv4Apart={ipv4.has(`${p.protocol} ${p.hostPort}`)} />,
    },
    { key: "container", header: t`Container`, mono: true, render: ({ port: p }) => p.container },
    {
      key: "to",
      header: t`Its port`,
      width: 110,
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
            cover them.
          </Trans>
        </p>
      ) : null}
      <Table columns={columns} rows={rows} rowKey="id" caption={t`Ports Docker publishes`} density="compact" />
    </Panel>
  );
}
