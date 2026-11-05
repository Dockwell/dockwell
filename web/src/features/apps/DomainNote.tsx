// SPDX-License-Identifier: AGPL-3.0-only

import type { Domain, DomainRoute } from "@dockwell/api";
import { type Status, StatusBadge } from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link } from "@tanstack/react-router";
import { useEffect, useState } from "react";

import { asksNoCertificate, type DomainLook, panelWords, plainForAnyone, SILENT_AFTER_MS } from "./domains";

import "./DomainNote.css";

/** The switch on the form, by name, wherever the server's words would name a field only the API has. */
export function useTunnelSwitchName(): string {
  const { t } = useLingui();
  return t`A tunnel or a proxy in front handles HTTPS`;
}

/** True once active has held for ms without a break. */
function useLongWhile(active: boolean, ms: number): boolean {
  const [long, setLong] = useState(false);
  useEffect(() => {
    if (!active) {
      setLong(false);
      return;
    }
    const timer = setTimeout(() => setLong(true), ms);
    return () => clearTimeout(timer);
  }, [active, ms]);
  return active && long;
}

/**
 * True once a server has said nothing of a domain for about a minute. It's a server to
 * go and look at then, rather than something about to change.
 */
export function useSilent(look: DomainLook): boolean {
  return useLongWhile(look.unreported !== undefined, SILENT_AFTER_MS);
}

/** A state's shape, word and colour, as every state has in the brand book, and the port behind it. */
export function DomainFacts({
  domain,
  route,
  look,
  silent,
}: {
  domain: Domain;
  route: DomainRoute | undefined;
  look: DomainLook;
  silent: boolean;
}) {
  const { t } = useLingui();
  const [status, word] = ((): [Status, string] => {
    if (look.state === "failed") return ["down", t`Failed`];
    if (look.orphaned) return ["stopped", t`No server`];
    if (silent) return ["stopped", t`No report`];
    if (look.noCertificate) {
      return plainForAnyone(domain) ? ["degraded", t`Plain HTTP`] : ["healthy", t`TLS ends in front`];
    }
    if (look.warning) return ["degraded", t`Renewal failing`];
    if (look.state === "serving") {
      if (look.expires === undefined) return ["healthy", t`Valid`];
      const days = Math.max(0, Math.ceil((look.expires - Date.now()) / 86_400_000));
      return ["healthy", plural(days, { one: "Valid for # day", other: "Valid for # days" })];
    }
    return ["deploying", asksNoCertificate(domain) ? t`Waiting for the server` : t`Issuing`];
  })();
  const port = route?.hostPort || route?.port;
  return (
    <span className="domain-facts">
      <StatusBadge status={status} label={word} labelIsState />
      {port ? (
        <span className="domain-port">
          <Trans>
            Port <code>{port}</code>
          </Trans>
        </span>
      ) : null}
    </span>
  );
}

/**
 * What a row says beneath its name, when its state's word isn't enough: what the server
 * says of it, or what to go and look at. Nothing, for a domain that's simply serving.
 */
export function DomainNote({
  domain,
  look,
  silent,
  onLeave,
}: {
  domain: Domain;
  look: DomainLook;
  silent: boolean;
  /** Closes what the row is in, when its link goes elsewhere. */
  onLeave?: () => void;
}) {
  const switchName = useTunnelSwitchName();
  if (look.orphaned) {
    return <Trans>No server runs this any more, so nothing answers for it. Remove it, or place it on a server.</Trans>;
  }
  if (look.noCertificate) {
    return plainForAnyone(domain) ? (
      <Trans>It answers anyone who can reach this server over plain HTTP, around whatever is in front.</Trans>
    ) : null;
  }
  if (look.message) return <>{panelWords(look.message, switchName)}</>;
  if (silent && look.unreported) {
    const server = look.unreported.name;
    return (
      <Trans>
        <code>{server}</code> hasn't reported yet.{" "}
        <Link
          className="domain-note-link"
          to="/servers/$serverId"
          params={{ serverId: look.unreported.id }}
          {...(onLeave ? { onClick: onLeave } : {})}
        >
          Check its page
        </Link>
      </Trans>
    );
  }
  return null;
}
