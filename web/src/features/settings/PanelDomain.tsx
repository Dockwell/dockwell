// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { createConnectQueryKey, skipToken, useQuery, useTransport } from "@connectrpc/connect-query";
import {
  type CheckPanelDomainResponse,
  PanelDomainFrom,
  PanelServedBy,
  type PanelServing,
  Role,
  ServerService,
  SettingsService,
  StepUpAction,
} from "@dockwell/api";
import { Button, Callout, DnsRecord, SegmentedControl, TextField } from "@dockwell/ui";
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

/** How the domain reaches the panel: Dockwell serves it, or a tunnel or proxy that ends TLS does (ADR 0063). */
type Via = "dockwell" | "proxy";

/**
 * The panel's domain, set here (ADR 0063): a field checked as it's typed, served by
 * Dockwell or behind a tunnel, a save the owner confirms with a passkey, which
 * restarts Dockwell to serve it, then the way onto it. A domain dockwell run was
 * given stays where it was given.
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
  return <DomainField owner={owner} listen={now.listen} onRestart={onRestart} />;
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
  const save = async (domain: string, confirmOn: string, behindProxy = false) => {
    setBusy(true);
    setFailure(undefined);
    try {
      const client = createClient(SettingsService, transport);
      // The passkey confirms how it's served too, as the server's target says (settings.PanelDomainTarget).
      const target = domain && behindProxy ? `${confirmOn} behind a proxy` : confirmOn;
      const res = await withStepUpIfAsked(transport, StepUpAction.SET_PANEL_DOMAIN, target, (proof) =>
        client.setPanelDomain({ domain, proof, behindProxy }),
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

/** A live check of the way in through a tunnel or proxy, which asks the domain for a token. */
function useReach(domain: string) {
  const may = useMay();
  const mayCheck = may(SettingsService.method.checkPanelReach, { install: true });
  return useQuery(SettingsService.method.checkPanelReach, domain && mayCheck.allowed ? { domain } : skipToken, {
    placeholderData: (previous) => previous,
  });
}

type Reach = ReturnType<typeof useReach>;

/**
 * The way in through a tunnel or proxy, checked live: the control plane asks the
 * domain for a token, and this says whether the panel answered, with where the
 * tunnel should point, ready to copy, and why not.
 */
function TunnelRecord({ domain, listen, reach }: { domain: string; listen: string; reach: Reach }) {
  const { t } = useLingui();
  const found = reach.data;
  const target = found?.target || (listen ? `http://${listen}` : "");
  if (!target) return null;
  return (
    <DnsRecord
      type={t`Tunnel`}
      name={domain}
      value={target}
      state={
        found
          ? found.reached
            ? { status: "healthy", word: t`Reached through your tunnel` }
            : { status: "degraded", word: t`Not reached yet` }
          : undefined
      }
      onCheck={found?.reached ? undefined : () => void reach.refetch()}
      checking={reach.isFetching}
      note={found && !found.reached && found.problem ? capital(found.problem) : undefined}
    />
  );
}

/**
 * Where a domain Dockwell serves itself leads, from a check: the record to make, with
 * its state, or why that can't be told and the way to fix it, and what holds a port
 * its certificate needs. The field and the way back from a tunnel show the same.
 */
function DomainLeads({
  domain,
  found,
  checking,
  onCheck,
}: {
  domain: string;
  found: CheckPanelDomainResponse;
  checking: boolean;
  onCheck: () => void;
}) {
  const { t } = useLingui();
  const may = useMay();
  const maySetAddress = may(ServerService.method.setServerPublicAddress);
  const fleet = useFleet();
  const controlServer = fleet.status === "connecting" ? undefined : fleet.servers.find((s) => s.controlServer);
  const [addressing, setAddressing] = useState(false);
  const address = found.publicAddresses.find((a) => !a.includes(":")) ?? found.publicAddresses[0] ?? "";
  const leads = found.addresses.length
    ? t`It leads to ${found.addresses.join(", ")}`
    : found.dnsProblem
      ? capital(found.dnsProblem)
      : "";
  return (
    <>
      {address ? (
        <DnsRecord
          type={address.includes(":") ? "AAAA" : "A"}
          name={domain}
          value={address}
          state={
            found.pointsHere
              ? { status: "healthy", word: t`Points here` }
              : { status: "degraded", word: t`DNS doesn't point here yet` }
          }
          onCheck={found.pointsHere ? undefined : onCheck}
          checking={checking}
          note={found.pointsHere || !leads ? undefined : leads}
        />
      ) : (
        // Without this server's public address, where the name leads can't be told from here.
        <Callout
          stacked
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
      )}
      {addressing && controlServer ? (
        <SetPublicAddressSheet
          server={controlServer}
          current={controlServer.network?.ipv4}
          onClose={() => {
            setAddressing(false);
            onCheck();
          }}
        />
      ) : null}
      {found.portsHeld.map((p) => (
        <p key={`${p.port}-${p.program}`} className="settings-sub panel-domain-warn">
          {t`${p.program} holds port ${p.port} on this server, so no certificate can come until it's freed.`}
        </p>
      ))}
    </>
  );
}

/** A domain to give the panel, checked as it's typed. */
function DomainField({
  owner,
  listen,
  onRestart,
}: {
  owner: boolean;
  listen: string;
  onRestart: (r: { to: string }) => void;
}) {
  const { t } = useLingui();
  const may = useMay();
  const mayCheck = may(SettingsService.method.checkPanelDomain, { install: true });
  const maySet = may(SettingsService.method.setPanelDomain, { install: true });
  const [typed, setTyped] = useState("");
  const [settled, setSettled] = useState("");
  const [confirming, setConfirming] = useState(false);
  const [chosen, setChosen] = useState<Via | undefined>();
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
  // A name that leads to Cloudflare is behind its proxy or tunnel, which ends TLS before this server.
  const cloudflare = found?.cloudflare ?? false;
  // On Cloudflare the way in is asked too: a tunnel that reaches this panel chooses
  // itself, and Cloudflare's proxy without one leaves Dockwell serving it.
  const askReach =
    found !== undefined && refusal === "" && (chosen === "proxy" || (chosen === undefined && cloudflare));
  const reach = useReach(askReach ? domain : "");
  const tunnelled = cloudflare && (reach.data?.reached ?? false) && !reach.isPlaceholderData;
  const via: Via = chosen ?? (tunnelled ? "proxy" : "dockwell");
  const ways = [
    { value: "dockwell" as const, label: t`Dockwell serves it` },
    { value: "proxy" as const, label: t`Behind a tunnel or proxy` },
  ];
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
      {found && refusal === "" ? (
        <div className="panel-domain-via">
          <span className="dw-field-label" aria-hidden="true">
            <Trans>How it reaches this server</Trans>
          </span>
          <SegmentedControl<Via>
            label={t`How it reaches this server`}
            size="sm"
            value={via}
            // In one order always: the choice moving as each check lands could take a click meant for the other.
            options={ways}
            onChange={(value) => {
              setChosen(value);
              setConfirming(false);
            }}
          />
        </div>
      ) : null}
      {found && refusal === "" && via === "proxy" ? (
        <TunnelRecord domain={domain} listen={listen} reach={reach} />
      ) : null}
      {found && refusal === "" && via === "dockwell" ? (
        <DomainLeads domain={domain} found={found} checking={check.isFetching} onCheck={() => void check.refetch()} />
      ) : null}
      {set.failure ? (
        <Callout stacked tone="critical">
          {sentence(set.failure)}
        </Callout>
      ) : null}
      {confirming ? (
        <Callout
          stacked
          tone="info"
          title={t`Dockwell restarts to serve ${domain}`}
          actions={
            <>
              <Button size="sm" variant="ghost" onClick={() => setConfirming(false)}>
                <Trans>Cancel</Trans>
              </Button>
              <Button
                size="sm"
                variant="primary"
                loading={set.busy}
                onClick={() => void set.save(domain, domain, via === "proxy")}
              >
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
  const mayCheck = may(SettingsService.method.checkPanelDomain, { install: true });
  const [opening, setOpening] = useState(false);
  const [failure, setFailure] = useState<unknown>();
  // A move between the two ways of serving the same domain, waiting for its confirm.
  const [switching, setSwitching] = useState<Via | undefined>();
  const domain = panel.setDomain;
  const behind = panel.servedBy === PanelServedBy.PROXY;
  const waiting = !panel.problem && !behind && !panel.certified;
  // A certificate that doesn't come may be one a tunnel stands in front of: a domain on
  // Cloudflare is asked through the way in, as its proxy without a tunnel needs one.
  // Before Dockwell serves it itself, as before a domain is set, where its record leads
  // and what holds the ports its certificate needs.
  const dns = useQuery(
    SettingsService.method.checkPanelDomain,
    (waiting || switching === "dockwell") && mayCheck.allowed ? { domain } : skipToken,
  );
  // A domain that isn't served is asked too, as the way back to its tunnel may be one restart.
  const reach = useReach(
    (behind && !panel.problem) || (waiting && dns.data?.cloudflare) || (panel.problem && domain) ? domain : "",
  );
  const reached = (reach.data?.reached ?? false) && !reach.isPlaceholderData;
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
  const toTunnel = owner ? (
    <Button
      size="sm"
      variant={(waiting || panel.problem) && reached ? "primary" : "ghost"}
      onClick={() => setSwitching("proxy")}
      {...maySet.off}
    >
      <Trans>Serve it behind a tunnel</Trans>
    </Button>
  ) : null;
  const toCertificate = owner ? (
    <Button size="sm" variant="ghost" onClick={() => setSwitching("dockwell")} {...maySet.off}>
      <Trans>Serve it with its own certificate</Trans>
    </Button>
  ) : null;
  // Ports another program holds would leave the domain served nowhere after the
  // restart, so the way back from a tunnel waits for them (the server refuses too).
  const portHeld = switching === "dockwell" && (dns.data?.portsHeld.length ?? 0) > 0;
  const opener = (
    <Callout
      stacked
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
  let body: React.ReactNode;
  const confirm = (
    <>
      <Button size="sm" variant="ghost" onClick={() => setSwitching(undefined)}>
        <Trans>Cancel</Trans>
      </Button>
      <Button
        size="sm"
        variant="primary"
        loading={set.busy}
        disabled={portHeld || (switching === "dockwell" && !dns.data)}
        onClick={() => void set.save(domain, domain, switching === "proxy")}
      >
        <Trans>Restart and serve it</Trans>
      </Button>
    </>
  );
  if (switching === "proxy") {
    body = (
      <Callout stacked tone="info" title={t`Dockwell restarts to serve ${domain} behind your tunnel`} actions={confirm}>
        <Trans>Apps, websites and passkeys keep working. It asks for your passkey.</Trans>
      </Callout>
    );
  } else if (switching === "dockwell") {
    // As the field does before a domain is set: where it leads, and what holds a port.
    // Off the tunnel, the domain answers only once its record leads here and a
    // certificate comes, which the confirm says where it doesn't yet, or the person
    // is on it now (#734's re-check, N1).
    const dark = here || !dns.data?.pointsHere;
    body = (
      <>
        {dns.data ? (
          <DomainLeads domain={domain} found={dns.data} checking={dns.isFetching} onCheck={() => void dns.refetch()} />
        ) : null}
        <Callout
          stacked
          tone={dark ? "warning" : "info"}
          title={t`Dockwell restarts to serve ${domain} with its own certificate`}
          actions={confirm}
        >
          {dark ? (
            <Trans>
              {domain} stops answering through the tunnel until its record leads here and a certificate comes. The SSH
              tunnel keeps working.
            </Trans>
          ) : (
            <Trans>Apps, websites and passkeys keep working. It asks for your passkey.</Trans>
          )}
        </Callout>
      </>
    );
  } else if (panel.problem) {
    body = (
      <Callout
        stacked
        tone="warning"
        title={t`${domain} isn't served`}
        actions={
          <>
            {toTunnel}
            {stop}
          </>
        }
      >
        {panel.problem}
      </Callout>
    );
  } else if (behind) {
    body = (
      <>
        <TunnelRecord domain={domain} listen={panel.listen} reach={reach} />
        {reached && !here ? opener : null}
        <div className="panel-domain-actions">
          {toCertificate}
          {reached && !here ? null : stop}
        </div>
      </>
    );
  } else if (waiting && reached) {
    // The check reached this panel through a tunnel, which ends TLS before it.
    body = (
      <Callout
        stacked
        tone="warning"
        title={t`${domain} reaches this panel through a tunnel`}
        actions={
          <>
            {toTunnel}
            {stop}
          </>
        }
      >
        <Trans>The tunnel ends TLS, so no certificate comes here.</Trans>
      </Callout>
    );
  } else if (!panel.certified) {
    body = (
      <Callout
        stacked
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
            {toTunnel}
            {stop}
          </>
        }
      >
        <Trans>Once DNS leads here and ports 80 and 443 reach this server, it comes on its own.</Trans>
      </Callout>
    );
  } else if (!here) {
    body = opener;
  } else {
    body = <div className="panel-domain-actions">{stop}</div>;
  }
  return (
    <div className="panel-domain">
      {body}
      {failed ? (
        <Callout stacked tone="critical">
          {sentence(failed)}
        </Callout>
      ) : null}
    </div>
  );
}
