// SPDX-License-Identifier: AGPL-3.0-only

import type { Domain, DomainRoute } from "@dockwell/api";
import { type Status, StatusBadge } from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link } from "@tanstack/react-router";
import { type ReactNode, useRef } from "react";

import { useTimeWords } from "../../lib/time";
import { useControlPlaneNow, useFleetView } from "../servers/useFleet";

import {
  asksNoCertificate,
  type DomainLook,
  type DomainRead,
  panelWords,
  plainForAnyone,
  SILENT_AFTER_MS,
  useRouteTimes,
} from "./domains";
import { useHealthWords } from "./Health";

import "./DomainNote.css";

/** The switch on the form, by name, wherever the server's words would name a field only the API has. */
export function useTunnelSwitchName(): string {
  const { t } = useLingui();
  return t`A tunnel or a proxy in front handles HTTPS`;
}

/** How often whether a server has gone quiet is looked at again. */
const SILENCE_TICK_MS = 5_000;

/**
 * Whether a server has said nothing of a domain for about a minute, so that it's one to
 * go and look at, rather than something about to change. An agent says where a domain
 * stands within seconds of connecting, and of a route changing, and a report has no time
 * of its own, so its silence is counted from the later of those, and doesn't start over
 * on each visit. Where one of them can't be read, it's counted from when this screen
 * first found it.
 */
export function useSilence(now: number): (domain: Domain, look: DomainLook) => boolean {
  const { servers } = useFleetView();
  const routes = useRouteTimes();
  const found = useRef(new Map<string, number>());
  return (domain, look) => {
    if (!look.unreported) {
      found.current.delete(domain.name);
      return false;
    }
    const { id } = look.unreported;
    const connected = servers?.find((server) => server.id === id)?.connectedAt;
    const applied = domain.routes.map((route) => routes?.get(route.id));
    const known = connected !== undefined && applied.every((at) => at !== undefined);
    const since = known ? Math.max(connected, ...(applied as number[])) : (found.current.get(domain.name) ?? now);
    if (!known) found.current.set(domain.name, since);
    return now - since >= SILENT_AFTER_MS;
  };
}

/** useSilence, for one domain. */
export function useSilent(domain: Domain, look: DomainLook): boolean {
  return useSilence(useControlPlaneNow(SILENCE_TICK_MS))(domain, look);
}

/** Whom a route sends to: its app, or a port on the server that's no app's. */
export function useRouteTarget(): (route: { service: string; hostPort: number }) => string {
  const { t } = useLingui();
  return ({ service, hostPort }) => service || t`port ${hostPort} on the server`;
}

/** A state's shape and word, as every state has in the brand book. */
export function useDomainWord(domain: Domain, look: DomainLook, silent: boolean): [Status, string] {
  const { t } = useLingui();
  if (look.stale) return ["stopped", t`Not connected`];
  if (look.expired) return ["down", t`Expired`];
  if (look.state === "failed") return ["down", t`Failed`];
  if (look.orphaned) return ["stopped", t`No server`];
  if (silent) return ["stopped", t`No report`];
  if (look.noCertificate) {
    return plainForAnyone(domain) ? ["degraded", t`Plain HTTP`] : ["healthy", t`TLS ends in front`];
  }
  if (look.warning) return ["degraded", t`Renewal failing`];
  if (look.state === "serving") {
    const { days } = look;
    if (days === undefined) return ["healthy", t`Valid`];
    return ["healthy", plural(days, { one: "Valid for # day", other: "Valid for # days" })];
  }
  return ["deploying", asksNoCertificate(domain) ? t`Waiting for the server` : t`Issuing`];
}

/** A domain's shape and word, or that nobody here can say, as what's read past its time says nothing. */
export function useDomainState(domain: Domain, look: DomainLook, silent: boolean, read: DomainRead): [Status, string] {
  const [status, word] = useDomainWord(domain, look, silent);
  const words = useHealthWords();
  return read === "read" ? [status, word] : ["unknown", words.word({ kind: "unknown", read })];
}

/** A state's shape, word and colour, as every state has in the brand book, and the port behind it. */
export function DomainFacts({
  domain,
  route,
  look,
  silent,
  read = "read",
}: {
  domain: Domain;
  route: DomainRoute | undefined;
  look: DomainLook;
  silent: boolean;
  read?: DomainRead;
}) {
  const [status, word] = useDomainState(domain, look, silent, read);
  const port = route?.hostPort || route?.port;
  const { servers } = useFleetView();
  // With one server, there's only one it can be.
  const held = (servers?.length ?? 0) > 1 ? domain.servers.map((server) => server.serverName).join(", ") : "";
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
      {held ? (
        <span className="domain-held">
          <Trans>
            On <code>{held}</code>
          </Trans>
        </span>
      ) : null}
    </span>
  );
}

/** A server's own page, from a row that's about what it has or hasn't said. */
function ServerPage({
  server,
  onLeave,
  children,
}: {
  server: { id: string };
  onLeave: (() => void) | undefined;
  children: ReactNode;
}) {
  return (
    <Link
      className="domain-note-link"
      to="/servers/$serverId"
      params={{ serverId: server.id }}
      {...(onLeave ? { onClick: onLeave } : {})}
    >
      {children}
    </Link>
  );
}

/** A certificate that has run out: where, when, what follows, and what to do. */
function ExpiredNote({
  expired,
  onLeave,
}: {
  expired: NonNullable<DomainLook["expired"]>;
  onLeave: (() => void) | undefined;
}) {
  const time = useTimeWords();
  const now = useControlPlaneNow(30_000);
  const server = expired.name;
  const when = time.ago(new Date(expired.at), now);
  return (
    <Trans>
      The certificate on <code>{server}</code> ran out {when}, so browsers refuse this domain. Dockwell renews it by
      itself. If it stays like this,{" "}
      <ServerPage server={expired} onLeave={onLeave}>
        check its page
      </ServerPage>
      .
    </Trans>
  );
}

/** A domain whose server isn't connected: which server, what it last said, and when. */
function StaleNote({
  domain,
  stale,
  said,
  onLeave,
}: {
  domain: Domain;
  stale: { id: string; name: string };
  said: DomainLook;
  onLeave: (() => void) | undefined;
}) {
  const time = useTimeWords();
  const now = useControlPlaneNow(30_000);
  const { servers } = useFleetView();
  const [, word] = useDomainWord(domain, said, false);
  const server = stale.name;
  const seen = servers?.find((s) => s.id === stale.id)?.lastSeen;
  const when = seen ? time.ago(new Date(seen), now) : "";
  // Its words may end a sentence, and these go on.
  const last = (said.message || word).replace(/\.$/, "");
  if (said.unreported || said.orphaned) {
    return (
      <Trans>
        <code>{server}</code> isn't connected, and hasn't said anything of this yet.{" "}
        <ServerPage server={stale} onLeave={onLeave}>
          Check its page
        </ServerPage>
      </Trans>
    );
  }
  return when ? (
    <Trans>
      <code>{server}</code> isn't connected, so this is what it last said, {when}: {last}.{" "}
      <ServerPage server={stale} onLeave={onLeave}>
        Check its page
      </ServerPage>
    </Trans>
  ) : (
    <Trans>
      <code>{server}</code> isn't connected, so this is what it last said: {last}.{" "}
      <ServerPage server={stale} onLeave={onLeave}>
        Check its page
      </ServerPage>
    </Trans>
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
  if (look.stale && look.said) {
    return <StaleNote domain={domain} stale={look.stale} said={look.said} onLeave={onLeave} />;
  }
  if (look.expired) {
    return (
      <>
        <ExpiredNote expired={look.expired} onLeave={onLeave} />
        {look.message ? ` ${panelWords(look.message, switchName)}` : null}
      </>
    );
  }
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
        <ServerPage server={look.unreported} onLeave={onLeave}>
          Check its page
        </ServerPage>
      </Trans>
    );
  }
  return null;
}
