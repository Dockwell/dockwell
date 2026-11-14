// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate } from "@bufbuild/protobuf/wkt";
import { type BackupDestinationInfo, ChangeService, type DnsProviderInfo, type RegistryLoginInfo } from "@dockwell/api";
import { Button, Icon, type IconName, PHONE_QUERY, StatusBadge, useMediaQuery } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link } from "@tanstack/react-router";
import { type CSSProperties, type ReactNode, useState } from "react";

import { formatBytes } from "../../lib/format";
import { useTimeWords } from "../../lib/time";
import { DestinationSheet } from "../backups/DestinationSheet";
import { useDestinations } from "../backups/DestinationsPanel";
import { DnsProviderSheet } from "../dns/DnsProviderSheet";
import { useDnsProviders } from "../dns/dns";
import { RegistryLoginSheet } from "../registries/RegistryLoginSheet";
import { REGISTRY_NAMES, registryKind, useRegistryLogins } from "../registries/registries";
import { useControlPlaneNow } from "../servers/useFleet";
import { Page } from "../shell/Page";
import "../backups/Backups.css";
import "./Settings.css";
import { useMay } from "../auth/may";
import { ReadFailed } from "../shell/ReadFailed";

const CLOUDFLARE =
  "https://github.com/Dockwell/dockwell/blob/main/docs/install.md#behind-cloudflare-tunnel-or-another-proxy";

/**
 * Integrations, as Settings-Integrations.dc.html has them, for what Dockwell
 * connects to today: storage backups go to, which a person adds here as a plan,
 * and Cloudflare in front of routes. What comes later is a line, never drawn as
 * a card to connect.
 */
export function IntegrationsPage() {
  const { t } = useLingui();
  const phone = useMediaQuery(PHONE_QUERY);
  const destinations = useDestinations();
  const [adding, setAdding] = useState(false);
  const logins = useRegistryLogins();
  // A registry login to add, from a card's registry, or to change.
  const [login, setLogin] = useState<{ host?: string; editing?: RegistryLoginInfo }>();
  const loginList = logins.data?.logins ?? [];
  const dns = useDnsProviders();
  const dnsList = dns.data?.providers ?? [];
  // A DNS provider to connect, or to change (ADR 0088).
  const [provider, setProvider] = useState<{ editing?: DnsProviderInfo }>();
  // A destination belongs to no project, so connecting one is an admin's: off, with why, otherwise (ADR 0059).
  const mayPlan = useMay()(ChangeService.method.proposeChange, { install: true });
  const read = destinations.data !== undefined;
  const list = destinations.data?.destinations ?? [];
  const connected = list.length;
  // Counted once the list is read, never as none while it loads or can't be read.
  const lead = read
    ? connected === 1
      ? t`1 connected · the services Dockwell sends backups to and routes behind`
      : t`${connected} connected · the services Dockwell sends backups to and routes behind`
    : undefined;
  const variables = (
    <p className="settings-meta integrations-aside">
      <Trans>
        Values your apps use live in{" "}
        <Link to="/variables" className="cp-link">
          Variables and secrets
        </Link>
      </Trans>
    </p>
  );
  const connect = (
    <Button size="sm" icon="plus" onClick={() => setAdding(true)} {...mayPlan.off}>
      <Trans>Connect</Trans>
    </Button>
  );
  // How many cards each section holds, so the sections sit side by side, each as wide as its cards, as
  // Settings-Integrations lays them out, rather than a row each with most of it empty.
  const cards = (n: number) => ({ "--cards": Math.max(1, n) }) as CSSProperties;
  const storageCards = read ? list.length + (connected === 0 ? 1 : 0) : 1;
  // While a list loads, as many as it shows once it arrives empty, so a section doesn't change rows on every visit.
  const registryCards = logins.data
    ? loginList.length +
      (loginList.some((l) => l.registry === "docker.io") ? 0 : 1) +
      (loginList.some((l) => l.registry === "ghcr.io") ? 0 : 1)
    : 2;
  const dnsCards = dns.data || dns.error ? dnsList.length + (dnsList.length > 0 ? 0 : 1) + 1 : 2;
  return (
    <Page width="narrow" title={t`Integrations`} lead={lead} phoneSub={lead} back="/settings" actions={variables}>
      <div className="integrations-sections">
        <section className="integrations-section" aria-labelledby="integrations-storage" style={cards(storageCards)}>
          <div className="cp-section-head">
            <h2 id="integrations-storage" className="dw-text-heading">
              <Trans>Object storage</Trans>
            </h2>
            {read ? (
              <span className="settings-meta">
                {connected === 1
                  ? t`1 bucket · where backups go beside each server's disk`
                  : t`${connected} buckets · where backups go beside each server's disk`}
              </span>
            ) : null}
            {/* Once storage is connected, more is connected from here rather than from a card of its own. */}
            {connected > 0 ? (
              <Button
                size="sm"
                variant="ghost"
                icon="plus"
                className="integrations-more"
                onClick={() => setAdding(true)}
                {...mayPlan.off}
              >
                <Trans>Connect storage</Trans>
              </Button>
            ) : null}
          </div>
          {destinations.error && !destinations.data ? (
            <ReadFailed
              error={destinations.error}
              title={t`Dockwell can't list where backups go`}
              actions={
                <Button
                  size="sm"
                  icon="refresh"
                  loading={destinations.isFetching}
                  onClick={() => void destinations.refetch()}
                >
                  <Trans>Try again</Trans>
                </Button>
              }
            />
          ) : !read ? (
            <div className="integrations-grid" aria-busy="true">
              <div className="dw-panel integrations-card integrations-skeleton" />
              <span className="dw-sr">{t`Loading where backups go`}</span>
            </div>
          ) : (
            <div className="integrations-grid">
              {list.map((d) => (
                <DestinationCard key={d.id} destination={d} />
              ))}
              {connected === 0 ? (
                <Card
                  icon="archive"
                  name={t`S3-compatible storage`}
                  state={<span className="settings-meta">{t`Not connected`}</span>}
                  text={t`S3, R2, B2, MinIO or any S3 API. Backups are sealed before they leave.`}
                  action={connect}
                />
              ) : null}
            </div>
          )}
        </section>

        <section
          className="integrations-section"
          aria-labelledby="integrations-registries"
          style={cards(registryCards)}
        >
          <div className="cp-section-head">
            <h2 id="integrations-registries" className="dw-text-heading">
              <Trans>Registries</Trans>
            </h2>
            {logins.data ? (
              <span className="settings-meta">
                {loginList.length === 1
                  ? t`1 login · for images a registry keeps private`
                  : t`${loginList.length} logins · for images a registry keeps private`}
              </span>
            ) : null}
            <Button
              size="sm"
              variant="ghost"
              icon="plus"
              className="integrations-more"
              onClick={() => setLogin({})}
              {...mayPlan.off}
            >
              <Trans>Connect a registry</Trans>
            </Button>
          </div>
          {logins.error && !logins.data ? (
            <ReadFailed
              error={logins.error}
              title={t`Dockwell can't list its registry logins`}
              actions={
                <Button size="sm" icon="refresh" loading={logins.isFetching} onClick={() => void logins.refetch()}>
                  <Trans>Try again</Trans>
                </Button>
              }
            />
          ) : !logins.data ? (
            <div className="integrations-grid" aria-busy="true">
              <div className="dw-panel integrations-card integrations-skeleton" />
              <span className="dw-sr">{t`Loading registry logins`}</span>
            </div>
          ) : (
            <div className="integrations-grid">
              {loginList.map((l) => (
                <LoginCard key={l.id} login={l} onManage={() => setLogin({ editing: l })} manage={mayPlan.off} />
              ))}
              {/* Docker Hub and GitHub's, the registries most images come from, until one is connected. */}
              {loginList.some((l) => l.registry === "docker.io") ? null : (
                <Card
                  icon="layers"
                  name="Docker Hub"
                  state={<span className="settings-meta">{t`Not connected`}</span>}
                  text={t`Private images, and pulls past Docker Hub's limits.`}
                  action={
                    <Button size="sm" icon="plus" onClick={() => setLogin({ host: "docker.io" })} {...mayPlan.off}>
                      <Trans>Connect</Trans>
                    </Button>
                  }
                />
              )}
              {loginList.some((l) => l.registry === "ghcr.io") ? null : (
                <Card
                  icon="layers"
                  name="GitHub Container Registry"
                  state={<span className="settings-meta">{t`Not connected`}</span>}
                  text={t`Private images from GitHub, such as those Actions push.`}
                  action={
                    <Button size="sm" icon="plus" onClick={() => setLogin({ host: "ghcr.io" })} {...mayPlan.off}>
                      <Trans>Connect</Trans>
                    </Button>
                  }
                />
              )}
            </div>
          )}
        </section>

        <section className="integrations-section" aria-labelledby="integrations-dns" style={cards(dnsCards)}>
          <div className="cp-section-head">
            <h2 id="integrations-dns" className="dw-text-heading">
              <Trans>DNS and proxies</Trans>
            </h2>
            <span className="settings-meta">{t`in front of a server's routes`}</span>
          </div>
          <div className="integrations-grid">
            {dnsList.map((p) => (
              <ProviderCard key={p.id} provider={p} onManage={() => setProvider({ editing: p })} manage={mayPlan.off} />
            ))}
            {/* Not connected only once the list says so, never while it loads. */}
            {dnsList.length > 0 || (!dns.data && !dns.error) ? null : (
              <Card
                icon="globe"
                name="Cloudflare"
                state={<span className="settings-meta">{t`Not connected`}</span>}
                text={t`Certificates by DNS, behind NAT or for a wildcard. A route can also sit behind its proxy or a Tunnel.`}
                action={
                  <div className="integrations-actions">
                    <Button size="sm" icon="plus" onClick={() => setProvider({})} {...mayPlan.off}>
                      <Trans>Connect</Trans>
                    </Button>
                    <a href={CLOUDFLARE} target="_blank" rel="noreferrer" className="dw-btn dw-btn-ghost dw-btn-sm">
                      <Icon name="external" size={14} />
                      <span>
                        <Trans>How to route behind it</Trans>
                      </span>
                    </a>
                  </div>
                }
              />
            )}
            <Card
              icon="link"
              name={t`Domains`}
              state={<span className="settings-meta">{t`Any DNS provider`}</span>}
              text={t`Dockwell says which record to make, then gets the certificate.`}
              action={
                <Link to="/domains" className="dw-btn dw-btn-secondary dw-btn-sm">
                  <span>
                    <Trans>Open Domains</Trans>
                  </span>
                </Link>
              }
            />
          </div>
        </section>
      </div>

      <p className="settings-meta integrations-later">{t`Git providers and cloud providers come later.`}</p>

      {/* A phone's header has no room for the aside, so it closes the page. */}
      {phone ? variables : null}
      {adding ? <DestinationSheet existing={list} onClose={() => setAdding(false)} /> : null}
      {provider ? (
        <DnsProviderSheet existing={dnsList} editing={provider.editing} onClose={() => setProvider(undefined)} />
      ) : null}
      {login ? (
        <RegistryLoginSheet
          existing={loginList}
          editing={login.editing}
          host={login.host}
          onClose={() => setLogin(undefined)}
        />
      ) : null}
    </Page>
  );
}

interface CardProps {
  icon: IconName;
  name: string;
  state: ReactNode;
  text: ReactNode;
  action: ReactNode;
}

/** One integration, as the mock draws each: its mark and state, its name, what it is, and what to do. */
function Card({ icon, name, state, text, action }: CardProps) {
  return (
    <article className="dw-panel integrations-card" aria-label={name}>
      <div className="integrations-card-head">
        <span className="integrations-mark" aria-hidden="true">
          <Icon name={icon} size={16} />
        </span>
        {state}
      </div>
      <div className="integrations-card-text">
        <h3 className="integrations-name">{name}</h3>
        <p className="integrations-desc">{text}</p>
      </div>
      <div>{action}</div>
    </article>
  );
}

/** Storage backups go to, and how it's doing by the last backup that got there. */
function DestinationCard({ destination: d }: { destination: BackupDestinationInfo }) {
  const { t, i18n } = useLingui();
  const words = useTimeWords();
  const now = useControlPlaneNow(60_000);
  let host = d.endpoint;
  try {
    host = new URL(d.endpoint).host;
  } catch {
    // Shown as it was given.
  }
  // How it's doing is how its newest backup went: one that got there is a check that
  // passed, one that didn't is a check that failed, and until either, nobody can say.
  const uploaded = d.lastUpload ? timestampDate(d.lastUpload) : undefined;
  const failed = d.lastFailure ? timestampDate(d.lastFailure) : undefined;
  const failing = failed !== undefined && (uploaded === undefined || failed > uploaded);
  const when = failed ? words.ago(failed, now) : "";
  const state = failing ? (
    <StatusBadge status="down" label={t`A backup didn't get here ${when}`} />
  ) : uploaded ? (
    <StatusBadge status="healthy" label={t`Received a backup ${words.ago(uploaded, now)}`} />
  ) : (
    <StatusBadge status="unknown" label={t`No backup yet`} />
  );
  return (
    <Card
      icon="archive"
      name={d.name}
      state={state}
      text={
        <>
          <Trans>
            Bucket <code>{d.bucket}</code> at <code>{host}</code>
          </Trans>
          <span className="integrations-sub">
            {d.backups === 1
              ? t`1 backup, ${formatBytes(d.usedBytes, i18n.locale)}`
              : t`${d.backups} backups, ${formatBytes(d.usedBytes, i18n.locale)}`}
          </span>
          {failing ? (
            <span className="integrations-error" role="alert">
              {d.lastFailureError}
            </span>
          ) : null}
        </>
      }
      action={
        <Link to="/backups" className="dw-btn dw-btn-secondary dw-btn-sm">
          <Icon name="settings" size={14} />
          <span>
            <Trans>Manage in Backups</Trans>
          </span>
        </Link>
      }
    />
  );
}

/** A registry login: its registry, its name, and which projects may pull with it. */
function LoginCard({
  login: l,
  onManage,
  manage,
}: {
  login: RegistryLoginInfo;
  onManage: () => void;
  manage: { off?: string; disabled?: true };
}) {
  const { t } = useLingui();
  const kind = registryKind(l.registry);
  const who =
    l.projects.length === 0
      ? t`every project`
      : l.projects.length === 1
        ? t`1 project`
        : t`${l.projects.length} projects`;
  return (
    <Card
      icon="layers"
      name={REGISTRY_NAMES[kind] ?? l.registry}
      state={<StatusBadge status="healthy" label={t`Connected`} />}
      text={
        <>
          <Trans>
            <code>{l.name}</code> for {who}
          </Trans>
          <span className="integrations-sub">
            <code>{l.registry}</code>
          </span>
        </>
      }
      action={
        <Button size="sm" variant="secondary" icon="settings" onClick={onManage} {...manage}>
          <Trans>Manage</Trans>
        </Button>
      }
    />
  );
}

/** A DNS provider an admin connected, with the zones it writes in. */
function ProviderCard({
  provider: p,
  onManage,
  manage,
}: {
  provider: DnsProviderInfo;
  onManage: () => void;
  manage: { off?: string; disabled?: true };
}) {
  const { t } = useLingui();
  return (
    <Card
      icon="globe"
      name="Cloudflare"
      state={<StatusBadge status="healthy" label={t`Connected`} />}
      text={
        <>
          <Trans>
            <code>{p.name}</code> proves names by DNS
          </Trans>
          <span className="integrations-sub">
            <code>{p.zones.join(", ")}</code>
          </span>
        </>
      }
      action={
        <Button size="sm" variant="secondary" icon="settings" onClick={onManage} {...manage}>
          <Trans>Manage</Trans>
        </Button>
      }
    />
  );
}
