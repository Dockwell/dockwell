// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate } from "@bufbuild/protobuf/wkt";
import { useQuery } from "@connectrpc/connect-query";
import {
  AuthService,
  Connection,
  type ControlPlane,
  type Server,
  ServiceManager,
  SettingsService,
  StateStoreEngine,
} from "@dockwell/api";
import { Button, Glyph, Icon, Panel, STATUS, StatusBadge } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link } from "@tanstack/react-router";

import { formatBytes, formatDay, formatFixed } from "../../lib/format";
import { RouterButtonLink } from "../../lib/links";
import { useTimeWords } from "../../lib/time";
import { serverStatus } from "../servers/server";
import { useControlPlaneNow, useFleet } from "../servers/useFleet";
import { Page } from "../shell/Page";
import { shortVersion } from "../shell/version";
import { CopyCommand } from "../system/CopyCommand";
import "./Settings.css";
import { ReadFailed } from "../shell/ReadFailed";

/**
 * How to install, and so to upgrade: the same command again, as root
 * (docs/install.md). It names the script in the repository, which works today,
 * as get.dockwell.dev isn't set up yet.
 */
export const INSTALL_SCRIPT = "https://raw.githubusercontent.com/Dockwell/dockwell/main/scripts/install.sh";
export const INSTALL_COMMAND = `curl -fsSL ${INSTALL_SCRIPT} | sudo sh`;

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
    <Page width="narrow" title={t`Control plane`} lead={lead} back="/settings" className="control-plane">
      {got.error ? (
        <ReadFailed
          error={got.error}
          title={t`Dockwell can't say how its control plane is`}
          actions={
            <Button size="sm" icon="refresh" loading={got.isFetching} onClick={() => void got.refetch()}>
              <Trans>Try again</Trans>
            </Button>
          }
        />
      ) : null}
      <Replicas cp={cp} own={own} />
      <div className="cp-panels">
        {/* Paired by length, so a row's panels end close together: the facts, then the instructions. */}
        <StorePanel cp={cp} />
        <AgentsPanel cp={cp} servers={servers} />
        <BackupPanel cp={cp} />
        <UpdatesPanel cp={cp} />
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
        <article
          className="dw-panel cp-replica"
          aria-busy={cp === undefined}
          aria-label={own?.name ?? t`This control plane`}
        >
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
                <Link to="/servers/$serverId" params={{ serverId: own.id }} className="cp-link">
                  {own.name}
                </Link>
              ) : (
                "–"
              )}
            </dd>
            <dt>{t`Up`}</dt>
            <dd>
              {started
                ? t`${words.span(Math.max(0, now - started.getTime()))}, since ${formatDay(started, i18n.locale)}`
                : "–"}
            </dd>
            <dt>{t`State store`}</dt>
            <dd>
              {cp?.store
                ? cp.store.engine === StateStoreEngine.POSTGRES
                  ? t`PostgreSQL`
                  : t`SQLite, on this server`
                : "–"}
            </dd>
            <dt>{t`Version`}</dt>
            <dd title={cp?.build?.version}>{cp?.build?.version ? shortVersion(cp.build.version) : "–"}</dd>
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
  // In milliseconds, to the microsecond: durationMs rounds to whole ones, and a local store answers in less.
  const trip = store?.roundTrip ? Number(store.roundTrip.seconds) * 1000 + store.roundTrip.nanos / 1e6 : undefined;
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
        <dt>{t({ message: "Size", context: "on disk" })}</dt>
        <dd>{store ? formatBytes(store.sizeBytes, i18n.locale) : "–"}</dd>
        <dt>{t`Answers in`}</dt>
        <dd>
          {trip === undefined
            ? "–"
            : trip < 0.1
              ? t`under 0.1 ms`
              : t`${formatFixed(trip, trip < 10 ? 1 : 0, i18n.locale)} ms`}
        </dd>
        <dt>{t({ message: "Key", context: "encryption" })}</dt>
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
  const manager = cp?.serviceManager ?? ServiceManager.UNSPECIFIED;
  const copy = cp?.dataDir && cp.keyFile ? copyCommand(cp.dataDir, cp.keyFile, manager) : "";
  return (
    <Panel title={t`Backups`} actions={<span className="settings-meta">{t`By hand, for now`}</span>}>
      <div className="cp-body" aria-busy={cp === undefined}>
        {copy ? (
          <>
            {/* Dockwell's own terminal ends when Dockwell stops, so nothing after the stop would run. */}
            <p className="cp-text">
              {manager === ServiceManager.SYSTEMD ? (
                <Trans>
                  Run it over SSH, not in Dockwell's terminal: Dockwell stops while it copies; apps keep serving.
                </Trans>
              ) : manager === ServiceManager.OPENRC ? (
                <Trans>
                  Run it as root over SSH, not in Dockwell's terminal: Dockwell stops while it copies; apps keep
                  serving.
                </Trans>
              ) : (
                <Trans>
                  Not a service here: stop <code>dockwell run</code>, run this as root, then start it again.
                </Trans>
              )}
            </p>
            <CopyCommand command={copy} className="cp-commands" />
            <p className="cp-note">
              <Trans>
                Then move <code>/root/dockwell-control.tgz</code> and <code>/root/dockwell-key</code> off this server,
                to two places: the key opens every secret in the copy.
              </Trans>
            </p>
          </>
        ) : null}
        {postgres ? (
          <p className="cp-note">
            <Trans>
              The database isn't in it: back it up with <code>pg_dump</code>.
            </Trans>
          </p>
        ) : null}
        <p className="cp-note cp-divided">
          <span>
            <Trans>
              Apps' volumes and databases have backups of their own, under{" "}
              <Link to="/backups" className="cp-link">
                Backups
              </Link>
              .
            </Trans>{" "}
            <a href={`${DOCS}#back-up-the-control-plane`} target="_blank" rel="noreferrer" className="cp-link">
              <Trans>How to back it up and restore it</Trans>
            </a>
          </span>
        </p>
      </div>
    </Panel>
  );
}

/**
 * Packs the control plane's folder and its own agent's in root's home, with
 * the key apart from them, as docs/install.md says. Under a service manager it
 * stops Dockwell first and copies only once that worked, so a store still
 * being written is never copied, then starts it again whatever the copy did.
 * Started by hand, Dockwell is stopped by hand, and the command only copies.
 */
export function copyCommand(dataDir: string, keyFile: string, manager: ServiceManager): string {
  const key = keyFile.startsWith(`${dataDir}/`) ? keyFile.slice(dataDir.length + 1) : "";
  // With what a first start can leave beside the key, such as key.new.
  const exclude = key ? ` --exclude '${key}*'` : "";
  const sudo = manager === ServiceManager.SYSTEMD ? "sudo " : "";
  const tar = `${sudo}tar -C ${dataDir}${exclude} -czf /root/dockwell-control.tgz control agent`;
  const cp = `${sudo}cp -p ${keyFile} /root/dockwell-key`;
  switch (manager) {
    case ServiceManager.SYSTEMD:
      return ["sudo systemctl stop dockwell &&", `  ${tar} &&`, `  ${cp}`, "sudo systemctl start dockwell"].join("\n");
    case ServiceManager.OPENRC:
      return ["rc-service dockwell stop &&", `  ${tar} &&`, `  ${cp}`, "rc-service dockwell start"].join("\n");
    default:
      return [`${tar} &&`, `  ${cp}`].join("\n");
  }
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
          {/* It runs, and nothing has checked it's the latest, so it isn't drawn as healthy. */}
          <span className="cp-current-glyph">
            <Glyph shape={STATUS.running.shape} size={10} />
          </span>
          <div className="cp-current-text">
            <span className="cp-current-title">
              {build?.version ? t`${shortVersion(build.version)} runs here` : t`The version that runs here is unknown`}
            </span>
            {sub ? <span className="settings-meta">{sub}</span> : null}
          </div>
        </div>
        <div className="cp-divided">
          <p className="cp-text">
            <Trans>On this server, this installs the newest release and restarts Dockwell; apps keep serving.</Trans>
          </p>
          <CopyCommand command={INSTALL_COMMAND} />
          <p className="cp-note">
            <Trans>
              For a particular release, end it with <code>sudo sh -s -- --version 0.4.0</code>.
            </Trans>
          </p>
        </div>
      </div>
    </Panel>
  );
}

/** Its servers' agents: how many, which versions, and which aren't connected. */
function AgentsPanel({ cp, servers }: { cp: ControlPlane | undefined; servers: Server[] | undefined }) {
  const { t } = useLingui();
  const words = useTimeWords();
  const now = useControlPlaneNow(60_000);
  const version = cp?.build?.version ?? "";
  const short = version ? shortVersion(version) : "";
  const all = servers ?? [];
  const connected = all.filter((s) => s.connection === Connection.CONNECTED);
  const away = all.filter((s) => s.connection !== Connection.CONNECTED);
  const behind = all.filter((s) => s.agentBuild?.version && version && s.agentBuild.version !== version);
  const count = all.length;
  let versions = "";
  if (version && count > 0) {
    versions =
      behind.length === 0
        ? t`all on ${short}`
        : t`${count - behind.length} on ${short}, ${behind.length} on another version`;
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
            {versions
              ? count === 1
                ? t`agent · ${versions}`
                : t`agents · ${versions}`
              : count === 1
                ? t`agent`
                : t`agents`}
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
                  status={serverStatus(s)}
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
              <li key={`v-${s.id}`} className="cp-agent-behind">
                <StatusBadge
                  status="degraded"
                  label={<code>{s.name}</code>}
                  meta={t`runs ${shortVersion(s.agentBuild?.version ?? "")}`}
                />
                {/* Its Updates tab has the command that updates it, from where this install's releases come. */}
                <RouterButtonLink
                  to="/servers/$serverId/updates"
                  params={{ serverId: s.id }}
                  size="sm"
                  variant="secondary"
                  aria-label={t`Update the agent on ${s.name}`}
                >
                  <Trans>Update</Trans>
                </RouterButtonLink>
              </li>
            ))}
          </ul>
        ) : null}
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
    <Panel title={t`Addresses`} className="cp-wide">
      <dl className="settings-facts" aria-busy={cp === undefined}>
        <dt>{t`Panel and API`}</dt>
        <dd>
          {panel?.domain ? (
            <a href={panel.url || `https://${panel.domain}`} className="settings-mono cp-link">
              {panel.url || `https://${panel.domain}`}
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
          Set where Dockwell starts; the panel's domain is under{" "}
          <Link to="/settings/general" className="cp-link">
            General
          </Link>
          .
        </Trans>{" "}
        <a href={`${DOCS}#change-settings`} target="_blank" rel="noreferrer" className="cp-link">
          <Trans>How to change settings</Trans>
        </a>
      </p>
    </Panel>
  );
}
