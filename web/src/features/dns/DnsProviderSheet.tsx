// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import {
  ChangeService,
  type CheckDnsProviderResponse,
  type DnsProviderInfo,
  DnsProviderState,
  DomainService,
} from "@dockwell/api";
import { Advanced, Button, Callout, Sheet, StatusBadge, TextField } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { useEffect, useMemo, useRef, useState } from "react";

import { sentence } from "../../lib/errors";
import { useMay } from "../auth/may";
import { useChangeSheet } from "../changes/ChangeSheet";
import { CLOUDFLARE_TOKENS, freeProviderName } from "./dns";
import "./Dns.css";

/** How long typing pauses before a token is checked. */
const CHECK_AFTER_MS = 600;

/**
 * Connects Cloudflare's DNS, or changes a connection, as a plan (ADR 0088). A person
 * pastes an API token; the control plane checks it as they type and lists the zones
 * it reaches, the only one picked, and others for them to pick, as each picked is
 * written in; it writes and deletes a TXT record in each, saying Can write DNS before
 * anything is planned. A token that reaches more than is picked says so, with where to
 * scope it. The plan saves the token, sealed, and it
 * never comes back. Changing one asks for no token again.
 */
export function DnsProviderSheet({
  existing,
  editing,
  onClose,
}: {
  existing: readonly DnsProviderInfo[];
  editing?: DnsProviderInfo;
  onClose: () => void;
}) {
  const { t } = useLingui();
  const may = useMay();
  const mayPlan = may(ChangeService.method.proposeChange, { install: true });
  // Checking writes in the zones, so it's an admin's too, signed in.
  const mayCheck = may(DomainService.method.checkDnsProvider);
  const transport = useTransport();
  const client = useMemo(() => createClient(DomainService, transport), [transport]);
  const sheet = useChangeSheet();

  const [token, setToken] = useState("");
  // The zones picked, once the check has said which the token reaches: the only one
  // it reaches, or none of several, as a check writes in each one picked; until
  // then, the saved ones.
  const [picked, setPicked] = useState<string[] | undefined>(editing ? [...editing.zones] : undefined);
  const [typedName, setTypedName] = useState<string>();
  const [answer, setAnswer] = useState<CheckDnsProviderResponse>();
  const [checking, setChecking] = useState(false);
  const [failure, setFailure] = useState<string>();
  const asked = useRef(0);

  const name = editing?.name ?? typedName ?? freeProviderName("cloudflare", existing);
  const checkable = mayCheck.allowed && (token !== "" || editing !== undefined);
  // No zone picked yet and none of several picked ask the same, so neither asks again.
  const what = JSON.stringify([token, picked ?? []]);
  // biome-ignore lint/correctness/useExhaustiveDependencies: it checks again whenever the token or the zones change, which `what` holds
  useEffect(() => {
    const mine = ++asked.current;
    setFailure(undefined);
    if (!checkable) {
      setAnswer(undefined);
      setChecking(false);
      return;
    }
    setChecking(true);
    const timer = setTimeout(async () => {
      try {
        const said = await client.checkDnsProvider({
          name: token ? "" : (editing?.name ?? ""),
          provider: "cloudflare",
          apiToken: token,
          zones: picked ?? [],
        });
        if (mine !== asked.current) return;
        setAnswer(said);
        // The only zone the token reaches, picked; of several, none until a person picks.
        if (picked === undefined && said.state === DnsProviderState.REACHES) {
          setPicked(said.reaches.length === 1 ? [...said.reaches] : []);
        }
      } catch (err) {
        if (mine === asked.current) setFailure(sentence(err));
      } finally {
        if (mine === asked.current) setChecking(false);
      }
    }, CHECK_AFTER_MS);
    return () => clearTimeout(timer);
  }, [what, checkable]);

  const zones = picked ?? [];
  const writes = answer?.state === DnsProviderState.WRITES && !checking;
  const changed = editing ? token !== "" || zones.join() !== editing.zones.join() : true;
  const ready = writes && zones.length > 0 && changed;
  const offered = [...new Set([...(answer?.reaches ?? []), ...(editing?.zones ?? [])])].sort();
  // The token reaches more than it's used for, whether or not it has written yet.
  const good = answer?.state === DnsProviderState.WRITES || answer?.state === DnsProviderState.REACHES;
  const wider = good && !checking && (answer?.reaches.length ?? 0) > zones.length;

  const review = () =>
    sheet.open({
      kind: "propose",
      summary: editing ? t`Change the DNS provider ${name}` : t`Prove names by DNS through Cloudflare`,
      edits: [
        {
          resource: { kind: "dns.provider", name },
          spec: { kind: { case: "dnsProvider", value: { provider: "cloudflare", zones, apiToken: token } } },
        },
      ],
      onClose: (change) => {
        if (change) onClose();
      },
    });
  const remove = () =>
    sheet.open({
      kind: "propose",
      summary: t`Remove the DNS provider ${name}`,
      edits: [{ resource: { kind: "dns.provider", name }, delete: true }],
      onClose: (change) => {
        if (change) onClose();
      },
    });

  const stateLabel = (a: CheckDnsProviderResponse) => {
    switch (a.state) {
      case DnsProviderState.WRITES:
        return zones.length === 1 ? t`Can write DNS for ${zones[0]}` : t`Can write DNS for ${zones.length} zones`;
      case DnsProviderState.REFUSED:
        return t`Wrong token`;
      case DnsProviderState.CANT_WRITE:
        return t`Can't write DNS`;
      case DnsProviderState.REACHES:
        return zones.length === 0 ? t`Token works · pick its zones` : t`Checking`;
    }
    return t`Can't reach Cloudflare`;
  };
  const stateStatus = (a: CheckDnsProviderResponse) =>
    a.state === DnsProviderState.WRITES
      ? "healthy"
      : a.state === DnsProviderState.REACHES
        ? zones.length === 0
          ? "healthy"
          : "deploying"
        : a.state === DnsProviderState.UNREACHABLE
          ? "unknown"
          : "down";
  const zoneSaid = (zone: string) => answer?.zones.find((z) => z.zone === zone && !z.writes)?.detail;

  return (
    <Sheet
      open
      onClose={onClose}
      title={editing ? t`DNS provider ${editing.name}` : t`Connect Cloudflare`}
      footer={
        <>
          {editing ? (
            <Button variant="ghost" className="dns-remove" onClick={remove} {...mayPlan.off}>
              <Trans>Remove</Trans>
            </Button>
          ) : null}
          <Button variant="secondary" onClick={onClose}>
            <Trans>Cancel</Trans>
          </Button>
          <Button variant="primary" disabled={!ready} onClick={review} {...mayPlan.off}>
            <Trans>Review plan</Trans>
          </Button>
        </>
      }
    >
      <div className="dns-sheet">
        <TextField
          label={t`API token`}
          mono
          type="password"
          autoComplete="off"
          value={token}
          placeholder={editing ? t`Kept unless you paste one` : undefined}
          hint={t`An API token with Zone:Read and DNS:Edit for your zones.`}
          onChange={(event) => {
            setToken(event.target.value.trim());
            if (!editing) setPicked(undefined);
          }}
        />
        <a href={CLOUDFLARE_TOKENS} target="_blank" rel="noreferrer" className="cp-link">
          <Trans>Make a token at Cloudflare</Trans>
        </a>
        <div className="dns-sheet-check" aria-live="polite">
          {checking ? (
            <StatusBadge status="deploying" label={t`Checking`} labelIsState />
          ) : answer ? (
            <StatusBadge status={stateStatus(answer)} label={stateLabel(answer)} labelIsState />
          ) : null}
          {answer && !checking && answer.state !== DnsProviderState.WRITES && answer.detail ? (
            <p className="dns-sheet-said">{answer.detail}</p>
          ) : null}
          {wider ? (
            <p className="dns-sheet-said">
              {t`The token reaches ${answer?.reaches.length ?? 0} zones.`}{" "}
              <a href={CLOUDFLARE_TOKENS} target="_blank" rel="noreferrer" className="cp-link">
                <Trans>Scope it to these zones</Trans>
              </a>
            </p>
          ) : null}
        </div>
        {offered.length > 0 ? (
          <fieldset className="dns-zones">
            <legend>{t`Zones`}</legend>
            {offered.map((zone) => (
              <label key={zone} className="dns-zone">
                <input
                  type="checkbox"
                  checked={zones.includes(zone)}
                  onChange={(event) =>
                    setPicked(event.target.checked ? [...zones, zone].sort() : zones.filter((z) => z !== zone))
                  }
                />
                <span>{zone}</span>
                {zoneSaid(zone) ? <span className="dns-zone-said">{zoneSaid(zone)}</span> : null}
              </label>
            ))}
          </fieldset>
        ) : null}
        {failure ? (
          <Callout tone="critical" title={t`Not checked`}>
            {failure}
          </Callout>
        ) : null}
        {editing ? null : (
          <Advanced summary={t`Named ${name}`}>
            <TextField
              label={t`Name`}
              mono
              value={name}
              hint={t`What a certificate names it by, in the CLI.`}
              onChange={(event) => setTypedName(event.target.value)}
            />
          </Advanced>
        )}
      </div>
    </Sheet>
  );
}
