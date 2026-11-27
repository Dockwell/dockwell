// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate } from "@bufbuild/protobuf/wkt";
import { createClient } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import {
  type BackupDestinationInfo,
  BackupService,
  ChangeService,
  ChangeState,
  type ConnectionInfo,
  type DnsProviderInfo,
  GitProviderService,
  type RegistryLoginInfo,
  type SigninProviderInfo,
} from "@dockwell/api";
import {
  type BrandId,
  BrandMark,
  Button,
  Icon,
  type IconName,
  PHONE_QUERY,
  StatusBadge,
  Switch,
  useMediaQuery,
} from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link } from "@tanstack/react-router";
import { type ReactNode, useMemo, useState } from "react";

import { formatBytes } from "../../lib/format";
import { useTimeWords } from "../../lib/time";
import { StateLine } from "../apps/DomainStep";
import { type StorageItem, useStorage } from "../backups/storage";
import { ConnectionSheet } from "../connections/ConnectionSheet";
import { HostName } from "../connections/HostName";
import { useLastCheck } from "../connections/LastCheck";
import { ConnectionMark, DestinationMark } from "../connections/ProviderMark";
import { appsOf } from "../connections/storage";
import { DnsProviderSheet } from "../dns/DnsProviderSheet";
import { dnsProviderEdit, useDnsProviders } from "../dns/dns";
import { RegistryLoginSheet } from "../registries/RegistryLoginSheet";
import {
  REGISTRY_NAMES,
  type RegistryKind,
  registryKind,
  registryPushEdit,
  useRegistryLogins,
} from "../registries/registries";
import { useControlPlaneNow } from "../servers/useFleet";
import {
  ListNoMatch,
  ListPager,
  type ListRouteProps,
  ListToolbar,
  useListPlace,
  useListSection,
} from "../shell/ListTools";
import { Page } from "../shell/Page";
import { issuerHost, useSigninProviders } from "../signin/providers";
import { SigninProviderSheet } from "../signin/SigninProviderSheet";
import { CardsLoading } from "./CardsLoading";
import { GitHubSection } from "./GitHub";
import { useGitHubApps } from "./github";
import "../backups/Backups.css";
import "./Settings.css";
import { useMay } from "../auth/may";
import { useChangeSheet } from "../changes/ChangeSheet";
import { ReadFailed } from "../shell/ReadFailed";

const CLOUDFLARE =
  "https://github.com/Dockwell/dockwell/blob/main/docs/install.md#behind-cloudflare-tunnel-or-another-proxy";

/**
 * Integrations, as Settings-Integrations.dc.html has them, for what Dockwell
 * connects to today: GitHub, as an App, storage backups go to, which a person adds
 * here as a plan, and Cloudflare in front of routes. What comes later is a line,
 * never drawn as a card to connect.
 */
export function IntegrationsPage({ search, onSearch }: ListRouteProps) {
  const { t } = useLingui();
  const phone = useMediaQuery(PHONE_QUERY);
  const storage = useStorage();
  const [adding, setAdding] = useState(false);
  const logins = useRegistryLogins();
  // A registry login to add, from a card's registry, or to change.
  const [login, setLogin] = useState<{ host?: string; editing?: RegistryLoginInfo }>();
  const loginList = logins.data?.logins ?? [];
  const loginCount = loginList.length;
  const signins = useSigninProviders();
  // A sign-in provider to add, or to change.
  const [signin, setSignin] = useState<{ editing?: SigninProviderInfo }>();
  const signinList = signins.data?.providers ?? [];
  const signinCount = signinList.length;
  const dns = useDnsProviders();
  const dnsList = dns.data?.providers ?? [];
  // A DNS provider to connect, or to change (ADR 0088).
  const [provider, setProvider] = useState<{ editing?: DnsProviderInfo }>();
  // Storage belongs to no project, so connecting one is an admin's: off, with why, otherwise (ADR 0059).
  const may = useMay();
  const mayPlan = may(ChangeService.method.proposeChange, { install: true });
  const mayUseAsConnection = may(BackupService.method.useDestinationAsConnection);
  const read = storage.ready;
  const list = storage.items.filter((item) => !item.destination?.sftp);
  const place = useListPlace(search, onSearch, []);
  const storagePages = useListSection(
    list,
    place.query,
    (item) => `${item.name} ${item.label} ${item.host} ${item.bucket}`,
  );
  const registryPages = useListSection(loginList, place.query, (item) => `${item.name} ${item.registry}`);
  const dnsPages = useListSection(
    dnsList,
    place.query,
    (item) => `${item.name} ${item.provider} ${item.zones.join(" ")}`,
  );
  const signinPages = useListSection(
    signinList,
    place.query,
    (item) => `${item.name} ${item.displayName} ${item.issuer}`,
  );
  const connected = list.length;
  // Every integration connected, as the page lists them: GitHub, storage, registry
  // logins, DNS providers and sign-in providers. A list the caller may not read
  // counts none of its own.
  const mayListApps = may(GitProviderService.method.listGitProviders).allowed;
  const apps = useGitHubApps();
  const appCount = apps.data
    ? apps.data.apps.filter((a) => !a.gone).length + apps.data.gitlabConnections.length
    : mayListApps
      ? undefined
      : 0;
  const counts = [
    read ? connected : undefined,
    logins.data ? loginList.length : undefined,
    dns.data ? dnsList.length : undefined,
    signins.data ? signinList.length : undefined,
    appCount,
  ];
  // Counted once every list is read, never as fewer while one loads or can't be read.
  const all = counts.every((n) => n !== undefined) ? counts.reduce<number>((sum, n) => sum + (n ?? 0), 0) : undefined;
  const lead =
    all === undefined
      ? undefined
      : all === 1
        ? t`1 connected · the services Dockwell builds from, deploys with, routes through and backs up to`
        : t`${all} connected · the services Dockwell builds from, deploys with, routes through and backs up to`;
  const variables = (
    <p className="settings-meta integrations-aside">
      <Trans>
        Values your apps use live in{" "}
        <Link to="/variables" className="cp-link">
          Variables
        </Link>
      </Trans>
    </p>
  );
  const connect = (
    <Button size="sm" icon="plus" onClick={() => setAdding(true)} {...mayPlan.off}>
      <Trans>Connect</Trans>
    </Button>
  );
  // How many of the page's columns each section takes: one a card, up to four, so every section's cards sit on
  // the same columns, as Settings-Integrations lays them out (#786).
  const cards = (n: number) => ({ "data-cards": String(Math.min(4, Math.max(1, n))) });
  const storageCards = read ? list.length + (connected === 0 ? 1 : 0) : 1;
  // While a list loads, as many as it shows once it arrives empty, so a section doesn't change rows on every visit.
  const registryCards = logins.data
    ? loginList.length +
      (loginList.some((l) => l.registry === "docker.io") ? 0 : 1) +
      (loginList.some((l) => l.registry === "ghcr.io") ? 0 : 1)
    : 2;
  const dnsCards = dns.data || dns.error ? dnsList.length + (dnsList.length > 0 ? 0 : 1) + 1 : 2;
  const signinCards = Math.max(1, signinList.length);
  const zoneCount = dnsList.reduce((n, p) => n + p.zones.length, 0);
  return (
    <Page width="narrow" title={t`Integrations`} lead={lead} phoneSub={lead} back="/settings" actions={variables}>
      {all || place.query ? <ListToolbar place={place} label={t`Filter integrations`} /> : null}
      <div className="integrations-sections">
        <div className="integrations-board">
          <GitHubSection query={place.query} />

          <section className="integrations-section" aria-labelledby="integrations-storage" {...cards(storageCards)}>
            <div className="integrations-head">
              <h2 id="integrations-storage" className="dw-text-heading">
                <Trans>Object storage</Trans>
              </h2>
              {read && connected > 0 ? (
                <span className="integrations-count">{connected === 1 ? t`1 bucket` : t`${connected} buckets`}</span>
              ) : null}
              {/* Once storage is connected, more is connected from here rather than from a card of its own. */}
              {connected > 0 ? (
                <Button
                  size="sm"
                  variant="ghost"
                  icon="plus"
                  title={t`Connect storage`}
                  className="integrations-more"
                  onClick={() => setAdding(true)}
                  {...mayPlan.off}
                >
                  <Trans>Connect storage</Trans>
                </Button>
              ) : null}
            </div>
            {storage.error && !read ? (
              <ReadFailed
                error={storage.error}
                title={t`Dockwell can't list where backups go`}
                actions={
                  <Button size="sm" icon="refresh" loading={storage.isFetching} onClick={() => void storage.refetch()}>
                    <Trans>Try again</Trans>
                  </Button>
                }
              />
            ) : !read ? (
              <CardsLoading label={t`Loading where backups go`} />
            ) : (
              <div className="integrations-grid">
                {storagePages.page.rows.map((item) => (
                  <StorageCard key={item.key} item={item} useAsConnectionOff={mayUseAsConnection.off} />
                ))}
                {connected === 0 ? (
                  <Card
                    icon="archive"
                    name={t`S3-compatible storage`}
                    state={<span className="settings-meta">{t`Not connected`}</span>}
                    text={t`S3, R2, B2, MinIO or any S3 API`}
                    action={connect}
                  />
                ) : null}
              </div>
            )}
            {place.query && list.length > 0 && !storagePages.page.total ? (
              <ListNoMatch clear={place.clearQuery} />
            ) : null}
            <ListPager {...storagePages} />
          </section>

          <section className="integrations-section" aria-labelledby="integrations-registries" {...cards(registryCards)}>
            <div className="integrations-head">
              <h2 id="integrations-registries" className="dw-text-heading">
                <Trans>Registries</Trans>
              </h2>
              {logins.data && loginList.length > 0 ? (
                <span className="integrations-count">{loginCount === 1 ? t`1 login` : t`${loginCount} logins`}</span>
              ) : null}
              <Button
                size="sm"
                variant="ghost"
                icon="plus"
                title={t`Connect a registry`}
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
              <CardsLoading label={t`Loading registry logins`} />
            ) : (
              <div className="integrations-grid">
                {registryPages.page.rows.map((l) => (
                  <LoginCard
                    key={l.id}
                    onChanged={() => void logins.refetch()}
                    login={l}
                    onManage={() => setLogin({ editing: l })}
                    manage={mayPlan.off}
                  />
                ))}
                {/* Docker Hub and GitHub's, the registries most images come from, until one is connected. */}
                {loginList.some((l) => l.registry === "docker.io") ? null : (
                  <Card
                    icon="layers"
                    brand="docker"
                    name="Docker Hub"
                    state={<span className="settings-meta">{t`Not connected`}</span>}
                    text={t`Private images, past its pull limits`}
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
                    brand="github"
                    name="GitHub Container Registry"
                    state={<span className="settings-meta">{t`Not connected`}</span>}
                    text={t`Private images, such as Actions push`}
                    action={
                      <Button size="sm" icon="plus" onClick={() => setLogin({ host: "ghcr.io" })} {...mayPlan.off}>
                        <Trans>Connect</Trans>
                      </Button>
                    }
                  />
                )}
              </div>
            )}
            {place.query && loginList.length > 0 && !registryPages.page.total ? (
              <ListNoMatch clear={place.clearQuery} />
            ) : null}
            <ListPager {...registryPages} />
          </section>

          <section className="integrations-section" aria-labelledby="integrations-dns" {...cards(dnsCards)}>
            <div className="integrations-head">
              <h2 id="integrations-dns" className="dw-text-heading">
                <Trans>DNS and proxies</Trans>
              </h2>
              {zoneCount > 0 ? (
                <span className="integrations-count">{zoneCount === 1 ? t`1 zone` : t`${zoneCount} zones`}</span>
              ) : null}
            </div>
            <div className="integrations-grid">
              {dnsPages.page.rows.map((p) => (
                <ProviderCard
                  key={p.id}
                  provider={p}
                  onManage={() => setProvider({ editing: p })}
                  manage={mayPlan.off}
                />
              ))}
              {/* Not connected only once the list says so, never while it loads. */}
              {dnsList.length > 0 || (!dns.data && !dns.error) ? null : (
                <Card
                  icon="globe"
                  brand="cloudflare"
                  name="Cloudflare"
                  state={<span className="settings-meta">{t`Not connected`}</span>}
                  text={t`Certificates by DNS, its proxy and tunnels`}
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
                text={t`Says which record to make, then gets the certificate`}
                action={
                  <Link to="/domains" className="dw-btn dw-btn-secondary dw-btn-sm">
                    <span>
                      <Trans>Open Domains</Trans>
                    </span>
                  </Link>
                }
              />
            </div>
            {place.query && dnsList.length > 0 && !dnsPages.page.total ? (
              <ListNoMatch clear={place.clearQuery} />
            ) : null}
            <ListPager {...dnsPages} />
          </section>

          <section className="integrations-section" aria-labelledby="integrations-signin" {...cards(signinCards)}>
            <div className="integrations-head">
              <h2 id="integrations-signin" className="dw-text-heading">
                <Trans>Sign-in</Trans>
              </h2>
              {signins.data && signinCount > 0 ? (
                <span className="integrations-count">
                  {signinCount === 1 ? t`1 provider` : t`${signinCount} providers`}
                </span>
              ) : null}
              {signinList.length > 0 ? (
                <Button
                  size="sm"
                  variant="ghost"
                  icon="plus"
                  title={t`Add a provider`}
                  className="integrations-more"
                  onClick={() => setSignin({})}
                  {...mayPlan.off}
                >
                  <Trans>Add a provider</Trans>
                </Button>
              ) : null}
            </div>
            {signins.error && !signins.data ? (
              <ReadFailed
                error={signins.error}
                title={t`Dockwell can't list its sign-in providers`}
                actions={
                  <Button size="sm" icon="refresh" loading={signins.isFetching} onClick={() => void signins.refetch()}>
                    <Trans>Try again</Trans>
                  </Button>
                }
              />
            ) : !signins.data ? (
              <CardsLoading label={t`Loading sign-in providers`} />
            ) : (
              <div className="integrations-grid">
                {signinPages.page.rows.map((p) => (
                  <SigninCard key={p.id} provider={p} onManage={() => setSignin({ editing: p })} manage={mayPlan.off} />
                ))}
                {signinList.length > 0 ? null : (
                  <Card
                    icon="lock"
                    name={t`Sign-in provider`}
                    state={<span className="settings-meta">{t`Not connected`}</span>}
                    text={t`Private routes, with Authentik, Google or another`}
                    action={
                      <Button size="sm" icon="plus" onClick={() => setSignin({})} {...mayPlan.off}>
                        <Trans>Connect</Trans>
                      </Button>
                    }
                  />
                )}
              </div>
            )}
            {place.query && signinList.length > 0 && !signinPages.page.total ? (
              <ListNoMatch clear={place.clearQuery} />
            ) : null}
            <ListPager {...signinPages} />
          </section>
        </div>
      </div>

      <p className="settings-meta integrations-later">{t`Cloud providers come later.`}</p>

      {/* A phone's header has no room for the aside, so it closes the page. */}
      {phone ? variables : null}
      {adding ? (
        <ConnectionSheet
          kind="storage"
          existing={storage.allConnections}
          onClose={() => setAdding(false)}
          onSaved={() => void storage.refetch()}
        />
      ) : null}
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
      {signin ? (
        <SigninProviderSheet existing={signinList} editing={signin.editing} onClose={() => setSignin(undefined)} />
      ) : null}
    </Page>
  );
}

interface CardProps {
  icon: IconName;
  /** The service's own mark, where it has one, in place of the icon. */
  brand?: BrandId | undefined;
  /** A mark drawn by its caller, such as a connection's provider's. */
  mark?: ReactNode;
  name: string;
  state: ReactNode;
  text: ReactNode;
  action: ReactNode;
}

/** One integration, as the mock draws each: its mark and state, its name, what it is, and what to do. */
function Card({ icon, brand, mark, name, state, text, action }: CardProps) {
  return (
    <article className="dw-panel integrations-card" aria-label={name}>
      <div className="integrations-card-head">
        {mark ?? <BrandMark brand={brand} icon={icon} />}
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

/** One storage bucket on Integrations: an S3 connection (paired with backups or alone) or a legacy destination. */
function StorageCard({
  item,
  useAsConnectionOff,
}: {
  item: StorageItem;
  useAsConnectionOff: { off?: string; disabled?: true };
}) {
  if (item.connection) return <ConnectionStorageCard connection={item.connection} destination={item.destination} />;
  if (item.destination)
    return <DestinationCard destination={item.destination} useAsConnectionOff={useAsConnectionOff} />;
  return null;
}

/** An S3 connection on Integrations, with backup standing where a backup destination uses it. */
function ConnectionStorageCard({
  connection: c,
  destination: d,
}: {
  connection: ConnectionInfo;
  destination?: BackupDestinationInfo;
}) {
  const { t, i18n } = useLingui();
  const words = useTimeWords();
  const now = useControlPlaneNow(60_000);
  const lastCheck = useLastCheck(c);
  const apps = appsOf(c);
  let host = c.endpoint;
  try {
    host = c.endpoint ? new URL(c.endpoint).host : "";
  } catch {
    // Shown as it was given.
  }
  const uploaded = d?.lastUpload ? timestampDate(d.lastUpload) : undefined;
  const failed = d?.lastFailure ? timestampDate(d.lastFailure) : undefined;
  const failing = failed !== undefined && (uploaded === undefined || failed > uploaded);
  const when = failed ? words.ago(failed, now) : "";
  const state = failing ? (
    <StatusBadge status="down" label={t`A backup didn't get here ${when}`} />
  ) : uploaded ? (
    <StatusBadge status="healthy" label={t`Received a backup ${words.ago(uploaded, now)}`} />
  ) : lastCheck ? (
    <StatusBadge status={lastCheck.status} label={lastCheck.label} labelIsState />
  ) : d ? (
    <StatusBadge status="unknown" label={t`No backup yet`} />
  ) : (
    <StatusBadge status="healthy" label={t`Connected`} />
  );
  return (
    <Card
      icon="archive"
      mark={<ConnectionMark connection={c} />}
      name={c.name}
      state={state}
      text={
        <>
          {c.bucket && host ? (
            <Trans>
              Bucket <code>{c.bucket}</code> at <HostName host={host} />
            </Trans>
          ) : (
            c.displayName || c.name
          )}
          <span className="integrations-sub">
            {d
              ? d.backups === 1
                ? t`1 backup, ${formatBytes(d.usedBytes, i18n.locale)}`
                : t`${d.backups} backups, ${formatBytes(d.usedBytes, i18n.locale)}`
              : apps.length > 0
                ? plural(apps.length, { one: "Used by # app", other: "Used by # apps" })
                : t`Ready for apps or backups`}
          </span>
          {failing && d ? (
            <span className="integrations-error" role="alert">
              {d.lastFailureError}
            </span>
          ) : null}
        </>
      }
      action={
        <div className="integrations-actions">
          <Link to="/connections/$name" params={{ name: c.name }} className="dw-btn dw-btn-secondary dw-btn-sm">
            <Icon name="settings" size={14} />
            <span>
              <Trans>Manage</Trans>
            </span>
          </Link>
          {d ? (
            <Link to="/backups" className="dw-btn dw-btn-ghost dw-btn-sm">
              <span>
                <Trans>Backups</Trans>
              </span>
            </Link>
          ) : null}
        </div>
      }
    />
  );
}

/** Legacy standalone storage backups go to, with Use as a connection to move its keys into a connection (#831). */
function DestinationCard({
  destination: d,
  useAsConnectionOff,
}: {
  destination: BackupDestinationInfo;
  useAsConnectionOff: { off?: string; disabled?: true };
}) {
  const { t, i18n } = useLingui();
  const words = useTimeWords();
  const now = useControlPlaneNow(60_000);
  const sheet = useChangeSheet();
  const transport = useTransport();
  const backupClient = useMemo(() => createClient(BackupService, transport), [transport]);
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
  const useAsConnection = () =>
    sheet.open({
      kind: "plan",
      // biome-ignore lint/correctness/useHookAtTopLevel: Connect RPC method name begins with "use", not a React hook
      make: () => backupClient.useDestinationAsConnection({ destination: d.name }),
    });
  return (
    <Card
      icon="archive"
      mark={<DestinationMark destination={d} />}
      name={d.name}
      state={state}
      text={
        <>
          <Trans>
            Bucket <code>{d.bucket}</code> at <HostName host={host} />
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
        <div className="integrations-actions">
          <Link to="/backups" className="dw-btn dw-btn-secondary dw-btn-sm">
            <Icon name="settings" size={14} />
            <span>
              <Trans>Manage in Backups</Trans>
            </span>
          </Link>
          {d.connection === "" ? (
            <Button size="sm" variant="ghost" onClick={useAsConnection} {...useAsConnectionOff}>
              <Trans>Use as a connection</Trans>
            </Button>
          ) : null}
        </div>
      }
    />
  );
}

/** A registry login: its registry, its name, and which projects may pull with it. */
function LoginCard({
  login: l,
  onManage,
  onChanged,
  manage,
}: {
  login: RegistryLoginInfo;
  onManage: () => void;
  onChanged: () => void;
  manage: { off?: string; disabled?: true };
}) {
  const { t } = useLingui();
  const sheet = useChangeSheet();
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
      brand={REGISTRY_BRANDS[kind]}
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
          <Switch
            label={t`Allow pushes`}
            description={t`Use this login to publish reviewed images.`}
            checked={l.allowPush}
            disabled={Boolean(manage.off || manage.disabled)}
            onChange={(allowed) =>
              sheet.open({
                kind: "propose",
                summary: allowed ? t`Allow pushes with ${l.name}` : t`Keep ${l.name} pull-only`,
                edits: [registryPushEdit(l.name, allowed)],
                onClose: (change) => {
                  if (change?.state === ChangeState.APPLIED) onChanged();
                },
              })
            }
          />
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

/** Each registry's own mark, by its kind; another registry has the product's layers icon. */
const REGISTRY_BRANDS: Partial<Record<RegistryKind, BrandId>> = {
  hub: "docker",
  ghcr: "github",
  gitlab: "gitlab",
  ecr: "amazon-ecr",
};

/** How many kept records a provider's card shows before Show more. */
const KEPT_SHOWN = 3;

/** A DNS provider an admin connected, with the zones it writes in, and the records made by hand it keeps a copy of. */
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
  const sheet = useChangeSheet();
  // A few kept records show; the rest are one step away (the review's B-L8).
  const [allKept, setAllKept] = useState(false);
  const [showKept, setShowKept] = useState(false);
  const keptCount = p.kept.length;
  const hidden = p.kept.length - KEPT_SHOWN;
  // Putting a kept record back is the provider's own plan, which sends the rest of it as it is (ADR 0104).
  const putBack = (id: string, name: string) =>
    sheet.open({
      kind: "propose",
      summary: t`Put the record for ${name} back at Cloudflare`,
      edits: [dnsProviderEdit(p, { putBack: [id] })],
    });
  const useEverywhere = () =>
    sheet.open({
      kind: "propose",
      summary: t`Use ${p.name} for every project`,
      edits: [dnsProviderEdit(p, { everyProject: true, projects: [] })],
    });
  return (
    <Card
      icon="globe"
      brand="cloudflare"
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
          {p.provider ? (
            p.everyProject || p.projects.length > 0 ? (
              <span className="integrations-sub">
                {p.everyProject ? t`Serves every project` : t`Serves selected projects`}
              </span>
            ) : (
              <StateLine
                status="unknown"
                action={
                  manage.disabled ? undefined : (
                    <button type="button" className="domain-step-act" onClick={useEverywhere}>
                      <Trans>Use it for every project</Trans>
                    </button>
                  )
                }
              >
                <Trans>Serves no project</Trans>
              </StateLine>
            )
          ) : null}
          {p.kept.length > 0 && !showKept ? (
            // The records it keeps are a line until asked for, so the card is as tall as the rest of its row.
            <StateLine
              status={p.kept.some((k) => k.nameFree) ? "degraded" : "unknown"}
              action={
                <button type="button" className="domain-step-act" onClick={() => setShowKept(true)}>
                  <Trans>Show</Trans>
                </button>
              }
            >
              {keptCount === 1 ? t`1 record kept` : t`${keptCount} records kept`}
            </StateLine>
          ) : null}
          {p.kept.length > 0 && showKept ? (
            <ul className="integrations-kept" aria-label={t`Records kept`}>
              {p.kept.slice(0, allKept ? p.kept.length : KEPT_SHOWN).map((k) => {
                const name = k.name;
                return (
                  <li key={k.id}>
                    <StateLine
                      status={k.nameFree ? "degraded" : "unknown"}
                      action={
                        k.nameFree && !manage.disabled ? (
                          <button
                            type="button"
                            className="domain-step-act"
                            aria-label={t`Put the record for ${name} back`}
                            onClick={() => putBack(k.id, name)}
                          >
                            <Trans>Put it back</Trans>
                          </button>
                        ) : undefined
                      }
                    >
                      {k.nameFree
                        ? t`Kept: ${name}, ${k.record}`
                        : t`Kept: ${name}, ${k.record}. Put back once its name is free`}
                    </StateLine>
                  </li>
                );
              })}
              {p.kept.length > KEPT_SHOWN && !allKept ? (
                <li>
                  <button type="button" className="domain-step-act" onClick={() => setAllKept(true)}>
                    {t`Show ${hidden} more`}
                  </button>
                </li>
              ) : null}
            </ul>
          ) : null}
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

/** A sign-in provider: what the panel calls it, its issuer's host, its name, and which projects may use it. */
function SigninCard({
  provider: p,
  onManage,
  manage,
}: {
  provider: SigninProviderInfo;
  onManage: () => void;
  manage: { off?: string; disabled?: true };
}) {
  const { t } = useLingui();
  const who =
    p.projects.length === 0
      ? t`every project`
      : p.projects.length === 1
        ? t`1 project`
        : t`${p.projects.length} projects`;
  const host = issuerHost(p.issuer);
  const clientId = p.clientId;
  const shownHost = host && host !== (p.displayName || p.name) ? host : "";
  return (
    <Card
      icon="lock"
      name={p.displayName || p.name}
      state={<StatusBadge status="healthy" label={t`Connected`} title={host || undefined} />}
      text={
        <>
          <Trans>
            <code>{p.name}</code> for {who}
          </Trans>
          {/* Its issuer, and the client it signs in as, in one line of facts. */}
          <span className="integrations-sub">
            {/* Its host, where the card isn't already called by it. */}
            {shownHost ? <code>{shownHost}</code> : null}
            {shownHost && clientId ? " · " : null}
            {clientId ? (
              <Trans>
                Client <code>{clientId}</code>
              </Trans>
            ) : null}
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
