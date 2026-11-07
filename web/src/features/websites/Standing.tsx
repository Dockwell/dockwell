// SPDX-License-Identifier: AGPL-3.0-only

import type { Domain, ServiceInfo, WebsiteInfo } from "@dockwell/api";
import { cx, STATUS, type Status, StatusBadge } from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { type ReactNode, useMemo } from "react";

import { useDomainState, useSilent } from "../apps/DomainNote";
import { type DomainLook, type DomainRead, lookOf, plainForAnyone, readOf, useAllDomains } from "../apps/domains";
import { useHealthWords } from "../apps/Health";
import { type AppHealth, type HealthView, useHealth } from "../apps/health";
import { useServices } from "../apps/useServices";
import { useControlPlaneNow } from "../servers/useFleet";
import { useWebsites } from "./website";

/** How often what a certificate has left is looked at again. */
const NOW_EVERY_MS = 60_000;

/** A website, with what's read of how it serves. */
export interface Row {
  id: string;
  site: WebsiteInfo;
  domain: Domain | undefined;
  look: DomainLook | undefined;
  service: ServiceInfo | undefined;
}

/**
 * Every website, with what's read of how each serves: its domain's state, and its web
 * server, whose health the rows read. The list and a website's page share it.
 */
export function useWebsiteRows() {
  const websites = useWebsites();
  const domains = useAllDomains();
  const services = useServices();
  const health = useHealth();
  const now = useControlPlaneNow(NOW_EVERY_MS);
  const rows = useMemo<Row[]>(() => {
    const byName = new Map((domains.last?.domains ?? []).map((d) => [d.name, d]));
    return (websites.data ?? []).map((site) => {
      const domain = byName.get(site.domain);
      return {
        id: site.id,
        site,
        domain,
        look: domain ? lookOf(domain, now) : undefined,
        service: services.data?.find((s) => s.id === site.serviceId),
      };
    });
  }, [websites.data, domains.last, services.data, now]);
  return { websites, rows, read: readOf(domains), health };
}

/** How a website stands, as a shape and a word, and what more there is to say of it. */
export interface Standing {
  status: Status;
  word: string;
  detail?: string;
}

/**
 * Whether what a website sends to answers, as its server last found, when it doesn't:
 * nothing listening on its port, or a URL that can't be reached or answers 5xx.
 */
function useUnreachable(site: WebsiteInfo): Standing | undefined {
  const { t } = useLingui();
  const reach = site.reach;
  if (!reach || reach.answered) return undefined;
  const proxy = site.spec?.serves.case === "proxy" ? site.spec.serves.value : undefined;
  if (proxy?.port) return { status: "down", word: t`Nothing listens on port ${proxy.port}`, detail: reach.said };
  let host = proxy?.url ?? "";
  try {
    host = new URL(host).host;
  } catch {
    // It's said whole, then.
  }
  const code = /^(5\d\d)\b/.exec(reach.said)?.[1];
  if (code) return { status: "down", word: t`${host} answers ${code}`, detail: reach.said };
  return { status: "down", word: t`Can't reach ${host}`, detail: reach.said };
}

/**
 * How a website serves, as a shape and a word: whether what it sends to answers, when
 * its server says it doesn't; else its web server's health where it has one, and its
 * domain's state where it hasn't. "Healthy" says what it sends to answered: until its
 * server has said so, a web server that runs is "Running", and a port is "Not checked".
 */
export function SiteStanding({
  row,
  health,
  read,
  children,
}: {
  row: Row;
  health: HealthView;
  read: DomainRead;
  children: (standing: Standing) => ReactNode;
}) {
  const { t } = useLingui();
  const unreachable = useUnreachable(row.site);
  // A folder or PHP sends nowhere, so what its web server says is all there is to check.
  const checked = !!row.site.reach || row.site.spec?.serves.case !== "proxy";
  if (row.service || row.site.serviceId) {
    return (
      <ServiceStanding
        health={health.of(row.service ?? { id: row.site.serviceId })}
        unreachable={unreachable}
        checked={checked}
      >
        {children}
      </ServiceStanding>
    );
  }
  if (!row.domain || !row.look) return children({ status: "unknown", word: t`No domain` });
  return (
    <DomainStanding domain={row.domain} look={row.look} read={read} unreachable={unreachable} checked={checked}>
      {children}
    </DomainStanding>
  );
}

function ServiceStanding({
  health,
  unreachable,
  checked,
  children,
}: {
  health: AppHealth;
  unreachable: Standing | undefined;
  checked: boolean;
  children: (standing: Standing) => ReactNode;
}) {
  const { t } = useLingui();
  const words = useHealthWords();
  // Its web server's own trouble comes first: then nothing is reached at all.
  if (health.status !== "healthy") return children({ status: health.status, word: words.word(health) });
  if (unreachable) return children(unreachable);
  return children({ status: "healthy", word: checked ? t`Healthy` : t`Running` });
}

function DomainStanding({
  domain,
  look,
  read,
  unreachable,
  checked,
  children,
}: {
  domain: Domain;
  look: DomainLook;
  read: DomainRead;
  unreachable: Standing | undefined;
  checked: boolean;
  children: (standing: Standing) => ReactNode;
}) {
  const { t } = useLingui();
  const silent = useSilent(domain, look);
  const [status] = useDomainState(domain, look, silent, read);
  const word = useServingWord(status);
  if (status !== "healthy") return children({ status, word });
  if (unreachable) return children(unreachable);
  return children(checked ? { status, word } : { status: "unknown", word: t`Not checked` });
}

export function SiteStatus({ row, health, read }: { row: Row; health: HealthView; read: DomainRead }) {
  return (
    <SiteStanding row={row} health={health} read={read}>
      {({ status, word, detail }) => (
        <StatusBadge status={status} label={word} labelIsState title={detail ? `${word}: ${detail}` : word} />
      )}
    </SiteStanding>
  );
}

/** Whether a site that has nothing but its domain serves, in the list's words for how things run. */
export function useServingWord(status: Status): string {
  const { t } = useLingui();
  switch (status) {
    case "healthy":
      return t`Healthy`;
    case "deploying":
      return t`Starting`;
    case "down":
      return t`Not serving`;
    case "degraded":
      return t`Degraded`;
    default:
      return t`Unknown`;
  }
}

/** Where a website's certificate stands: valid and for how long, none as TLS ends in front, or why not. */
export function CertificateCell({ row, read }: { row: Row; read: DomainRead }) {
  if (!row.domain || !row.look) return <span className="websites-muted">—</span>;
  return <Certificate domain={row.domain} look={row.look} read={read} />;
}

export function Certificate({ domain, look, read }: { domain: Domain; look: DomainLook; read: DomainRead }) {
  const silent = useSilent(domain, look);
  const [status, word] = useDomainState(domain, look, silent, read);
  if (read === "read" && look.state === "serving" && !look.noCertificate && !look.warning && !look.stale) {
    const { days } = look;
    return (
      <span className="websites-certificate">
        <Trans>Valid</Trans>
        {days !== undefined ? (
          <span className="websites-muted">
            {" · "}
            {plural(days, { one: "# day left", other: "# days left" })}
          </span>
        ) : null}
      </span>
    );
  }
  return (
    <span
      className={cx("websites-certificate", status !== "healthy" && `dw-tone-${STATUS[status].tone}`)}
      title={look.message || word}
    >
      {word}
    </span>
  );
}

/** Where a website's certificate stands, as a clause of a line, such as "certificate valid for 74 days". */
export function useCertificateWords(): (domain: Domain, look: DomainLook, read: DomainRead) => string {
  const { t } = useLingui();
  return (domain, look, read) => {
    if (read !== "read" || look.stale) return "";
    if (look.expired) return t`certificate expired`;
    if (look.state === "failed") return t`certificate failed`;
    if (look.noCertificate) return plainForAnyone(domain) ? t`plain HTTP` : t`TLS ends in front`;
    if (look.warning) return t`certificate renewal failing`;
    if (look.state === "pending") return t`certificate on its way`;
    const { days } = look;
    if (days === undefined) return t`certificate valid`;
    return plural(days, { one: "certificate valid for # day", other: "certificate valid for # days" });
  };
}
