// SPDX-License-Identifier: AGPL-3.0-only

import { useQuery } from "@connectrpc/connect-query";
import { type DomainCheck, DomainCheckState, DomainService, type Server } from "@dockwell/api";
import { Callout, SegmentedControl, type Status, StatusBadge } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link } from "@tanstack/react-router";
import { Fragment, useEffect, useState } from "react";

import { CopyButton } from "../servers/CopyButton";
import { domainAdviceOf, providerName, useFirewallWords } from "../servers/network";

import "./DomainRecord.css";

/** How long typing has to stop before a name is looked up, and how often DNS is read again until it has the record. */
const SETTLES_MS = 400;
const READ_EVERY_MS = 5000;

/** A name worth asking DNS about: one with a dot in it, that's stopped changing. */
function useSettled(value: string): string {
  const [settled, setSettled] = useState(value);
  useEffect(() => {
    const timer = setTimeout(() => setSettled(value), SETTLES_MS);
    return () => clearTimeout(timer);
  }, [value]);
  return settled === value && value.includes(".") ? value : "";
}

/** What DNS says of a name, read again until it points here, so a record shows as it arrives. */
function useDomainCheck(server: string, domain: string): DomainCheck | undefined {
  const asked = useSettled(domain);
  const read = useQuery(
    DomainService.method.checkDomain,
    { serverId: server, domain: asked },
    {
      enabled: server !== "" && asked !== "",
      retry: false,
      refetchInterval: (query) =>
        query.state.data?.check?.state === DomainCheckState.POINTS_HERE ? false : READ_EVERY_MS,
    },
  );
  return asked === "" ? undefined : read.data?.check;
}

/** Whether DNS has the record, as a state's shape and word. */
function useDnsWord(check: DomainCheck | undefined): [Status, string] | undefined {
  const { t } = useLingui();
  if (!check) return undefined;
  const found = check.found.join(", ");
  switch (check.state) {
    case DomainCheckState.POINTS_HERE:
      return ["healthy", t`DNS has this record`];
    case DomainCheckState.NO_RECORD:
      return ["deploying", t`Waiting for DNS`];
    case DomainCheckState.ELSEWHERE:
      return ["degraded", t`DNS points at ${found}`];
    case DomainCheckState.CANT_TELL:
      return ["unknown", t`DNS points at ${found}, and Dockwell can't tell whether that's this server`];
    default:
      return ["unknown", t`Can't read DNS`];
  }
}

/** One DNS record to make, ready to copy, with whether DNS has it yet. */
function RecordRow({ domain, address, check }: { domain: string; address: string; check: DomainCheck | undefined }) {
  const { t } = useLingui();
  const kind = address.includes(":") ? "AAAA" : "A";
  const word = useDnsWord(check);
  return (
    <>
      <div className="domain-record-row">
        <code className="domain-record-kind">{kind}</code>
        <code className="domain-record-name">{domain}</code>
        <code className="domain-record-address">{address}</code>
        <CopyButton text={address} label={t`Copy ${address}`} />
      </div>
      {word ? <StatusBadge status={word[0]} label={word[1]} labelIsState /> : null}
    </>
  );
}

/** Where to get a tunnel. It leaves the form, as it's another install. */
function CatalogLink({ onLeave }: { onLeave: () => void }) {
  return (
    <Link className="domain-record-link" to="/catalog/$entryId" params={{ entryId: "cloudflared" }} onClick={onLeave}>
      <Trans>Install Cloudflare Tunnel from the Catalog</Trans>
    </Link>
  );
}

/**
 * Says what to do so a domain reaches a server, by what's known of it (ADR 0034): the
 * record to make, or a tunnel first, as a home router needs a forwarded port and a
 * carrier's NAT can't have one. With nothing known, it asks. What it says of a record
 * follows DNS as the record arrives.
 */
export function DomainRecord({
  server,
  domain,
  tunnel,
  tunnelName,
  onTunnel,
  onLeave,
}: {
  server: Server | undefined;
  /** What's typed, which may not be a name yet. */
  domain: string;
  /** Whether a tunnel or a proxy in front handles HTTPS, as the form says. */
  tunnel: boolean;
  /** That switch, by name. */
  tunnelName: string;
  onTunnel: (on: boolean) => void;
  /** Closes the form, for a link that goes elsewhere. */
  onLeave: () => void;
}) {
  const { t } = useLingui();
  const firewall = useFirewallWords();
  const check = useDomainCheck(server?.id ?? "", domain);
  const advice = domainAdviceOf(server?.network);
  const name = domain === "" ? t`your domain` : domain;
  if (!server) return null;

  const proxied = check?.proxied ? (
    <Callout tone="warning">
      <Trans>
        {name} points at Cloudflare's proxy, which blocks the certificate check. Turn on "{tunnelName}", or set the
        record to DNS only.
      </Trans>
    </Callout>
  ) : null;

  if (advice.lead === "tunnel-only") {
    return (
      <div className="domain-record">
        <Callout tone="warning">
          <Trans>
            This server is behind carrier NAT, so a DNS record can't reach it, and neither can a forwarded port. A
            tunnel needs neither.
          </Trans>
        </Callout>
        <CatalogLink onLeave={onLeave} />
      </div>
    );
  }

  if (advice.lead === "tunnel") {
    const router = advice.router;
    return (
      <div className="domain-record">
        <p className="domain-record-lead">
          <Trans>This looks like a home or office network, so the internet can't reach this server by itself.</Trans>
        </p>
        <CatalogLink onLeave={onLeave} />
        <p className="domain-record-hint">
          <Trans>Recommended. It needs no public address and no forwarded port.</Trans>
        </p>
        {tunnel ? null : (
          <>
            <p className="domain-record-lead">
              <Trans>Or forward ports 80 and 443 on your router to this server, and make this record.</Trans>
            </p>
            {router ? (
              <>
                <RecordRow domain={name} address={router} check={check} />
                <p className="domain-record-hint">
                  <Trans>That's your router's address, as far as Dockwell can tell.</Trans>
                </p>
              </>
            ) : (
              <p className="domain-record-hint">
                <Trans>Point an A record at your router's public address.</Trans>
              </p>
            )}
            {proxied}
          </>
        )}
      </div>
    );
  }

  if (advice.lead === "ask") {
    const provider = providerName(advice.provider);
    return (
      <div className="domain-record">
        <SegmentedControl
          label={t`How traffic reaches this server`}
          value={tunnel ? "tunnel" : "direct"}
          options={[
            { value: "direct", label: t`Directly` },
            { value: "tunnel", label: t`Through a tunnel or a proxy` },
          ]}
          onChange={(how) => onTunnel(how === "tunnel")}
        />
        {tunnel ? (
          <CatalogLink onLeave={onLeave} />
        ) : (
          <>
            <p className="domain-record-hint">
              {provider ? (
                <Trans>
                  Dockwell doesn't know this server's public address. {provider}'s console shows it. Point an A record
                  at it, and set it under Network on this server's page.
                </Trans>
              ) : (
                <Trans>
                  Dockwell doesn't know this server's public address. Your provider's console shows it. Point an A
                  record at it, and set it under Network on this server's page.
                </Trans>
              )}
            </p>
            {proxied}
          </>
        )}
      </div>
    );
  }

  const records = advice.records;
  const provider = providerName(advice.provider);
  return tunnel ? null : (
    <div className="domain-record">
      <p className="domain-record-lead">
        {records.length > 1 ? (
          <Trans>Make these records at your DNS provider.</Trans>
        ) : (
          <Trans>Make this record at your DNS provider.</Trans>
        )}
      </p>
      {records.map((record, at) => (
        <Fragment key={record.address}>
          {/* What DNS says goes under the last, as one read says it of them all. */}
          <RecordRow domain={name} address={record.address} check={at === records.length - 1 ? check : undefined} />
          {record.unproven ? (
            <p className="domain-record-hint">
              {provider ? (
                <Trans>
                  That's how this server reaches the internet, which may not be its way in. Check it against {provider}
                  's console.
                </Trans>
              ) : (
                <Trans>
                  That's how this server reaches the internet, which may not be its way in. Check it against your
                  provider's console.
                </Trans>
              )}
            </p>
          ) : null}
        </Fragment>
      ))}
      {advice.provider !== undefined ? <p className="domain-record-hint">{firewall(advice.provider)}</p> : null}
      {proxied}
    </div>
  );
}
