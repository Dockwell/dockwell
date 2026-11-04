// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate } from "@bufbuild/protobuf/wkt";
import { useQuery } from "@connectrpc/connect-query";
import { Connection, type Server, SystemService } from "@dockwell/api";
import { Panel, StatusBadge } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import type { ReactNode } from "react";

import { formatBitRate, formatBytes, formatDateTime, formatPercent } from "../../lib/format";
import { useNow, useTimeWords } from "../../lib/time";
import { CopyButton } from "./CopyButton";
import { isPrivateAddress, serverStatus } from "./server";
import "./ServerPanels.css";

/** Each disk's use, as a bar of used and free space. */
export function DisksPanel({ server }: { server: Server }) {
  const { t, i18n } = useLingui();
  const disks = server.stats?.disks.filter((disk) => disk.sizeBytes > 0n) ?? [];
  if (disks.length === 0) return null;
  const locale = i18n.locale;
  return (
    <Panel title={t`Disks`}>
      <ul className="disks">
        {disks.map((disk) => {
          const info = server.inventory?.disks.find((d) => d.mountPoint === disk.mountPoint);
          const mount = disk.mountPoint;
          const usedBytes = Number(disk.usedBytes);
          const sizeBytes = Number(disk.sizeBytes);
          const used = formatBytes(usedBytes, locale);
          const free = formatBytes(sizeBytes - usedBytes, locale);
          const size = formatBytes(sizeBytes, locale);
          const share = formatPercent((usedBytes / sizeBytes) * 100, locale);
          return (
            <li key={mount} className="disk">
              <div className="disk-head">
                <code className="disk-mount">{mount}</code>
                {info ? (
                  <span className="disk-device">{[info.device, info.filesystem].filter(Boolean).join(" · ")}</span>
                ) : null}
                <span className="disk-share">{t`${share} of ${size} used`}</span>
              </div>
              <div className="disk-bar" role="img" aria-label={t`${mount}: ${used} used, ${free} free, of ${size}`}>
                <span className="disk-used" style={{ flexGrow: usedBytes }} />
                <span className="disk-free" style={{ flexGrow: sizeBytes - usedBytes }} />
              </div>
              <dl className="disk-legend">
                <div>
                  <dt>
                    <span className="disk-swatch is-used" />
                    <Trans>Used</Trans>
                  </dt>
                  <dd>{used}</dd>
                </div>
                <div>
                  <dt>
                    <span className="disk-swatch is-free" />
                    <Trans>Free</Trans>
                  </dt>
                  <dd>{free}</dd>
                </div>
              </dl>
            </li>
          );
        })}
      </ul>
    </Panel>
  );
}

/** What the server is: its system and hardware. */
export function SystemPanel({ server }: { server: Server }) {
  const { t, i18n } = useLingui();
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

/** Where the server can be reached, and how much it is sending and receiving. */
export function NetworkPanel({ server }: { server: Server }) {
  const { t, i18n } = useLingui();
  const locale = i18n.locale;
  const addresses = server.inventory?.addresses ?? [];
  const publics = addresses.filter((ip) => !isPrivateAddress(ip));
  const privates = addresses.filter(isPrivateAddress);
  const stats = server.connection === Connection.CONNECTED ? server.stats : undefined;
  const now = stats
    ? formatBitRate(stats.networkRxBytesPerSecond + stats.networkTxBytesPerSecond, locale).join(" ")
    : undefined;
  return (
    <Panel title={t`Network`} actions={now ? <span className="panel-meta">{t`${now} now`}</span> : null}>
      <dl className="facts">
        {publics.map((ip) => (
          <Fact key={ip} term={ip.includes(":") ? t`Public IPv6` : t`Public IPv4`}>
            <span className="fact-copy">
              <code>{ip}</code>
              <CopyButton text={ip} label={t`Copy ${ip}`} />
            </span>
          </Fact>
        ))}
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
        {stats ? (
          <>
            <Fact term={t`Receiving`}>{formatBitRate(stats.networkRxBytesPerSecond, locale).join(" ")}</Fact>
            <Fact term={t`Sending`}>{formatBitRate(stats.networkTxBytesPerSecond, locale).join(" ")}</Fact>
          </>
        ) : null}
      </dl>
    </Panel>
  );
}

/** The agent on the server: its build, its connection and when it last reported. */
export function AgentPanel({ server }: { server: Server }) {
  const { t, i18n } = useLingui();
  const words = useTimeWords();
  const now = useNow();
  const controlPlane = useQuery(SystemService.method.getVersion, {}).data?.build?.version;
  const version = server.agentBuild?.version;
  const badge =
    server.connection === Connection.CONNECTED
      ? t`Connected`
      : server.connection === Connection.UNREACHABLE
        ? t`Not connected`
        : t`Waiting`;
  const connected = server.connectTime ? words.span(now - timestampDate(server.connectTime).getTime()) : undefined;
  const joined = server.createTime ? timestampDate(server.createTime) : undefined;
  return (
    <Panel title={t`Agent`} actions={<StatusBadge status={serverStatus(server)} label={badge} />}>
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
        {connected ? <Fact term={t`Connected`}>{t`For ${connected}`}</Fact> : null}
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
