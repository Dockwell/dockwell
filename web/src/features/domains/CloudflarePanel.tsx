// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate } from "@bufbuild/protobuf/wkt";
import { createClient } from "@connectrpc/connect";
import { createConnectQueryKey, useTransport } from "@connectrpc/connect-query";
import {
  Capability,
  CapabilityState,
  type CheckDnsProviderResponse,
  type DnsProviderInfo,
  DomainService,
} from "@dockwell/api";
import { Button, Panel, StatusBadge } from "@dockwell/ui";
import { useLingui } from "@lingui/react/macro";
import { useQueryClient } from "@tanstack/react-query";
import { type ReactNode, useMemo, useState } from "react";

import { sentence } from "../../lib/errors";
import { useNow, useTimeWords } from "../../lib/time";
import { StateLine } from "../apps/DomainStep";
import { useMay } from "../auth/may";
import { DnsProviderSheet } from "../dns/DnsProviderSheet";

/**
 * Each Cloudflare connection beside the list, as Domains-List draws it (#820): its
 * zones, how many records Dockwell keeps there and when it last read them, with Sync
 * now and Replace token. Below an admin, its name and zones alone, as the list is
 * trimmed to them.
 */
export function CloudflarePanels({ providers }: { providers: readonly DnsProviderInfo[] }) {
  const cloudflare = providers.filter((p) => p.provider === "" || p.provider === "cloudflare");
  if (cloudflare.length === 0) return null;
  return (
    <>
      {cloudflare.map((p) => (
        <CloudflarePanel key={p.id} provider={p} all={providers} several={cloudflare.length > 1} />
      ))}
    </>
  );
}

function CloudflarePanel({
  provider: listed,
  all,
  several,
}: {
  provider: DnsProviderInfo;
  all: readonly DnsProviderInfo[];
  several: boolean;
}) {
  const { t } = useLingui();
  const time = useTimeWords();
  const now = useNow(30_000);
  const may = useMay();
  const maySync = may(DomainService.method.syncDnsProvider).allowed;
  const mayCheck = may(DomainService.method.checkDnsProvider).allowed;
  const transport = useTransport();
  const client = useMemo(() => createClient(DomainService, transport), [transport]);
  const queryClient = useQueryClient();
  const [synced, setSynced] = useState<DnsProviderInfo>();
  const [rights, setRights] = useState<CheckDnsProviderResponse>();
  const [syncing, setSyncing] = useState(false);
  const [failure, setFailure] = useState<string>();
  const [replacing, setReplacing] = useState(false);
  // What the last Sync said, until the list says of a later read.
  const readMs = (info: DnsProviderInfo | undefined) => (info?.readAt ? timestampDate(info.readAt).getTime() : -1);
  const p = synced && readMs(synced) > readMs(listed) ? synced : listed;
  const admin = maySync;

  const sync = async () => {
    setSyncing(true);
    setFailure(undefined);
    try {
      const [read, checked] = await Promise.all([
        client.syncDnsProvider({ id: p.id }),
        // The token's rights, read once a person asks, as each check is on Activity.
        mayCheck ? client.checkDnsProvider({ name: p.name }).catch(() => undefined) : Promise.resolve(undefined),
      ]);
      setSynced(read.provider);
      if (checked) setRights(checked);
      // What the read marked shows on the list: a domain gone, or back.
      void queryClient.invalidateQueries({
        queryKey: createConnectQueryKey({ schema: DomainService.method.listDnsProviders, cardinality: "finite" }),
      });
      void queryClient.invalidateQueries({
        queryKey: createConnectQueryKey({ schema: DomainService.method.listDomains, cardinality: "finite" }),
      });
    } catch (err) {
      setFailure(sentence(err));
    } finally {
      setSyncing(false);
    }
  };

  const zones = p.zones.join(", ");
  const name = p.name;
  const facts: [string, ReactNode][] = [[p.zones.length > 1 ? t`Zones` : t`Zone`, <code key="zones">{zones}</code>]];
  if (admin) {
    const records = p.managedRecords;
    facts.push([t`Records`, records === 0 ? t`None yet` : t`${records} managed by Dockwell`]);
    facts.push([t`API token`, <TokenWords key="token" provider={p} rights={rights} />]);
    const readAt = p.readAt ? timestampDate(p.readAt) : undefined;
    facts.push([t`Last sync`, readAt ? time.ago(readAt, now) : t`Not read yet`]);
  }
  return (
    <Panel
      className="domains-cloudflare"
      title={several ? t`Cloudflare · ${name}` : t`Cloudflare`}
      actions={<StatusBadge status="healthy" label={t`Connected`} />}
    >
      <dl className="domains-facts">
        {facts.map(([term, value]) => (
          <div key={term}>
            <dt>{term}</dt>
            <dd>{value}</dd>
          </div>
        ))}
      </dl>
      {failure ? <StateLine status="degraded">{failure}</StateLine> : null}
      {admin ? (
        <div className="domains-cloudflare-actions">
          <Button size="sm" icon="refresh" loading={syncing} onClick={() => void sync()}>
            {t`Sync now`}
          </Button>
          <Button size="sm" variant="ghost" onClick={() => setReplacing(true)}>
            {t`Replace token`}
          </Button>
        </div>
      ) : null}
      {replacing ? <DnsProviderSheet existing={all} editing={p} onClose={() => setReplacing(false)} /> : null}
    </Panel>
  );
}

/**
 * What the token may do: as checked, once Sync now has; until then, what's known
 * without asking Cloudflare, as each check of a saved token is on Activity.
 */
function TokenWords({ provider, rights }: { provider: DnsProviderInfo; rights: CheckDnsProviderResponse | undefined }) {
  const { t } = useLingui();
  const zones = provider.zones.length;
  if (!rights) {
    const dns = zones === 1 ? t`Edits DNS in its zone` : t`Edits DNS in ${zones} zones`;
    return <>{provider.tunnelApiTokenSet ? t`${dns}, a second token for tunnels` : dns}</>;
  }
  const has = (c: Capability) => rights.capabilities.find((x) => x.capability === c)?.state === CapabilityState.HAS;
  const dns = has(Capability.DNS) ? t`Edits DNS` : t`Can't edit DNS`;
  return <>{has(Capability.TUNNELS) ? t`${dns}, publishes through tunnels` : t`${dns}, no tunnels`}</>;
}
