// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate } from "@bufbuild/protobuf/wkt";
import type { BackupDestinationInfo } from "@dockwell/api";
import { Button, Icon, type IconName, Panel, PHONE_QUERY, StatusBadge, useMediaQuery } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link } from "@tanstack/react-router";
import { type ReactNode, useState } from "react";

import { formatBytes } from "../../lib/format";
import { useTimeWords } from "../../lib/time";
import { DestinationSheet, useDestinations } from "../backups/DestinationsPanel";
import { useControlPlaneNow } from "../servers/useFleet";
import { Page } from "../shell/Page";
import "../backups/Backups.css";
import "./Settings.css";
import { ReadFailed } from "../shell/ReadFailed";

const CLOUDFLARE =
  "https://github.com/Dockwell/dockwell/blob/main/docs/install.md#behind-cloudflare-tunnel-or-another-proxy";

/**
 * Integrations, as Settings-Integrations.dc.html has them, for what Dockwell
 * connects to today: storage backups go to, which a person adds here as a plan,
 * and Cloudflare in front of routes. What comes later is said in words, never
 * drawn as a card to connect.
 */
export function IntegrationsPage() {
  const { t } = useLingui();
  const phone = useMediaQuery(PHONE_QUERY);
  const destinations = useDestinations();
  const [adding, setAdding] = useState(false);
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
    <Button size="sm" icon="plus" onClick={() => setAdding(true)}>
      <Trans>Connect</Trans>
    </Button>
  );
  return (
    <Page width="narrow" title={t`Integrations`} lead={lead} phoneSub={lead} back="/settings" actions={variables}>
      <section className="integrations-section" aria-labelledby="integrations-storage">
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
            <Button size="sm" variant="ghost" icon="plus" className="integrations-more" onClick={() => setAdding(true)}>
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
                text={t`Amazon S3, Cloudflare R2, Backblaze B2, MinIO or any S3 API. Backups are sealed on their server before they leave.`}
                action={connect}
              />
            ) : null}
          </div>
        )}
      </section>

      <section className="integrations-section" aria-labelledby="integrations-dns">
        <div className="cp-section-head">
          <h2 id="integrations-dns" className="dw-text-heading">
            <Trans>DNS and proxies</Trans>
          </h2>
          <span className="settings-meta">{t`in front of a server's routes`}</span>
        </div>
        <div className="integrations-grid">
          <Card
            icon="globe"
            name="Cloudflare"
            state={<span className="settings-meta">{t`Works in front of routes`}</span>}
            text={t`A route can sit behind Cloudflare's proxy, trusting its addresses, or behind a Cloudflare Tunnel, with TLS ending there. Managing its DNS records from Dockwell comes later.`}
            action={
              <a href={CLOUDFLARE} target="_blank" rel="noreferrer" className="dw-btn dw-btn-secondary dw-btn-sm">
                <Icon name="external" size={14} />
                <span>
                  <Trans>How to route behind it</Trans>
                </span>
              </a>
            }
          />
          <Card
            icon="link"
            name={t`Domains`}
            state={<span className="settings-meta">{t`Any DNS provider`}</span>}
            text={t`Point a domain's record at a server and Dockwell gets its certificate. The domain form says which record to make, and follows DNS until it's there.`}
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

      <Panel title={t`Coming later`} className="settings-panel">
        <dl className="settings-facts integrations-later">
          <dt>{t`Git`}</dt>
          <dd>{t`GitHub, GitLab and Gitea, to deploy an app from a repository when it changes. Today an app runs an image, or a Compose file.`}</dd>
          <dt>{t`Registries`}</dt>
          <dd>
            {t`Credentials for private images. Today a server pulls public images only, without its docker login, so a private image runs only once someone has pulled it on that server by hand.`}
          </dd>
          <dt>{t`Cloud providers`}</dt>
          <dd>{t`Hetzner, DigitalOcean, Vultr and AWS, to make and replace servers. Today a server joins with the install script and a ticket.`}</dd>
        </dl>
      </Panel>

      {/* A phone's header has no room for the aside, so it closes the page. */}
      {phone ? variables : null}
      {adding ? <DestinationSheet onClose={() => setAdding(false)} /> : null}
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
