// SPDX-License-Identifier: AGPL-3.0-only

import { useQuery } from "@connectrpc/connect-query";
import { type DomainCheck, DomainCheckState, DomainService, Reachable, type Server } from "@dockwell/api";
import { Callout, DnsRecord, type Status } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link } from "@tanstack/react-router";
import { Fragment, useEffect, useState } from "react";

import { domainAdviceOf, forwardToOf, providerName, useFirewallWords } from "../servers/network";

import "./DomainRecord.css";
import { useMay } from "../auth/may";

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

/**
 * What DNS says of a name, read again until it has every record wanted, so each shows as
 * it arrives. With none wanted, until it points here.
 */
function useDomainCheck(
  server: string,
  domain: string,
  wanted: readonly string[],
): { check: DomainCheck | undefined; again: () => void; checking: boolean } {
  const asked = useSettled(domain);
  // Checking a name is part of planning a route, which is a developer's (ADR 0059).
  const allowed = useMay()(DomainService.method.checkDomain).allowed;
  const read = useQuery(
    DomainService.method.checkDomain,
    { serverId: server, domain: asked },
    {
      enabled: server !== "" && asked !== "" && allowed,
      retry: false,
      refetchInterval: (query) => {
        const check = query.state.data?.check;
        const done =
          wanted.length > 0
            ? wanted.every((address) => check?.found.includes(address))
            : check?.state === DomainCheckState.POINTS_HERE;
        return done ? false : READ_EVERY_MS;
      },
    },
  );
  return {
    check: asked === "" ? undefined : read.data?.check,
    again: () => void read.refetch(),
    checking: read.isFetching,
  };
}

/**
 * Whether DNS has one record, as a state's shape and word, and what it points at
 * instead, if anything: its own address among what DNS found, as an A record says
 * nothing of an AAAA one.
 */
function useRecordWord(): (
  check: DomainCheck | undefined,
  address: string,
) => { status: Status; word: string; note?: string; settled: boolean } | undefined {
  const { t } = useLingui();
  return (check, address) => {
    if (!check) return undefined;
    if (check.found.includes(address)) return { status: "healthy", word: t`Points here`, settled: true };
    const six = address.includes(":");
    const found = check.found.filter((ip) => ip.includes(":") === six).join(", ");
    if (found !== "") {
      return check.state === DomainCheckState.CANT_TELL
        ? {
            status: "unknown",
            word: t`Can't tell`,
            note: t`It points at ${found}, which may be this server.`,
            settled: false,
          }
        : { status: "degraded", word: t`Points elsewhere`, note: t`It points at ${found}.`, settled: false };
    }
    if (check.state === DomainCheckState.UNSPECIFIED)
      return { status: "unknown", word: t`Can't read DNS`, settled: false };
    if (check.state === DomainCheckState.ELSEWHERE && check.found.length === 0) {
      return {
        status: "degraded",
        word: t`Points elsewhere`,
        note: t`It points at a private address that isn't this server's.`,
        settled: false,
      };
    }
    return { status: "deploying", word: t`Not set yet`, settled: false };
  };
}

/** One DNS record to make, ready to copy, with whether DNS has it yet, and a check again until it does. */
function RecordRow({
  domain,
  address,
  read,
}: {
  domain: string;
  address: string;
  read: ReturnType<typeof useDomainCheck>;
}) {
  const said = useRecordWord()(read.check, address);
  return (
    <DnsRecord
      type={address.includes(":") ? "AAAA" : "A"}
      name={domain}
      value={address}
      state={said ? { status: said.status, word: said.word } : undefined}
      note={said?.note}
      onCheck={said && !said.settled ? read.again : undefined}
      checking={read.checking}
    />
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
  onLeave,
}: {
  server: Server | undefined;
  /** What's typed, which may not be a name yet. */
  domain: string;
  /** Whether a tunnel or a proxy in front handles HTTPS, as the form says. */
  tunnel: boolean;
  /** That switch, by name. */
  tunnelName: string;
  /** Closes the form, for a link that goes elsewhere. */
  onLeave: () => void;
}) {
  const { t } = useLingui();
  const firewall = useFirewallWords();
  const advice = domainAdviceOf(server?.network, forwardToOf(server));
  const wanted =
    advice.lead === "record"
      ? advice.records.map((record) => record.address)
      : advice.lead === "tunnel"
        ? [...advice.records.map((record) => record.address), ...(advice.router ? [advice.router] : [])]
        : [];
  const read = useDomainCheck(server?.id ?? "", domain, wanted);
  const check = read.check;
  const name = domain === "" ? t`your domain` : domain;
  if (!server) return null;
  const serverName = server.name;

  const proxied = check?.proxied ? (
    <Callout tone="warning" title={t`${name} points at Cloudflare's proxy`}>
      <Trans>Set the record to DNS only, or turn on "{tunnelName}".</Trans>
    </Callout>
  ) : null;

  if (advice.lead === "tunnel-only") {
    return (
      <div className="domain-record">
        <Callout tone="warning" title={t`This server is behind carrier NAT`}>
          <Trans>Only a tunnel reaches it.</Trans>
        </Callout>
        <CatalogLink onLeave={onLeave} />
      </div>
    );
  }

  if (advice.lead === "tunnel") {
    const { router, forwardTo, records } = advice;
    return (
      <div className="domain-record">
        <p className="domain-record-lead">
          <Trans>This server is on a home or office network: a tunnel reaches it with nothing forwarded.</Trans>
        </p>
        <CatalogLink onLeave={onLeave} />
        {tunnel ? null : (
          <>
            {records.length > 0 ? (
              <>
                <p className="domain-record-lead">
                  <Trans>Or make this record for its own IPv6 address, which reaches visitors with IPv6.</Trans>
                </p>
                {records.map((record) => (
                  <RecordRow key={record.address} domain={name} address={record.address} read={read} />
                ))}
              </>
            ) : null}
            <p className="domain-record-lead">
              {forwardTo ? (
                <Trans>
                  Or forward ports 80 and 443 on your router to this server, at {forwardTo}, and make this record.
                </Trans>
              ) : (
                <Trans>Or forward ports 80 and 443 on your router to this server, and make this record.</Trans>
              )}
            </p>
            {router ? (
              <>
                <RecordRow domain={name} address={router} read={read} />
                <p className="domain-record-hint">
                  <Trans>Your router's address, as far as Dockwell can tell.</Trans>
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
        {tunnel ? (
          <CatalogLink onLeave={onLeave} />
        ) : (
          <>
            <p className="domain-record-hint">
              {provider ? (
                <Trans>Point an A record at this server's public address, which {provider}'s console shows.</Trans>
              ) : (
                <Trans>Point an A record at this server's public address, which your provider's console shows.</Trans>
              )}{" "}
              <Link
                className="domain-record-link"
                to="/servers/$serverId"
                params={{ serverId: server.id }}
                hash="network"
                onClick={onLeave}
              >
                <Trans>Set it on the server</Trans>
              </Link>
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
      {records.map((record) => (
        <Fragment key={record.address}>
          <RecordRow domain={name} address={record.address} read={read} />
          {record.unproven ? (
            <p className="domain-record-hint">
              {provider ? (
                <Trans>That's its way out, which may not be its way in: check it in {provider}'s console.</Trans>
              ) : (
                <Trans>That's its way out, which may not be its way in: check it in your provider's console.</Trans>
              )}
            </p>
          ) : null}
        </Fragment>
      ))}
      {/* What to open at the provider, only once a certificate showed the internet can't reach it. */}
      {advice.provider !== undefined && server.network?.reachable === Reachable.NO ? (
        <div className="domain-record-blocked">
          <p className="domain-record-lead">
            <Trans>The internet can't reach {serverName} on ports 80 and 443.</Trans>
          </p>
          <details className="domain-record-how">
            <summary>
              <Trans>How to open it</Trans>
            </summary>
            <p>{firewall(advice.provider)}</p>
          </details>
        </div>
      ) : null}
      {proxied}
    </div>
  );
}
