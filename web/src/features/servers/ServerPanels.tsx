// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate } from "@bufbuild/protobuf/wkt";
import { useQuery } from "@connectrpc/connect-query";
import { Connection, type PublicAddress, type Server, SystemService } from "@dockwell/api";
import { Button, Panel, Sparkline, StatusBadge } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { type ReactNode, useState } from "react";

import { formatBitRate, formatBytes, formatDateTime } from "../../lib/format";
import { useTimeWords } from "../../lib/time";
import { CopyButton } from "./CopyButton";
import { trend } from "./history";
import { publicAddressesOf, useNetworkWords } from "./network";
import { SetPublicAddressSheet } from "./SetPublicAddressSheet";
import { isPrivateAddress, serverStatus } from "./server";
import { useControlPlaneNow } from "./useFleet";
import "./ServerPanels.css";

/** What the server is: its system and hardware. */
export function SystemPanel({ server }: { server: Server }) {
  const { t, i18n } = useLingui();
  const swapWords = (used: bigint, total: bigint) => {
    if (total === 0n) return t`None`;
    const size = formatBytes(total, i18n.locale);
    if (used === 0n) return t`${size} · none in use`;
    const inUse = formatBytes(used, i18n.locale);
    return t`${size} · ${inUse} in use`;
  };
  const inv = server.inventory;
  const cpus = inv?.cpuCount ?? 0;
  const unknown = <Muted>{t`Unknown`}</Muted>;
  return (
    <Panel
      title={t`System`}
      actions={inv?.architecture ? <code className="panel-meta">{inv.architecture}</code> : null}
    >
      {inv ? (
        <dl className="facts">
          <Fact term={t`Operating system`}>{inv.osName || unknown}</Fact>
          <Fact term={t`Kernel`}>{inv.kernel ? <code>{inv.kernel}</code> : unknown}</Fact>
          <Fact term={t`Processor`}>
            {[inv.cpuModel, cpus > 0 ? t`${cpus} vCPU` : ""].filter(Boolean).join(" · ") || unknown}
          </Fact>
          <Fact term={t`Memory`}>{inv.memoryBytes > 0n ? formatBytes(inv.memoryBytes, i18n.locale) : unknown}</Fact>
          {server.stats ? (
            <Fact term={t`Swap`}>{swapWords(server.stats.swapUsedBytes, server.stats.swapTotalBytes)}</Fact>
          ) : null}
          <Fact term={t`Docker`}>
            {inv.dockerVersion ? <code>{inv.dockerVersion}</code> : <Muted>{t`Not installed or not running`}</Muted>}
          </Fact>
          <Fact term={t`Hostname`}>{inv.hostname ? <code>{inv.hostname}</code> : unknown}</Fact>
        </dl>
      ) : (
        <p className="panel-empty">{t`The agent hasn't said what this server is yet.`}</p>
      )}
    </Panel>
  );
}

/**
 * How the server meets the internet, with three facts kept apart: what its own interfaces
 * have, what the internet knows it by and how that's known, and whether anything has
 * shown the internet can reach it. Then how much it is sending and receiving.
 */
export function NetworkPanel({ server }: { server: Server }) {
  const { t, i18n } = useLingui();
  const locale = i18n.locale;
  const words = useNetworkWords();
  const clock = useControlPlaneNow();
  // Which address the sheet is changing, or null for a first one.
  const [setting, setSetting] = useState<PublicAddress | null>();
  const network = server.network;
  const addresses = server.inventory?.addresses ?? [];
  const known = publicAddressesOf(network);
  const publics = addresses.filter((ip) => !isPrivateAddress(ip) && !known.some((a) => a.address === ip));
  const privates = addresses.filter(isPrivateAddress);
  const shape = words.shape(network);
  const reach = words.reachable(network, clock);
  const stats = server.connection === Connection.CONNECTED ? server.stats : undefined;
  const now = stats
    ? formatBitRate(stats.networkRxBytesPerSecond + stats.networkTxBytesPerSecond, locale).join(" ")
    : undefined;
  return (
    <Panel title={t`Network`} actions={now ? <span className="panel-meta">{t`${now} now`}</span> : null}>
      <dl className="facts">
        {shape ? <Fact term={t`Connection`}>{shape}</Fact> : null}
        {known.map((address) => {
          const ip = address.address;
          return (
            <Fact key={ip} term={ip.includes(":") ? t`Public IPv6` : t`Public IPv4`}>
              <span className="fact-copy">
                <code>{ip}</code>
                <CopyButton text={ip} label={t`Copy ${ip}`} />
                <Button variant="ghost" size="sm" onClick={() => setSetting(address)}>
                  <Trans>Change</Trans>
                </Button>
              </span>
              {network ? <span className="fact-note">{words.how(address, network, clock)}</span> : null}
            </Fact>
          );
        })}
        {known.length === 0 ? (
          <Fact term={t`Public address`}>
            <span className="fact-copy">
              <Muted>{t`Not known`}</Muted>
              <Button variant="ghost" size="sm" onClick={() => setSetting(null)}>
                <Trans>Set</Trans>
              </Button>
            </span>
          </Fact>
        ) : null}
        {publics.map((ip) => (
          <Fact key={ip} term={ip.includes(":") ? t`Public IPv6` : t`Public IPv4`}>
            <span className="fact-copy">
              <code>{ip}</code>
              <CopyButton text={ip} label={t`Copy ${ip}`} />
            </span>
          </Fact>
        ))}
        <Fact term={t`Reachable`}>
          <StatusBadge status={reach.status} label={reach.word} labelIsState />
          <span className="fact-note">{reach.proof}</span>
        </Fact>
        {privates.length > 0 ? (
          <Fact term={t`Private addresses`}>
            <span className="fact-list">
              {privates.map((ip) => (
                <code key={ip}>{ip}</code>
              ))}
            </span>
          </Fact>
        ) : null}
        {addresses.length === 0 ? (
          <Fact term={t`Addresses`}>
            <Muted>{t`None reported`}</Muted>
          </Fact>
        ) : null}
        {stats ? <Traffic server={server} /> : null}
      </dl>
      {setting !== undefined ? (
        <SetPublicAddressSheet server={server} current={setting ?? undefined} onClose={() => setSetting(undefined)} />
      ) : null}
    </Panel>
  );
}

/**
 * What the server sends and receives now, its shape over the last few minutes in
 * cobalt, as live traffic is, and how much since it booted.
 */
function Traffic({ server }: { server: Server }) {
  const { t, i18n } = useLingui();
  const locale = i18n.locale;
  const stats = server.stats;
  if (!stats) return null;
  const rate = (n: bigint) => formatBitRate(n, locale).join(" ");
  const rx = rate(stats.networkRxBytesPerSecond);
  const tx = rate(stats.networkTxBytesPerSecond);
  const { samples } = trend(server.history.length > 0 ? server.history : [stats]);
  const data = samples.map((s) => Number(s.networkRxBytesPerSecond + s.networkTxBytesPerSecond));
  const booted = stats.networkRxBytes > 0n || stats.networkTxBytes > 0n;
  const inTotal = formatBytes(stats.networkRxBytes, locale);
  const outTotal = formatBytes(stats.networkTxBytes, locale);
  return (
    <>
      <Fact term={t`Traffic`}>
        <span className="fact-traffic">{t`${rx} in · ${tx} out`}</span>
        {data.length > 1 ? (
          <Sparkline
            className="fact-spark"
            data={data}
            tone="cobalt"
            min={0}
            width="100%"
            height={28}
            label={t`Traffic over the last few minutes, now ${rx} in and ${tx} out`}
          />
        ) : null}
      </Fact>
      {booted ? <Fact term={t`Since boot`}>{t`${inTotal} in · ${outTotal} out`}</Fact> : null}
    </>
  );
}

/** The agent on the server: its build, its connection and when it last reported. */
export function AgentPanel({ server, unknown = false }: { server: Server; unknown?: boolean }) {
  const { t, i18n } = useLingui();
  const words = useTimeWords();
  // The control plane's clock, which its timestamps are on, so a browser whose clock is off doesn't skew them.
  const now = useControlPlaneNow();
  const controlPlane = useQuery(SystemService.method.getVersion, {}).data?.build?.version;
  const version = server.agentBuild?.version;
  const blind = server.connection === Connection.CONNECTED ? server.dockerUnreadableSince : undefined;
  const badge =
    server.connection === Connection.CONNECTED
      ? blind
        ? t`Can't read Docker`
        : t`Connected`
      : server.connection === Connection.UNREACHABLE
        ? t`Not connected`
        : t`Waiting`;
  const connected = server.connectTime ? words.span(now - timestampDate(server.connectTime).getTime()) : undefined;
  const joined = server.createTime ? timestampDate(server.createTime) : undefined;
  return (
    <Panel
      title={t`Agent`}
      actions={
        unknown ? (
          <StatusBadge status="unknown" label={t`Unknown`} labelIsState />
        ) : (
          <StatusBadge status={serverStatus(server)} label={badge} />
        )
      }
    >
      <dl className="facts">
        {version ? (
          <Fact term={t`Version`}>
            <code>{version}</code>
            {controlPlane ? (
              <Muted>
                {" · "}
                {controlPlane === version ? t`same as the control plane` : t`the control plane runs ${controlPlane}`}
              </Muted>
            ) : null}
          </Fact>
        ) : null}
        <Fact term={t`Control plane`}>{server.controlServer ? t`Runs on this server` : t`Runs on another server`}</Fact>
        {server.connection === Connection.CONNECTED && server.stats && server.stats.agentMemoryBytes > 0n ? (
          <Fact term={t`Memory`}>
            {server.controlServer
              ? t`${formatBytes(server.stats.agentMemoryBytes, i18n.locale)}, with the control plane`
              : formatBytes(server.stats.agentMemoryBytes, i18n.locale)}
          </Fact>
        ) : null}
        {connected ? <Fact term={t`Connected`}>{t`For ${connected}`}</Fact> : null}
        {blind ? <DockerFact since={timestampDate(blind)} now={now} /> : null}
        <Fact term={t`Last heartbeat`}>
          {server.lastSeenTime ? words.ago(timestampDate(server.lastSeenTime), now) : <Muted>{t`Never`}</Muted>}
        </Fact>
        {joined ? (
          <Fact term={t`Joined`}>
            <time dateTime={joined.toISOString()}>{formatDateTime(joined, i18n.locale)}</time>
          </Fact>
        ) : null}
      </dl>
    </Panel>
  );
}

/** An agent that can't read Docker: since when, what that means, and how to check it. */
function DockerFact({ since, now }: { since: Date; now: number }) {
  const { t } = useLingui();
  const ago = useTimeWords().ago(since, now);
  return (
    <Fact term={t`Docker`}>
      <Trans>
        Not answering, since {ago}, so nobody can say what runs here. Check that it's running, with{" "}
        <code>systemctl status docker</code>.
      </Trans>
    </Fact>
  );
}

function Fact({ term, children }: { term: ReactNode; children: ReactNode }) {
  return (
    <>
      <dt>{term}</dt>
      <dd>{children}</dd>
    </>
  );
}

function Muted({ children }: { children: ReactNode }) {
  return <span className="fact-muted">{children}</span>;
}
