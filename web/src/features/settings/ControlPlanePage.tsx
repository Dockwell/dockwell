// SPDX-License-Identifier: AGPL-3.0-only

import { durationMs, timestampDate } from "@bufbuild/protobuf/wkt";
import { useQuery } from "@connectrpc/connect-query";
import { AuthService, type ControlPlane, Connection, type Server, SettingsService, StateStoreEngine } from "@dockwell/api";
import { Callout, Glyph, Icon, Panel, StatusBadge } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link } from "@tanstack/react-router";

import { sentence } from "../../lib/errors";
import { formatBytes, formatDay, formatFixed } from "../../lib/format";
import { useTimeWords } from "../../lib/time";
import { useControlPlaneNow, useFleet } from "../servers/useFleet";
import { Page } from "../shell/Page";
import { CopyCommand } from "../system/CopyCommand";
import "./Settings.css";

/** How to install, and so to upgrade: the same command again, as root (docs/install.md). */
export const INSTALL_COMMAND = "curl -fsSL https://get.dockwell.dev | sh";

/** The control plane and its agent together stay under this, at idle (PRD, ADR 0018). */
const BUDGET_BYTES = 250 * 1024 * 1024;

const DOCS = "https://github.com/Dockwell/dockwell/blob/main/docs/install.md";
const RELEASES = "https://github.com/Dockwell/dockwell/releases";

/**
 * Control plane, as Settings-ControlPlane.dc.html has it for one control plane:
 * what runs it and how it's doing, where it keeps its state and how to back that
 * up, how to upgrade it, its agents, and the addresses it answers on.
 */
export function ControlPlanePage() {
  const { t } = useLingui();
  const got = useQuery(SettingsService.method.getControlPlane, {});
  const auth = useQuery(AuthService.method.getAuthState, {});
  const fleet = useFleet();
  const servers = fleet.status === "connecting" ? undefined : fleet.servers;
  const cp = got.data?.controlPlane;
  const own = servers?.find((s) => s.controlServer);
  const name = auth.data?.installName ?? "";
  let lead = t`Panel, API and scheduler for this install`;
  if (name && own) lead = t`Panel, API and scheduler for ${name} · one replica, on ${own.name}`;
  else if (name) lead = t`Panel, API and scheduler for ${name}`;
  else if (own) lead = t`Panel, API and scheduler for this install · one replica, on ${own.name}`;
  return (
    <Page title={t`Control plane`} lead={lead} back="/settings" className="control-plane">
      {got.error ? <Callout tone="critical">{sentence(got.error)}</Callout> : null}
      <Replicas cp={cp} own={own} />
      <div className="cp-panels">
        <StorePanel cp={cp} />
        <BackupPanel cp={cp} />
        <UpdatesPanel cp={cp} />
        <AgentsPanel cp={cp} servers={servers} />
        <AddressesPanel cp={cp} />
      </div>
    </Page>
  );
}

/** One replica, for now: the control plane that answered, on its server. */
function Replicas({ cp, own }: { cp: ControlPlane | undefined; own: Server | undefined }) {
  const { t, i18n } = useLingui();
  const words = useTimeWords();
  const now = useControlPlaneNow(60_000);
  const started = cp?.startTime ? timestampDate(cp.startTime) : undefined;
  const memory = Number(cp?.memoryBytes ?? 0n);
  return (
    <section className="cp-replicas" aria-labelledby="cp-replicas">
      <div className="cp-section-head">
        <h2 id="cp-replicas" className="dw-text-heading">
          <Trans>Replicas</Trans>
        </h2>
        <span className="settings-meta">
          <Trans>One · if it stops, the panel and API stop with it, and servers keep running what they run</Trans>
        </span>
      </div>
      <div className="cp-replica-grid">
        <article className="dw-panel cp-replica" aria-busy={cp === undefined} aria-label={own?.name ?? t`This control plane`}>
          <div className="cp-replica-head">
            <Icon name="server" size={16} />
            <span className="cp-replica-name">{own?.name ?? t`This control plane`}</span>
            <span className="cp-grow" />
            {cp ? <StatusBadge status="healthy" /> : null}
          </div>
          <dl className="settings-facts cp-facts-tight">
            <dt>{t`Server`}</dt>
            <dd>
              {own ? (
                <Link to="/servers/$serverId" params={{ serverId: own.id }}>
                  {own.name}
                </Link>
              ) : (
                "–"
              )}
            </dd>
            <dt>{t`Up`}</dt>
            <dd>{started ? t`${words.span(Math.max(0, now - started.getTime()))}, since ${formatDay(started, i18n.locale)}` : "–"}</dd>
            <dt>{t`Version`}</dt>
            <dd>{cp?.build?.version ?? "–"}</dd>
            <dt>{t`Memory`}</dt>
            <dd>
              {memory > 0
                ? t`${formatBytes(memory, i18n.locale)} with its agent, of a ${formatBytes(BUDGET_BYTES, i18n.locale)} budget`
                : "–"}
            </dd>
          </dl>
        </article>
      </div>
    </section>
  );
}

/** The database it keeps its state in. */
function StorePanel({ cp }: { cp: ControlPlane | undefined }) {
  const { t, i18n } = useLingui();
  const store = cp?.store;
  const engine =
    store?.engine === StateStoreEngine.POSTGRES
      ? `PostgreSQL ${store.version}`
      : store
        ? `SQLite ${store.version}`
        : "–";
  const trip = store?.roundTrip ? durationMs(store.roundTrip) : undefined;
  return (
    <Panel
      title={t`State store`}
      actions={store ? <StatusBadge status="healthy" label={t`Answering`} labelIsState /> : null}
    >
      <dl className="settings-facts" aria-busy={cp === undefined}>
        <dt>{t`Engine`}</dt>
        <dd>
          {engine}
          {store ? (
            <span className="settings-sub">
              {store.engine === StateStoreEngine.POSTGRES
                ? t`Ready for more than one control plane`
                : t`A file beside the control plane, for one control plane`}
            </span>
          ) : null}
        </dd>
        <dt>{t`Location`}</dt>
        <dd>{store?.location ? <code>{store.location}</code> : "–"}</dd>
        <dt>{t`Size`}</dt>
        <dd>{store ? formatBytes(store.sizeBytes, i18n.locale) : "–"}</dd>
        <dt>{t`Answers in`}</dt>
        <dd>{trip !== undefined ? t`${formatFixed(trip, trip < 10 ? 1 : 0, i18n.locale)} ms` : "–"}</dd>
        <dt>{t`Key`}</dt>
        <dd>
          {cp?.keyFile ? <code>{cp.keyFile}</code> : "–"}
          <span className="settings-sub">{t`Opens every secret the store keeps`}</span>
        </dd>
      </dl>
    </Panel>
  );
}

/** How to back the state up, which Dockwell doesn't do itself yet. */
function BackupPanel({ cp }: { cp: ControlPlane | undefined }) {
  const { t } = useLingui();
  const store = cp?.store;
  const postgres = store?.engine === StateStoreEngine.POSTGRES;
  const copy = store && !postgres ? copyCommand(store.location) : "";
  return (
    <Panel title={t`Backups`} actions={<span className="settings-meta">{t`By hand, for now`}</span>}>
      <div className="cp-body" aria-busy={cp === undefined}>
        <p className="cp-text">
          <Trans>
            Dockwell doesn't back up its own state yet. Copy the state store and the key off this server, and keep
            them apart: the key opens every secret the store keeps, and without it a copy of the store opens none.
          </Trans>
        </p>
        {postgres ? (
          <p className="cp-text">
            <Trans>
              Back the database up with <code>pg_dump</code>, as any Postgres database, and the key with it, kept
              apart.
            </Trans>
          </p>
        ) : copy ? (
          <>
            <p className="cp-text">
              <Trans>
                Stop Dockwell for the copy, so the file is whole. Your apps keep serving while it's stopped.
              </Trans>
            </p>
            <CopyCommand command={copy} />
          </>
        ) : null}
        <p className="cp-note">
          <Trans>
            Apps' volumes and databases have backups of their own, under <Link to="/backups">Backups</Link>.
          </Trans>{" "}
          <a href={`${DOCS}#back-up-the-control-plane`} target="_blank" rel="noreferrer">
            <Trans>How to back up and restore it</Trans>
          </a>
        </p>
      </div>
    </Panel>
  );
}

/** Stops Dockwell, copies the SQLite file beside root's home, and starts it again, whatever the copy did. */
function copyCommand(file: string): string {
  return `sudo systemctl stop dockwell; sudo cp -p ${file} /root/dockwell-state.db; sudo systemctl start dockwell`;
}

/** What it runs, and how to upgrade it. */
function UpdatesPanel({ cp }: { cp: ControlPlane | undefined }) {
  const { t, i18n } = useLingui();
  const build = cp?.build;
  const built = build?.commitTime ? formatDay(timestampDate(build.commitTime), i18n.locale) : "";
  const commit = build?.commit.slice(0, 12) ?? "";
  let sub = "";
  if (built && commit) sub = t`Built ${built} from ${commit}`;
  else if (commit) sub = t`Built from ${commit}`;
  return (
    <Panel
      title={t`Updates`}
      actions={
        <a href={RELEASES} target="_blank" rel="noreferrer" className="cp-head-link">
          <Trans>Releases</Trans>
        </a>
      }
    >
      <div className="cp-body" aria-busy={cp === undefined}>
        <div className="cp-current">
          <span className="cp-current-glyph">
            <Glyph shape="circle" size={10} />
          </span>
          <div className="cp-current-text">
            <span className="cp-current-title">
              {build?.version ? t`${build.version} runs here` : t`The version that runs here is unknown`}
            </span>
            {sub ? <span className="settings-meta">{sub}</span> : null}
          </div>
        </div>
        <div className="cp-divided">
          <p className="cp-text">
            <Trans>
              To upgrade, run the install script again on this server, as root. If there's a newer release, it
              verifies and installs it, then restarts Dockwell. Your apps keep serving.
            </Trans>
          </p>
          <CopyCommand command={INSTALL_COMMAND} />
          <p className="cp-note">
            <Trans>
              Dockwell doesn't check for releases or upgrade itself yet. Add <code>--version 0.4.0</code> for a
              particular release.
            </Trans>
          </p>
        </div>
      </div>
    </Panel>
  );
}

/** Its servers' agents: how many, which versions, and which aren't connected. */
function AgentsPanel({ cp, servers }: { cp: ControlPlane | undefined; servers: Server[] | undefined }) {
  const { t, i18n } = useLingui();
  const words = useTimeWords();
  const now = useControlPlaneNow(60_000);
  const version = cp?.build?.version ?? "";
  const all = servers ?? [];
  const connected = all.filter((s) => s.connection === Connection.CONNECTED);
  const away = all.filter((s) => s.connection !== Connection.CONNECTED);
  const behind = all.filter((s) => s.agentBuild?.version && version && s.agentBuild.version !== version);
  const count = all.length;
  let versions = "";
  if (version && count > 0) {
    versions =
      behind.length === 0
        ? t`all on ${version}`
        : t`${count - behind.length} on ${version}, ${behind.length} on another version`;
  }
  return (
    <Panel
      title={t`Agents`}
      actions={
        <Link to="/servers" className="cp-head-link">
          <Trans>Servers</Trans>
        </Link>
      }
    >
      <div className="cp-body" aria-busy={servers === undefined}>
        <div className="cp-count">
          <span className="dw-text-metric">{servers ? count : "–"}</span>
          <span className="cp-count-label">
            {versions ? (count === 1 ? t`agent · ${versions}` : t`agents · ${versions}`) : count === 1 ? t`agent` : t`agents`}
          </span>
        </div>
        {servers ? (
          <ul className="cp-agents">
            <li>
              <StatusBadge status="healthy" label={t`${connected.length} connected`} labelIsState />
            </li>
            {away.map((s) => (
              <li key={s.id}>
                <StatusBadge
                  status={s.connection === Connection.PENDING ? "stopped" : "down"}
                  label={<code>{s.name}</code>}
                  meta={
                    s.lastSeenTime
                      ? t`not connected, last heard ${words.ago(timestampDate(s.lastSeenTime), now)}`
                      : t`hasn't connected yet`
                  }
                />
              </li>
            ))}
            {behind.map((s) => (
              <li key={`v-${s.id}`}>
                <StatusBadge status="degraded" label={<code>{s.name}</code>} meta={t`runs ${s.agentBuild?.version ?? ""}`} />
              </li>
            ))}
          </ul>
        ) : null}
        <p className="cp-note cp-divided">
          <Trans>A server that joined upgrades the same way: run the install script again on it.</Trans>
        </p>
      </div>
    </Panel>
  );
}

/** Where people and agents reach it. */
function AddressesPanel({ cp }: { cp: ControlPlane | undefined }) {
  const { t } = useLingui();
  const panel = cp?.panel;
  const agents = cp?.agents;
  return (
    <Panel title={t`Addresses`}>
      <dl className="settings-facts" aria-busy={cp === undefined}>
        <dt>{t`Panel and API`}</dt>
        <dd>
          {panel?.domain ? (
            <a href={`https://${panel.domain}`} className="settings-mono">
              https://{panel.domain}
            </a>
          ) : null}
          {panel?.listen ? (
            <span className={panel.domain ? "settings-sub" : undefined}>
              <code>{panel.listen}</code> <span className="settings-muted">{t`on loopback`}</span>
            </span>
          ) : (
            "–"
          )}
        </dd>
        <dt>{t`Agents connect to`}</dt>
        <dd>{agents?.listen ? <code>{agents.listen}</code> : "–"}</dd>
        <dt>{t`From its network`}</dt>
        <dd>{agents?.url ? <code>{agents.url}</code> : "–"}</dd>
        <dt>{t`From elsewhere`}</dt>
        <dd>
          {agents && agents.publicUrls.length > 0 ? (
            agents.publicUrls.map((u) => (
              <code key={u} className="cp-line">
                {u}
              </code>
            ))
          ) : (
            <span className="settings-muted">{t`Not known`}</span>
          )}
        </dd>
      </dl>
      <p className="settings-how">
        <Trans>
          These come from how Dockwell was started. The panel's domain is under{" "}
          <Link to="/settings/general">General</Link>; the rest is set the same way.
        </Trans>{" "}
        <a href={`${DOCS}#change-settings`} target="_blank" rel="noreferrer">
          <Trans>How to change settings</Trans>
        </a>
      </p>
    </Panel>
  );
}
