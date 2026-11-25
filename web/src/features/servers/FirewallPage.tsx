// SPDX-License-Identifier: AGPL-3.0-only

import { useQuery } from "@connectrpc/connect-query";
import {
  ChangeService,
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
import {
  Button,
  Callout,
  Loading,
  Panel,
  PHONE_QUERY,
  SkeletonCard,
  SkeletonRows,
  Switch,
  type TableColumn,
  useMediaQuery,
} from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { type ReactNode, useState } from "react";

import { reasonOf, sentence } from "../../lib/errors";
import { useMay } from "../auth/may";
import { useChangeSheet } from "../changes/ChangeSheet";
import { useReadAgainOnceOver } from "../changes/follow";
import { type ListRouteProps, ListSectionTable, ListToolbar, useListPlace } from "../shell/ListTools";
import { ReadFailed } from "../shell/ReadFailed";
import { Fail2banPanel, SshPanel } from "./FirewallAccess";
import { DockwellRules, policyEdit, usePortUser } from "./FirewallRules";
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
export function FirewallPage({ id, search, onSearch }: { id: string } & ListRouteProps) {
  return (
    <ServerFrame id={id}>{(server) => <Firewall server={server} search={search} onSearch={onSearch} />}</ServerFrame>
  );
}

/** ufw and firewalld, which the tab reads rule by rule. */
function weighed(tool: FirewallTool): boolean {
  return tool.kind === FirewallToolKind.UFW || tool.kind === FirewallToolKind.FIREWALLD;
}

/** The firewall's read, every READ_EVERY_MS while the tab is open. */
function useFirewallRead(server: Server, connected: boolean) {
  return useQuery(
    FirewallService.method.getServerFirewall,
    { serverId: server.id },
    { enabled: connected, refetchInterval: READ_EVERY_MS, retry: false },
  );
}

function Firewall({ server, search, onSearch }: { server: Server } & ListRouteProps) {
  const { t } = useLingui();
  const connected = server.connection === Connection.CONNECTED;
  const read = useFirewallRead(server, connected);
  if (!connected) {
    return (
      <Callout tone="info">{t`${server.name} isn't reporting, so what filters its traffic can't be read now.`}</Callout>
    );
  }
  const fw = read.data?.firewall;
  return (
    <div className="firewall">
      <div className="firewall-main">
        <FirewallRead
          server={server}
          read={read}
          search={search}
          onSearch={onSearch}
          ssh={<SshPanel key="ssh" server={server} connected={connected} listening={fw?.listening} />}
        />
      </div>
      <div className="firewall-side">
        <Fail2banPanel server={server} connected={connected} />
      </div>
    </div>
  );
}

/**
 * What the firewall read says: Dockwell's rules, the firewalls already there,
 * then SSH, which the mock puts after the rules, then what listens and what
 * Docker publishes. SSH reads on its own, so it shows whatever this read does,
 * and keeps its place by its key as this read lands.
 */
function FirewallRead({
  server,
  read,
  ssh,
  search,
  onSearch,
}: ListRouteProps & {
  server: Server;
  read: ReturnType<typeof useFirewallRead>;
  ssh: ReactNode;
}) {
  const { t } = useLingui();
  const place = useListPlace(search, onSearch, ["action", "port", "from", "note", "rule"]);
  if (read.error) {
    if (reasonOf(read.error) === "agent_outdated") {
      return (
        <>
          <Callout tone="warning">
            <Trans>
              The agent on {server.name} is older than the control plane, so it can't read the firewall. Run the install
              script on {server.name} again to update it.
            </Trans>
          </Callout>
          {ssh}
        </>
      );
    }
    // A role that doesn't cover the firewall is told so as everywhere else, not as something that broke;
    // it doesn't cover SSH and fail2ban either, so they aren't asked. Otherwise each card reads on its own.
    return (
      <>
        <ReadFailed error={read.error} title={t`Dockwell can't read ${server.name}'s firewall`} />
        {reasonOf(read.error) === "needs_role" ? null : ssh}
      </>
    );
  }
  const fw = read.data?.firewall;
  if (!fw) {
    return (
      <>
        <Loading
          label={t`Reading what filters ${server.name}'s traffic`}
          slow={t`${server.name} is slow to say what filters its traffic`}
          className="firewall-loading"
        >
          <SkeletonCard title={t`Inbound traffic`} flush>
            <SkeletonRows rows={4} lines={1} end="pill" dense />
          </SkeletonCard>
          <SkeletonCard flush>
            <SkeletonRows rows={3} lines={1} end="value" dense />
          </SkeletonCard>
        </Loading>
        {ssh}
      </>
    );
  }
  const there = fw.tools.filter((tool) => tool.active || tool.unreadable);
  const primary = there.filter(weighed);
  const others = there.filter((tool) => !weighed(tool));
  const filtered = primary.some((tool) => tool.active && !tool.unreadable);
  const dockwell = fw.dockwell;
  // Dockwell's own table filters too, once it holds a rule or the policy.
  const dockwellFilters =
    (dockwell?.rules.length ?? 0) > 0 ||
    (dockwell?.policy?.denyEverythingElse ?? false) ||
    (dockwell?.policy?.guardDockerPorts ?? false) ||
    (dockwell?.waitingChange ?? "") !== "";
  return (
    <>
      <ListToolbar place={place} label={t`Filter firewall rules and ports`} />
      <DockwellRules
        server={server}
        fw={fw}
        yourAddress={read.data?.yourAddress ?? ""}
        place={place}
        onChanged={() => void read.refetch()}
      />
      {there.length === 0 ? (
        <Panel title={t`Inbound traffic`}>
          <p className="firewall-note">
            {dockwellFilters ? (
              <Trans>
                Only Dockwell's rules filter what comes in to {server.name}: it has no ufw, firewalld or other nftables
                or iptables rules of its own.
              </Trans>
            ) : (
              <Trans>
                Nothing filters what comes in to {server.name}: it has no ufw, firewalld, nftables or iptables rules
                that could.
              </Trans>
            )}
          </p>
        </Panel>
      ) : (
        primary.map((tool) => (
          <ToolPanel key={`${tool.kind}-${tool.name}`} server={server} fw={fw} tool={tool} query={place.query} />
        ))
      )}
      {ssh}
      <ListeningPanel server={server} listening={fw.listening} query={place.query} />
      <PublishedPanel
        server={server}
        onChanged={() => void read.refetch()}
        fw={fw}
        filtered={filtered}
        dockwellDenies={dockwell?.policy?.denyEverythingElse ?? false}
        query={place.query}
      />
      {others.length > 0 ? <OtherRulesPanel tools={others} query={place.query} /> : null}
    </>
  );
}

interface RuleRow {
  id: string;
  rule: FirewallSeenRule;
}

/** A rule's "To" as ufw says it: an address, a port, its app profile, and the interface, with every port in words. */
function useRuleTo(): (rule: FirewallSeenRule) => string {
  const { t } = useLingui();
  return (rule) => {
    let port = rule.port;
    if (port === "Anywhere") {
      port = rule.destination ? "" : t`Any port`;
    } else if (port.startsWith("Anywhere/")) {
      const proto = port.slice("Anywhere/".length);
      port = t`Any ${proto} port`;
    }
    const iface = rule.interface;
    return [rule.destination, port, rule.app ? `(${rule.app})` : "", iface ? t`on ${iface}` : ""]
      .filter(Boolean)
      .join(" ");
  };
}

/**
 * The ports a rule names, as ufw and firewalld write them: "22/tcp", "80,443/tcp",
 * "6000:6007/udp", or a port with no protocol. None for a rule of any port, or one
 * that names an app profile or a service alone; a range counts what's in it.
 */
export function portsOf(rule: string): { port: number; protocol?: string }[] {
  const m = /^([\d,:]+)(?:\/(tcp|udp))?$/.exec(rule);
  if (!m?.[1]) return [];
  const protocol = m[2];
  return m[1].split(",").flatMap((part) => {
    const [from, to] = part.split(":").map(Number);
    if (from === undefined || !Number.isInteger(from)) return [];
    if (to === undefined) return [{ port: from, protocol }];
    // A long range lists nothing rather than every port in it.
    if (!Number.isInteger(to) || to < from || to - from > 1024) return [];
    return Array.from({ length: to - from + 1 }, (_, i) => ({ port: from + i, protocol }));
  });
}

/** One firewall Dockwell didn't make: what it is, its default, and its rules, read-only, with what uses each. */
function ToolPanel({
  server,
  fw,
  tool,
  query,
}: {
  server: Server;
  fw: ServerFirewall;
  tool: FirewallTool;
  query: string;
}) {
  const { t } = useLingui();
  const words = useVerdictWords();
  const ruleTo = useRuleTo();
  const user = usePortUser(server, fw);
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
      // Its note names where it lets in from, as the mock's "Office" under an address.
      render: ({ rule }) => {
        const from = rule.source ? <code>{rule.source}</code> : rule.ipv6 ? t`Anywhere, IPv6` : t`Anywhere`;
        return rule.comment ? (
          <span className="firewall-stack">
            {from}
            <span className="firewall-sub">{rule.comment}</span>
          </span>
        ) : (
          from
        );
      },
    },
    { key: "used", header: t`Used by`, muted: true, render: ({ rule }) => user(portsOf(rule.port)) },
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
            <ListSectionTable
              query={query}
              values={{
                verdict: ({ rule }) => words[rule.verdict],
                port: ({ rule }) => ruleTo(rule),
                source: ({ rule }) => rule.source,
                used: ({ rule }) => user(portsOf(rule.port)),
                note: ({ rule }) => rule.comment,
              }}
              columns={columns}
              rows={rows}
              rowKey="id"
              caption={t`Rules of ${title}`}
              density="compact"
            />
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
 * "::" takes IPv4 too, as Linux has it by default, unless the port's IPv4 is
 * taken apart: by a socket on "0.0.0.0", or as a port Docker publishes.
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

/**
 * What listens on the server itself, and how far each reaches past the firewalls
 * that cover it. What reaches out comes first; what a firewall closes is a step
 * away, as it's what needs no look.
 */
function ListeningPanel({
  server,
  listening,
  query,
}: {
  server: Server;
  listening: readonly HostListener[];
  query: string;
}) {
  const { t } = useLingui();
  const phone = useMediaQuery(PHONE_QUERY);
  const reach = useReachWords();
  const [allShown, setAllShown] = useState(false);
  if (listening.length === 0) return null;
  const closed = listening.filter((l) => l.reach === ListenerReach.CLOSED);
  const shown = allShown
    ? [...listening.filter((l) => l.reach !== ListenerReach.CLOSED), ...closed]
    : listening.filter((l) => l.reach !== ListenerReach.CLOSED);
  const rows: ListenerRow[] = shown.map((l) => ({ id: `${l.protocol}-${l.address}-${l.port}`, listener: l }));
  // The agent says which sockets on "::" take IPv6 alone: it sees what the tab
  // doesn't, as the socket Docker forwards IPv4 past the rules with.
  const where = (l: HostListener) => <BoundTo address={l.address} ipv4Apart={l.ipv6Only} />;
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
      {rows.length > 0 ? (
        <ListSectionTable
          query={query}
          values={{
            port: ({ listener }) => listener.port,
            address: ({ listener }) => listener.address,
            reach: ({ listener }) => reach(listener),
            protocol: ({ listener }) => listener.protocol,
          }}
          columns={columns}
          rows={rows}
          rowKey="id"
          caption={t`What listens on ${server.name}`}
          density="compact"
        />
      ) : (
        <p className="firewall-note firewall-pad">
          {plural(closed.length, {
            one: "Its one port is closed by a firewall.",
            other: "All # are closed by a firewall.",
          })}
        </p>
      )}
      {closed.length > 0 && !allShown ? (
        <div className="firewall-foot">
          <Button size="sm" variant="ghost" icon="chevron-down" onClick={() => setAllShown(true)}>
            {plural(closed.length, { one: "Show # closed port", other: "Show # closed ports" })}
          </Button>
        </div>
      ) : null}
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
function PublishedPanel({
  server,
  onChanged,
  fw,
  filtered,
  dockwellDenies,
  query,
}: {
  server: Server;
  onChanged: () => void;
  fw: ServerFirewall;
  filtered: boolean;
  /** Dockwell denies everything else, which covers what Docker publishes too. */
  dockwellDenies: boolean;
  query: string;
}) {
  const { t } = useLingui();
  const phone = useMediaQuery(PHONE_QUERY);
  const changes = useChangeSheet();
  const mayPlan = useMay()(ChangeService.method.proposeChange, { install: true });
  const readAgain = useReadAgainOnceOver(onChanged);
  const dockwell = fw.dockwell;
  const waiting = !!dockwell?.waitingChange;
  const guards = !!dockwell?.policy?.guardDockerPorts;
  const waitingGuard = waiting && !!dockwell?.waitingPolicy?.guardDockerPorts !== guards;
  const guardOn = waitingGuard ? !guards : guards;
  const guardsUdp = !!dockwell?.policy?.guardDockerUdp;
  const udpOn = waiting ? !!dockwell?.waitingPolicy?.guardDockerUdp : guardsUdp;
  const waitingUdp = waiting && udpOn !== guardsUdp;
  const name = server.name;
  const setGuard = (on: boolean) =>
    changes.open({
      kind: "propose",
      summary: on ? t`Guard Docker ports on ${name}` : t`Stop guarding Docker ports on ${name}`,
      edits: [policyEdit(server, dockwellDenies, on, on && guardsUdp)],
      onClose: readAgain,
    });
  const setGuardUdp = (on: boolean) =>
    changes.open({
      kind: "propose",
      summary: on ? t`Guard Docker UDP ports on ${name}` : t`Leave Docker UDP ports open on ${name}`,
      edits: [policyEdit(server, dockwellDenies, true, on)],
      onClose: readAgain,
    });
  const ports = fw.published;
  if (fw.publishedUnreadable) {
    return (
      <Panel title={t`Ports Docker publishes`}>
        <p className="firewall-note">{sentence(new Error(fw.publishedUnreadable))}</p>
      </Panel>
    );
  }
  if (ports.length === 0 && !guards && !waitingGuard) return null;
  const rows: PortRow[] = ports.map((port) => ({ id: `${port.hostIp}-${port.hostPort}-${port.protocol}`, port }));
  // What the input rules don't see: a port on more than loopback that Docker's proxy doesn't take.
  const past = ports.filter((p) => !p.proxied && !p.hostIp.startsWith("127.") && p.hostIp !== "::1");
  const publicPort = past.some((p) => p.hostIp === "0.0.0.0" || p.hostIp === "::" || p.hostIp === "");
  const ipv4 = boundOnIPv4(ports.map((p) => ({ address: p.hostIp, port: p.hostPort, protocol: p.protocol })));
  // Each on a line of its own: the address, then what Docker's proxy means for it.
  const where = (p: PublishedPort) => (
    <>
      <span className="firewall-line">
        <BoundTo address={p.hostIp} ipv4Apart={ipv4.has(`${p.protocol} ${p.hostPort}`)} />
      </span>
      {p.proxied ? (
        <span className="firewall-line firewall-sub">{t`Through Docker's proxy, so the input rules cover it.`}</span>
      ) : null}
    </>
  );
  const columns: TableColumn<PortRow>[] = phone
    ? [
        {
          key: "port",
          header: t`Port`,
          render: ({ port: p }) => (
            <span className="firewall-stack firewall-sub-lines">
              <code>{`${p.hostPort}/${p.protocol}`}</code>
              {where(p)}
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
      {dockwell?.canGuardDockerPorts && (guards || waitingGuard || (publicPort && !dockwellDenies)) ? (
        <div className="firewall-policy">
          <Switch
            checked={guardOn}
            onChange={setGuard}
            disabled={waiting || !mayPlan.allowed}
            label={waitingGuard ? t`Guard Docker ports, waiting for Keep` : t`Guard Docker ports`}
            description={t`Forwarded connections need an allow rule. UDP keeps its existing access unless you include it.`}
          />
          {guardOn && !dockwellDenies ? (
            <Switch
              checked={udpOn}
              onChange={setGuardUdp}
              disabled={waiting || !mayPlan.allowed}
              label={waitingUdp ? t`Guard Docker UDP ports, waiting for Keep` : t`Guard Docker UDP ports`}
              description={t`Published UDP ports need an allow rule, including ports used by VPN containers.`}
            />
          ) : null}
        </div>
      ) : null}
      {/* Said only where it matters: a port that answers beyond loopback, past Docker's proxy (#800). */}
      {filtered && past.length > 0 ? (
        <p className="firewall-note">
          {plural(past.length, {
            one: "ufw and firewalld don't cover # port here, as Docker forwards it first.",
            other: "ufw and firewalld don't cover # ports here, as Docker forwards them first.",
          })}
        </p>
      ) : null}
      {dockwellDenies ? (
        <p className="firewall-note">
          <Trans>Deny everything else covers these too: only a port Dockwell allows reaches its container.</Trans>
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
      <ListSectionTable
        query={query}
        values={{
          port: ({ port }) => port.hostPort,
          address: ({ port }) => port.hostIp,
          container: ({ port }) => port.container,
          to: ({ port }) => port.containerPort,
          protocol: ({ port }) => port.protocol,
        }}
        columns={columns}
        rows={rows}
        rowKey="id"
        caption={t`Ports Docker publishes`}
        density="compact"
      />
    </Panel>
  );
}

interface OtherRow {
  id: string;
  tool: FirewallTool;
}

/** The kernel's other rules, which Dockwell doesn't weigh one by one, in one panel below the rest. */
function OtherRulesPanel({ tools, query }: { tools: readonly FirewallTool[]; query: string }) {
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
    {
      key: "name",
      header: t({ message: "Rules", context: "firewall rule sets" }),
      render: ({ tool }) => <code>{name(tool)}</code>,
    },
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
      <ListSectionTable
        query={query}
        values={{ name: ({ tool }) => name(tool), what: ({ tool }) => what(tool) }}
        columns={columns}
        rows={rows}
        rowKey="id"
        caption={t`The kernel's other rules`}
        density="compact"
      />
    </Panel>
  );
}
