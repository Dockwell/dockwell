// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { createConnectQueryKey, skipToken, useQuery, useTransport } from "@connectrpc/connect-query";
import { PanelDomainFrom, type PanelServing, Role, ServerService, SettingsService, StepUpAction } from "@dockwell/api";
import { Button, Callout, DnsRecord, TextField } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { useQueryClient } from "@tanstack/react-query";
import { useEffect, useState } from "react";

import { sentence } from "../../lib/errors";
import { useMay } from "../auth/may";
import { withStepUpIfAsked } from "../auth/stepUp";
import { useSignedIn } from "../auth/useAuthState";
import { SetPublicAddressSheet } from "../servers/SetPublicAddressSheet";
import { useFleet } from "../servers/useFleet";

/** How long typing rests before the domain is checked. */
const CHECK_AFTER_MS = 400;

/** Leaves for the panel on its domain, which a test watches rather than follows. */
export const leave = {
  to(url: string) {
    window.location.assign(url);
  },
};

/** Where the install docs say how to change what dockwell run starts with. */
const CHANGE_SETTINGS = "https://github.com/Dockwell/dockwell/blob/main/docs/install.md#change-settings";

/**
 * The panel's domain, set here (ADR 0063): a field checked as it's typed, a save
 * the owner confirms with a passkey, which restarts Dockwell to serve it, then
 * the way onto it. A domain dockwell run was given stays where it was given.
 */
export function PanelDomain({ panel }: { panel: PanelServing | undefined }) {
  const owner = useSignedIn()?.user?.role === Role.OWNER;
  const [restarting, setRestarting] = useState<{ to: string; from: string } | undefined>();
  // While Dockwell restarts, the settings are read again until the new one answers.
  const settings = useQuery(SettingsService.method.getSettings, {}, { refetchInterval: restarting ? 1000 : false });
  const now = settings.data?.settings?.panel ?? panel;
  useEffect(() => {
    if (!restarting || !now) return;
    // The control plane that answers before it restarts already says what was saved:
    // only one that started since is the restart.
    const arrived = restarting.from
      ? startedAt(now) !== "" && startedAt(now) !== restarting.from
      : restarting.to === ""
        ? !now.setDomain && !now.domain
        : now.domain === restarting.to || now.problem !== "";
    if (arrived) setRestarting(undefined);
  }, [restarting, now]);
  const onRestart = (r: { to: string }) => setRestarting({ to: r.to, from: startedAt(now) });
  if (!now) return null;
  if (restarting) {
    return (
      <p className="settings-sub" role="status">
        {restarting.to ? (
          <Trans>Dockwell restarts to serve {restarting.to}. Apps and websites keep running…</Trans>
        ) : (
          <Trans>Dockwell restarts to answer on loopback alone. Apps and websites keep running…</Trans>
        )}
      </p>
    );
  }
  if (now.domainFrom === PanelDomainFrom.RUN) {
    return (
      <p className="settings-sub">
        <Trans>
          dockwell run was started with this domain, as <code>DOCKWELL_DOMAIN</code> or <code>--domain</code>, which
          wins over Settings. Change it there.
        </Trans>{" "}
        <a href={CHANGE_SETTINGS} target="_blank" rel="noreferrer" className="settings-link">
          <Trans>How to change settings</Trans>
        </a>
      </p>
    );
  }
  if (now.setDomain) {
    return <Served panel={now} owner={owner} onRestart={onRestart} />;
  }
  return <DomainField owner={owner} onRestart={onRestart} />;
}

/** When the control plane that answered started, which a restart changes; "" when it doesn't say. */
function startedAt(panel: PanelServing | undefined): string {
  const t = panel?.startTime;
  return t ? `${t.seconds}.${t.nanos}` : "";
}

/** Saving or clearing a domain, with the passkey it takes, then the restart it starts. */
function useSetDomain(onRestart: (r: { to: string }) => void) {
  const transport = useTransport();
  const queryClient = useQueryClient();
  const [busy, setBusy] = useState(false);
  const [failure, setFailure] = useState<unknown>();
  const save = async (domain: string, confirmOn: string) => {
    setBusy(true);
    setFailure(undefined);
    try {
      const client = createClient(SettingsService, transport);
      const res = await withStepUpIfAsked(transport, StepUpAction.SET_PANEL_DOMAIN, confirmOn, (proof) =>
        client.setPanelDomain({ domain, proof }),
      );
      if (res.restarting) onRestart({ to: domain });
      void queryClient.invalidateQueries({
        queryKey: createConnectQueryKey({ schema: SettingsService.method.getSettings, cardinality: undefined }),
      });
    } catch (error) {
      setFailure(error);
    } finally {
      setBusy(false);
    }
  };
  return { save, busy, failure };
}

/** A server's words, which start lower case to sit inside a sentence, standing on their own. */
const capital = (words: string) => `${words.charAt(0).toUpperCase()}${words.slice(1)}`;

/** A domain to give the panel, checked as it's typed. */
function DomainField({ owner, onRestart }: { owner: boolean; onRestart: (r: { to: string }) => void }) {
  const { t } = useLingui();
  const may = useMay();
  const mayCheck = may(SettingsService.method.checkPanelDomain, { install: true });
  const maySet = may(SettingsService.method.setPanelDomain, { install: true });
  const maySetAddress = may(ServerService.method.setServerPublicAddress);
  const fleet = useFleet();
  const controlServer = fleet.status === "connecting" ? undefined : fleet.servers.find((s) => s.controlServer);
  const [addressing, setAddressing] = useState(false);
  const [typed, setTyped] = useState("");
  const [settled, setSettled] = useState("");
  const [confirming, setConfirming] = useState(false);
  const domain = settled.trim().toLowerCase().replace(/\.$/, "");
  useEffect(() => {
    const timer = setTimeout(() => setSettled(typed), CHECK_AFTER_MS);
    return () => clearTimeout(timer);
  }, [typed]);
  const check = useQuery(SettingsService.method.checkPanelDomain, domain && mayCheck.allowed ? { domain } : skipToken, {
    placeholderData: (previous) => previous,
  });
  const found = domain ? check.data : undefined;
  const set = useSetDomain(onRestart);
  const refusal = found?.refusal ?? "";
  const ready =
    owner && domain !== "" && typed.trim() !== "" && !check.isFetching && found !== undefined && refusal === "";
  const address = found?.publicAddresses.find((a) => !a.includes(":")) ?? found?.publicAddresses[0] ?? "";
  const leads = found?.addresses.length
    ? t`It leads to ${found.addresses.join(", ")}`
    : found?.dnsProblem
      ? capital(found.dnsProblem)
      : "";
  return (
    <div className="panel-domain">
      <TextField
        label={t`Domain`}
        mono
        value={typed}
        placeholder="panel.example.com"
        autoComplete="off"
        spellCheck={false}
        error={typed.trim() && refusal ? refusal : undefined}
        hint={t`The panel answers there over HTTPS too. The SSH tunnel keeps working.`}
        onChange={(event) => {
          setTyped(event.target.value);
          setConfirming(false);
        }}
      />
      {found && refusal === "" && address ? (
        <DnsRecord
          type={address.includes(":") ? "AAAA" : "A"}
          name={domain}
          value={address}
          state={
            found.pointsHere
              ? { status: "healthy", word: t`Points here` }
              : { status: "degraded", word: t`DNS doesn't point here yet` }
          }
          onCheck={found.pointsHere ? undefined : () => void check.refetch()}
          checking={check.isFetching}
          note={found.pointsHere || !leads ? undefined : leads}
        />
      ) : null}
      {found && refusal === "" && !address ? (
        // Without this server's public address, where the name leads can't be told from here.
        <Callout
          tone="warning"
          title={t`Dockwell can't tell whether ${domain} leads here`}
          actions={
            controlServer ? (
              <Button size="sm" onClick={() => setAddressing(true)} {...maySetAddress.off}>
                <Trans>Set public address</Trans>
              </Button>
            ) : null
          }
        >
          {leads
            ? t`${leads}. This server's public address isn't known: set it, and Dockwell compares the two.`
            : t`This server's public address isn't known: set it, and Dockwell compares the two.`}
        </Callout>
      ) : null}
      {addressing && controlServer ? (
        <SetPublicAddressSheet
          server={controlServer}
          current={controlServer.network?.ipv4}
          onClose={() => {
            setAddressing(false);
            void check.refetch();
          }}
        />
      ) : null}
      {found?.portsHeld.map((p) => (
        <p key={`${p.port}-${p.program}`} className="settings-sub panel-domain-warn">
          {t`${p.program} holds port ${p.port} on this server, so no certificate can come until it's freed.`}
        </p>
      ))}
      {set.failure ? <Callout tone="critical">{sentence(set.failure)}</Callout> : null}
      {confirming ? (
        <Callout
          tone="info"
          title={t`Dockwell restarts to serve ${domain}`}
          actions={
            <>
              <Button size="sm" variant="ghost" onClick={() => setConfirming(false)}>
                <Trans>Cancel</Trans>
              </Button>
              <Button size="sm" variant="primary" loading={set.busy} onClick={() => void set.save(domain, domain)}>
                <Trans>Restart and serve it</Trans>
              </Button>
            </>
          }
        >
          <Trans>Apps and websites keep running. It asks for your passkey.</Trans>
        </Callout>
      ) : (
        <div className="panel-domain-actions">
          <Button variant="primary" disabled={!ready} onClick={() => setConfirming(true)} {...maySet.off}>
            <Trans>Serve the panel there</Trans>
          </Button>
          {owner ? null : <span className="settings-sub">{t`Only the owner sets it.`}</span>}
        </div>
      )}
    </div>
  );
}

/** A domain set here: why it isn't served, its certificate on its way, or the way onto it. */
function Served({
  panel,
  owner,
  onRestart,
}: {
  panel: PanelServing;
  owner: boolean;
  onRestart: (r: { to: string }) => void;
}) {
  const { t } = useLingui();
  const transport = useTransport();
  const queryClient = useQueryClient();
  const set = useSetDomain(onRestart);
  const may = useMay();
  const maySet = may(SettingsService.method.setPanelDomain, { install: true });
  const mayOpen = may(SettingsService.method.createPanelSignInLink, { install: true });
  const [opening, setOpening] = useState(false);
  const [failure, setFailure] = useState<unknown>();
  const domain = panel.setDomain;
  const here = typeof window !== "undefined" && window.location.hostname === domain;
  const stop = owner ? (
    <Button size="sm" variant="ghost" loading={set.busy} onClick={() => void set.save("", domain)} {...maySet.off}>
      <Trans>Stop using it</Trans>
    </Button>
  ) : null;
  const open = async () => {
    setOpening(true);
    setFailure(undefined);
    try {
      const client = createClient(SettingsService, transport);
      const res = await withStepUpIfAsked(transport, StepUpAction.SIGN_IN_ON_PANEL_DOMAIN, domain, (proof) =>
        client.createPanelSignInLink({ proof }),
      );
      leave.to(res.url);
    } catch (error) {
      setFailure(error);
      setOpening(false);
    }
  };
  const failed = set.failure ?? failure;
  let body: React.ReactNode;
  if (panel.problem) {
    body = (
      <Callout tone="warning" title={t`${domain} isn't served`} actions={stop}>
        {panel.problem}
      </Callout>
    );
  } else if (!panel.certified) {
    body = (
      <Callout
        tone="info"
        title={t`Getting a certificate for ${domain}…`}
        actions={
          <>
            <Button
              size="sm"
              variant="secondary"
              onClick={() =>
                void queryClient.invalidateQueries({
                  queryKey: createConnectQueryKey({
                    schema: SettingsService.method.getSettings,
                    cardinality: undefined,
                  }),
                })
              }
            >
              <Trans>Check again</Trans>
            </Button>
            {stop}
          </>
        }
      >
        <Trans>Once DNS leads here and ports 80 and 443 reach this server, it comes on its own.</Trans>
      </Callout>
    );
  } else if (!here) {
    body = (
      <Callout
        tone="info"
        title={t`The panel answers on ${domain}`}
        actions={
          <>
            {owner ? (
              <Button size="sm" variant="primary" loading={opening} onClick={() => void open()} {...mayOpen.off}>
                <Trans>Open {domain}</Trans>
              </Button>
            ) : null}
            {stop}
          </>
        }
      >
        <Trans>Passkeys belong to the address they were made on: open it, and add one there.</Trans>
      </Callout>
    );
  } else {
    body = <div className="panel-domain-actions">{stop}</div>;
  }
  return (
    <div className="panel-domain">
      {body}
      {failed ? <Callout tone="critical">{sentence(failed)}</Callout> : null}
    </div>
  );
}
