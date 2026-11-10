// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate } from "@bufbuild/protobuf/wkt";
import { createClient } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import {
  type DockwellRelease,
  PackageEffect,
  type PackageUpdate,
  type Server,
  type ServerUpdates,
  UpdateService,
} from "@dockwell/api";
import {
  Button,
  Callout,
  Icon,
  Panel,
  PHONE_QUERY,
  StatusBadge,
  Table,
  type TableColumn,
  useMediaQuery,
} from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { type ReactNode, useId, useMemo, useState } from "react";

import { sentence } from "../../lib/errors";
import { useTimeWords } from "../../lib/time";
import { CopyButton } from "../servers/CopyButton";
import { ServerFrame } from "../servers/ServerFrame";
import { useControlPlaneNow } from "../servers/useFleet";
import { ReadFailed } from "../shell/ReadFailed";
import {
  installCommand,
  managerName,
  plainVersion,
  releaseStanding,
  splitVersion,
  summarize,
  type UpdatesSummary,
  upgradeCommand,
  useServerUpdates,
} from "./updates";
import { useWhyNot, type WhyNot } from "./why";
import "../servers/ServerPanels.css";
import "./Updates.css";

/** A server's Updates tab: what waits, which are security fixes, and Docker's and Dockwell's versions. */
export function UpdatesPage({ serverId }: { serverId: string }) {
  return <ServerFrame id={serverId}>{(server) => <Updates server={server} />}</ServerFrame>;
}

function Updates({ server }: { server: Server }) {
  const { t } = useLingui();
  const query = useServerUpdates(server.id);
  const transport = useTransport();
  const client = useMemo(() => createClient(UpdateService, transport), [transport]);
  const whyNot = useWhyNot();
  const [asked, setAsked] = useState<unknown>();
  // From the press until the control plane answers, as it may answer only once it has read.
  const [pressed, setPressed] = useState(false);
  const updates = query.data?.updates;
  const summary = summarize(updates);
  const check = async () => {
    setAsked(undefined);
    setPressed(true);
    try {
      await client.checkServerUpdates({ id: server.id });
      await query.refetch();
    } catch (err) {
      setAsked(err);
    } finally {
      setPressed(false);
    }
  };
  if (query.isPending) {
    return (
      <div className="updates-layout">
        <div className="updates-main">
          <p className="updates-waiting">
            <Trans>Asking {server.name} which updates wait…</Trans>
          </p>
        </div>
      </div>
    );
  }
  if (query.error && !updates) {
    return (
      // A role that doesn't cover a server's updates is told so as everywhere else.
      <ReadFailed error={query.error} tone="warning" title={t`Dockwell couldn't ask ${server.name} for its updates`} />
    );
  }
  if (!updates) return null;
  const release = query.data?.dockwell;
  const checking = updates.checking || pressed;
  const checkButton = (
    <Button variant="ghost" size="sm" icon="refresh" loading={checking} onClick={() => void check()}>
      {checking ? (updates.refreshing ? t`Refreshing lists` : t`Checking`) : t`Check now`}
    </Button>
  );
  const why = whyNot(server, updates, release);
  if (updates.unavailable) {
    return why ? <WhyNotCallout why={why} /> : null;
  }
  // Nothing read yet: a first read under way, one that failed, or an agent away.
  // That's said as it is, and never as a read that found nothing.
  if (!updates.readTime) {
    if (checking && !why) {
      return (
        <section className="updates-section" aria-labelledby="updates-asking">
          <div className="updates-section-head">
            <p className="updates-waiting" id="updates-asking">
              {updates.refreshing
                ? t`Refreshing ${server.name}'s package lists, then reading which updates wait…`
                : t`Asking ${server.name} which updates wait…`}
            </p>
            <span className="updates-grow" />
            {checkButton}
          </div>
        </section>
      );
    }
    return why ? <WhyNotCallout why={why} action={why.retry ? checkButton : undefined} /> : null;
  }
  return (
    <div className="updates-layout">
      <div className="updates-main">
        {why ? <WhyNotCallout why={why} /> : <Lead server={server} updates={updates} summary={summary} />}
        {asked ? <p className="updates-note updates-note-warning">{sentence(asked)}</p> : null}
        {updates.marksSecurity ? (
          <>
            <SecuritySection server={server} updates={updates} summary={summary} action={checkButton} />
            {summary.other.length > 0 ? <OtherUpdates server={server} updates={updates} summary={summary} /> : null}
          </>
        ) : (
          <AllSection server={server} updates={updates} summary={summary} action={checkButton} />
        )}
        <Held updates={updates} />
      </div>
      <div className="updates-side">
        <HowItUpdates updates={updates} />
        <DockerAndDockwell server={server} updates={updates} summary={summary} release={release} />
      </div>
    </div>
  );
}

/** The one callout the tab leads with: what's most worth doing, or why what's here can't stand. */
function Lead({ server, updates, summary }: { server: Server; updates: ServerUpdates; summary: UpdatesSummary }) {
  const { t } = useLingui();
  const command = <code>{upgradeCommand(updates.manager)}</code>;
  const auto = updates.automatic;
  const tool = auto?.on ? auto.tool : "";
  if (updates.rebootRequired) {
    const what = updates.rebootPackages.join(", ");
    return (
      <Callout tone="warning" title={t`${server.name} needs a reboot`}>
        {what ? (
          <Trans>
            What {what} changed takes effect only once {server.name} restarts, so it still runs the old versions. Apps
            and websites on it go offline while it does: reboot it when that suits you, with <code>sudo reboot</code>.
          </Trans>
        ) : (
          <Trans>
            What was installed takes effect only once {server.name} restarts, so it still runs the old versions. Apps
            and websites on it go offline while it does: reboot it when that suits you, with <code>sudo reboot</code>.
          </Trans>
        )}
      </Callout>
    );
  }
  if (summary.security.length > 0) {
    const n = summary.security.length;
    const title = plural(n, {
      one: "# security update is ready to install",
      other: "# security updates are ready to install",
    });
    const reboots = summary.security.filter((p) => p.effect === PackageEffect.REBOOT).length;
    const name = server.name;
    const rebooting =
      reboots === 0
        ? ""
        : n === 1
          ? t`It needs a reboot to take effect, and apps and websites go offline while ${name} restarts.`
          : plural(reboots, {
              one: `One of them needs a reboot to take effect, and apps and websites go offline while ${name} restarts.`,
              other: `# of them need a reboot to take effect, and apps and websites go offline while ${name} restarts.`,
            });
    return (
      <Callout tone="warning" title={title}>
        {rebooting ? `${rebooting} ` : null}
        {tool ? (
          <Trans>
            <code>{tool}</code> on {name} installs updates by itself, so these go in within about a day. To install them
            now, run {command} there, which installs every update waiting, not only these.
          </Trans>
        ) : (
          <Trans>
            Dockwell can't install updates yet, so run {command} on {name}, which installs every update waiting, not
            only these.
          </Trans>
        )}
      </Callout>
    );
  }
  return null;
}

/** Why what's shown isn't a current read, as a callout, with Check now where it could help. */
function WhyNotCallout({ why, action }: { why: WhyNot; action?: ReactNode }) {
  return (
    <Callout
      tone={why.tone}
      shape={why.shape === "dashed-ring" ? "dashed-ring" : undefined}
      title={why.title}
      actions={action}
    >
      {why.body}
    </Callout>
  );
}

/** When the lists were last refreshed, for a section's heading. */
function useListsAgo(updates: ServerUpdates): string {
  const { t } = useLingui();
  const words = useTimeWords();
  const now = useControlPlaneNow();
  return updates.listsTime ? t`checked ${words.ago(timestampDate(updates.listsTime), now)}` : t`lists never refreshed`;
}

function SecuritySection({
  server,
  updates,
  summary,
  action,
}: {
  server: Server;
  updates: ServerUpdates;
  summary: UpdatesSummary;
  action: ReactNode;
}) {
  const { t } = useLingui();
  const checked = useListsAgo(updates);
  const n = summary.security.length;
  const from = summary.securityFrom;
  const source = from.length === 1 ? from[0] : "";
  const meta = n === 0 ? checked : source ? `${t`${n} from ${source}`} · ${checked}` : `${n} · ${checked}`;
  return (
    <section className="updates-section" aria-labelledby="updates-security">
      <div className="updates-section-head">
        <h2 className="heading" id="updates-security">
          <Trans>Security updates</Trans>
        </h2>
        <span className="updates-meta">{meta}</span>
        <span className="updates-grow" />
        {action}
      </div>
      <RefreshNote updates={updates} />
      {n > 0 ? (
        <UpdatesTable updates={updates} packages={summary.security} caption={t`Security updates for ${server.name}`} />
      ) : (
        <p className="updates-clear">
          <StatusBadge status="healthy" label={t`None waiting`} labelIsState />
          <span>
            {summary.total === 0
              ? t`${server.name} is up to date, as far as its lists say.`
              : t`No security fix waits on ${server.name}.`}
          </span>
        </p>
      )}
    </section>
  );
}

/** Every update in one list, where the distribution doesn't mark security fixes, as Alpine doesn't. */
function AllSection({
  server,
  updates,
  summary,
  action,
}: {
  server: Server;
  updates: ServerUpdates;
  summary: UpdatesSummary;
  action: ReactNode;
}) {
  const { t } = useLingui();
  const checked = useListsAgo(updates);
  const all = [...summary.security, ...summary.other];
  return (
    <section className="updates-section" aria-labelledby="updates-all">
      <div className="updates-section-head">
        <h2 className="heading" id="updates-all">
          <Trans>Updates</Trans>
        </h2>
        <span className="updates-meta">{all.length > 0 ? `${all.length} · ${checked}` : checked}</span>
        <span className="updates-grow" />
        {action}
      </div>
      <RefreshNote updates={updates} />
      <p className="updates-note">
        <Trans>
          {managerName(updates.manager)} doesn't say which updates fix security issues, so none is marked here.
        </Trans>
      </p>
      {all.length > 0 ? (
        <UpdatesTable updates={updates} packages={all} caption={t`Updates for ${server.name}`} />
      ) : (
        <p className="updates-clear">
          <StatusBadge status="healthy" label={t`None waiting`} labelIsState />
          <span>{t`${server.name} is up to date, as far as its lists say.`}</span>
        </p>
      )}
    </section>
  );
}

/** Says when refreshing the lists failed, so what's shown is as old as they are. */
function RefreshNote({ updates }: { updates: ServerUpdates }) {
  if (!updates.refreshError) return null;
  return (
    <p className="updates-note updates-note-warning">
      <Trans>The package lists couldn't be refreshed, so this is what they said before: {updates.refreshError}</Trans>
    </p>
  );
}

/** The updates that aren't security fixes, folded away until asked for. */
function OtherUpdates({
  server,
  updates,
  summary,
}: {
  server: Server;
  updates: ServerUpdates;
  summary: UpdatesSummary;
}) {
  const { t } = useLingui();
  const [open, setOpen] = useState(false);
  const id = useId();
  const others = summary.other;
  const reboots = others.filter((p) => p.effect === PackageEffect.REBOOT).length;
  const from = summary.otherFrom;
  const fromWords = from.length === 1 ? t`From ${from[0]}` : from.length > 1 ? t`From ${from.length} sources` : "";
  const rebootWords =
    reboots === 0 ? t`none needs a reboot` : plural(reboots, { one: "# needs a reboot", other: "# need a reboot" });
  const shown = others.slice(0, 4).map((p) => p.name);
  const more = others.length - shown.length;
  const names = more > 0 ? t`${shown.join(", ")} and ${more} more` : shown.join(", ");
  return (
    <section className="dw-panel updates-other">
      <button
        type="button"
        className="updates-other-toggle"
        aria-expanded={open}
        aria-controls={id}
        onClick={() => setOpen((was) => !was)}
      >
        <span className="updates-other-chevron">
          <Icon name={open ? "chevron-down" : "chevron-right"} size={16} />
        </span>
        <span className="updates-other-text">
          <span className="updates-other-title">
            <span>
              <Trans>Other updates</Trans>
            </span>
            <span className="dw-tab-count">{others.length}</span>
          </span>
          <span className="updates-other-sub">
            {[fromWords ? `${fromWords}, ${rebootWords}` : rebootWords, names].join(" · ")}
          </span>
        </span>
        <span className="updates-other-show">{open ? t`Hide` : t`Show all`}</span>
      </button>
      <div id={id} hidden={!open} className="updates-other-body">
        {open ? (
          <UpdatesTable updates={updates} packages={others} caption={t`Other updates for ${server.name}`} />
        ) : null}
      </div>
    </section>
  );
}

/** What an upgrade holds back, as Ubuntu's phased updates. */
function Held({ updates }: { updates: ServerUpdates }) {
  const n = updates.held.length;
  if (n === 0) return null;
  const names = updates.held.join(", ");
  return (
    <p className="updates-note">
      {plural(n, {
        one: `# more is held back: ${names}. An upgrade doesn't take it yet, as with a phased update that hasn't reached this server.`,
        other: `# more are held back: ${names}. An upgrade doesn't take them yet, as with phased updates that haven't reached this server.`,
      })}
    </p>
  );
}

/** What installing a package does, beyond replacing its files. */
function useEffectWords(updates: ServerUpdates) {
  const { t } = useLingui();
  return (p: PackageUpdate): { effect: string; sub: string; strong: boolean } => {
    switch (p.effect) {
      case PackageEffect.REBOOT:
        return { effect: t`Needs a reboot`, sub: t`Apps and websites go offline`, strong: true };
      case PackageEffect.DOCKER_RESTART:
        return updates.dockerLiveRestore
          ? { effect: t`Restarts Docker`, sub: t`Containers keep running`, strong: false }
          : { effect: t`Restarts Docker`, sub: t`Containers stop; those set to restart start again`, strong: true };
    }
    return { effect: t`No reboot`, sub: "", strong: false };
  };
}

/** The available version, with what changed from the installed one marked. */
function Available({ p }: { p: PackageUpdate }) {
  const [same, changed] = splitVersion(p.installed, p.available);
  return (
    <span className="updates-version">
      {same}
      <mark className="updates-changed">{changed}</mark>
    </span>
  );
}

/** A package's name, with its description and advisory beneath. */
function Name({ p }: { p: PackageUpdate }) {
  const advisory = [p.advisory, p.severity].filter(Boolean).join(", ");
  return (
    <div className="dw-cell">
      <span className="updates-name">{p.name}</span>
      {p.summary ? <span className="updates-summary">{p.summary}</span> : null}
      {advisory ? <span className="updates-summary">{advisory}</span> : null}
    </div>
  );
}

function UpdatesTable({
  updates,
  packages,
  caption,
}: {
  updates: ServerUpdates;
  packages: PackageUpdate[];
  caption: string;
}) {
  const { t } = useLingui();
  const phone = useMediaQuery(PHONE_QUERY);
  const effectOf = useEffectWords(updates);
  const rows = packages.map((p) => ({ key: `${p.name}:${p.architecture}`, p }));
  if (phone) {
    return (
      <ul className="dw-panel updates-list" aria-label={caption}>
        {rows.map(({ key, p }) => {
          const e = effectOf(p);
          return (
            <li key={key} className="updates-list-row">
              <Name p={p} />
              <span className="updates-list-versions">
                <span className="updates-installed">
                  {p.installed || t({ message: "New", context: "a package not installed yet" })}
                </span>
                <span className="updates-arrow" aria-hidden="true">
                  →
                </span>
                <Available p={p} />
              </span>
              <span className={e.strong ? "updates-effect updates-effect-strong" : "updates-effect"}>
                {e.sub ? `${e.effect} · ${e.sub}` : e.effect}
              </span>
            </li>
          );
        })}
      </ul>
    );
  }
  type Row = (typeof rows)[number];
  const columns: TableColumn<Row>[] = [
    { key: "pkg", header: t`Package`, render: (r) => <Name p={r.p} /> },
    {
      key: "installed",
      header: t({ message: "Installed", context: "a package's version" }),
      width: 168,
      render: (r) => (
        <span className="updates-installed">
          {r.p.installed || t({ message: "New", context: "a package not installed yet" })}
        </span>
      ),
    },
    { key: "available", header: t`Available`, width: 184, render: (r) => <Available p={r.p} /> },
    {
      key: "effect",
      header: t`When installed`,
      width: 232,
      render: (r) => {
        const e = effectOf(r.p);
        return (
          <div className="dw-cell">
            <span className={e.strong ? "updates-effect-strong" : undefined}>{e.effect}</span>
            {e.sub ? <span className="updates-summary">{e.sub}</span> : null}
          </div>
        );
      },
    },
  ];
  return <Table columns={columns} rows={rows} rowKey="key" caption={caption} />;
}

/** Whether the server updates itself, and how fresh its lists are. */
function HowItUpdates({ updates }: { updates: ServerUpdates }) {
  const { t } = useLingui();
  const words = useTimeWords();
  const now = useControlPlaneNow();
  const auto = updates.automatic;
  const tool = auto?.tool ?? "";
  return (
    <Panel title={t`Automatic updates`}>
      <dl className="facts">
        <dt>{t`By itself`}</dt>
        <dd>
          {tool ? (
            auto?.on ? (
              <Trans>
                <code>{tool}</code> installs updates
              </Trans>
            ) : (
              <Trans>
                Nothing: <code>{tool}</code> is off
              </Trans>
            )
          ) : (
            t`Nothing installs updates`
          )}
        </dd>
        <dt>{t`Package lists`}</dt>
        <dd>
          {updates.listsTime ? t`Refreshed ${words.ago(timestampDate(updates.listsTime), now)}` : t`Never refreshed`}
          <span className="updates-dd-sub">{t`Dockwell refreshes them once they're a day old, when someone looks`}</span>
        </dd>
        <dt>{t`Reboot`}</dt>
        <dd>{updates.rebootRequired ? t`Needed now` : t`Not needed`}</dd>
        <dt>{t`Package manager`}</dt>
        <dd>
          <code>{managerName(updates.manager)}</code>
        </dd>
      </dl>
    </Panel>
  );
}

/** Docker's engine as the package manager has it, and Dockwell against its newest release. */
function DockerAndDockwell({
  server,
  updates,
  summary,
  release,
}: {
  server: Server;
  updates: ServerUpdates;
  summary: UpdatesSummary;
  release: DockwellRelease | undefined;
}) {
  const { t } = useLingui();
  const docker = summary.docker;
  const installed = server.inventory?.dockerVersion ?? "";
  const agent = plainVersion(release?.agent ?? "");
  const control = plainVersion(release?.controlPlane ?? "");
  const standing = releaseStanding(release);
  const newest = plainVersion(release?.newest ?? "");
  return (
    <Panel title={t`Docker and Dockwell`} flush>
      <ul className="updates-versions">
        <li className="updates-versions-row">
          <div className="updates-versions-text">
            <span className="updates-versions-name">{t`Docker Engine`}</span>
            {docker ? (
              <>
                <span className="updates-versions-mono">
                  <span className="updates-installed">{installed || engineVersion(docker.installed)}</span>
                  <span className="updates-arrow" aria-hidden="true">
                    {" "}
                    →{" "}
                  </span>
                  <span className="updates-versions-new">{engineVersion(docker.available)}</span>{" "}
                  <span className="updates-versions-words">{t`available`}</span>
                </span>
                <span className="updates-summary">
                  {updates.dockerLiveRestore
                    ? t`Containers keep running while it restarts`
                    : t`Containers stop with it, and only those set to restart start again`}
                </span>
              </>
            ) : (
              <span className="updates-summary">
                {installed ? <code>{installed}</code> : t`Not installed or not running`}
                {installed ? ` · ${t`no update waits`}` : ""}
              </span>
            )}
          </div>
        </li>
        <li className="updates-versions-row">
          <div className="updates-versions-text">
            <span className="updates-versions-name">Dockwell</span>
            <span className="updates-summary">
              {agent && control && agent !== control ? (
                <Trans>
                  Agent <code>{agent}</code>, control plane <code>{control}</code>
                </Trans>
              ) : (
                <Trans>
                  <code>{control || agent}</code> · agent and control plane
                </Trans>
              )}
            </span>
            {standing === "behind" ? <UpdateDockwell release={release} /> : null}
            {standing === "unknown" ? (
              <span className="updates-summary">{t`Its newest release isn't known`}</span>
            ) : null}
          </div>
          {standing === "current" ? <StatusBadge status="healthy" label={t`Current`} labelIsState /> : null}
          {standing === "behind" ? <StatusBadge status="degraded" label={t`${newest} available`} labelIsState /> : null}
        </li>
      </ul>
    </Panel>
  );
}

/**
 * How to update Dockwell to its newest release: that release's own install script,
 * from where this install's releases are, a mirror included, run as root on the
 * control server, which updates it in place. On one line, as a command.
 */
function UpdateDockwell({ release }: { release: DockwellRelease | undefined }) {
  const { t } = useLingui();
  const command = installCommand(release?.releases ?? "", release?.newest ?? "");
  if (!command) return null;
  return (
    <span className="updates-summary updates-update">
      <Trans>Update it on the control server, as root, which keeps what runs:</Trans>
      <span className="updates-command-row">
        <code className="updates-command">{command}</code>
        <CopyButton text={command} label={t`Copy the command`} />
      </span>
    </span>
  );
}

/** Docker's version from its package's, "5:27.4.0-1~ubuntu.24.04~noble" as "27.4.0". */
function engineVersion(v: string): string {
  return v.replace(/^\d+:/, "").split("-")[0] ?? v;
}
