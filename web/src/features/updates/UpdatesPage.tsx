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
import { type ReactNode, useEffect, useId, useMemo, useRef, useState } from "react";

import { sentence } from "../../lib/errors";
import { useTimeWords } from "../../lib/time";
import { useMay } from "../auth/may";
import { CopyButton } from "../servers/CopyButton";
import { ServerFrame } from "../servers/ServerFrame";
import { useControlPlaneNow } from "../servers/useFleet";
import { ReadFailed, RoleRefused } from "../shell/ReadFailed";
import { CopyButton as CopyAction } from "../system/CopyCommand";
import { type Install, type InstallAsk, type RowState, useInstall } from "./install";
import {
  installCommand,
  isDockerEngine,
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
  // A server's updates are an admin's to read. Someone else who opens this address is told so in
  // the words a refused read gets, and nothing is asked on their behalf (ADR 0059).
  const allowed = useMay()(UpdateService.method.getServerUpdates).allowed;
  const query = useServerUpdates(server.id, { ask: allowed });
  const transport = useTransport();
  const client = useMemo(() => createClient(UpdateService, transport), [transport]);
  const whyNot = useWhyNot();
  const [asked, setAsked] = useState<unknown>();
  // From the press until the control plane answers, as it may answer only once it has read.
  const [pressed, setPressed] = useState(false);
  const updates = query.data?.updates;
  const summary = summarize(updates);
  const install = useInstall(server, updates, () => void query.refetch());
  const selection = useSelection(updates, install);
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
  if (!allowed) return <RoleRefused />;
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
  const name = server.name;
  const checkButton = (
    <Button
      variant="ghost"
      size="sm"
      icon="refresh"
      loading={checking}
      onClick={() => void check()}
      // An install moves what's read: it's read again once it ends.
      off={install.busy ? t`Updates are read again once the install on ${name} ends` : undefined}
    >
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
  const rows: RowProps = { server, updates, install, selection };
  return (
    <div className="updates">
      <div className="updates-layout">
        <div className="updates-main">
          {why ? (
            <WhyNotCallout why={why} />
          ) : (
            <Lead server={server} updates={updates} summary={summary} install={install} />
          )}
          {asked ? <p className="updates-note updates-note-warning">{sentence(asked)}</p> : null}
          {updates.marksSecurity ? (
            <>
              <SecuritySection rows={rows} summary={summary} action={checkButton} />
              {summary.other.length > 0 ||
              leavingIn(install, (p) => !p.security && !isDockerEngine(p.name)).length > 0 ? (
                <OtherUpdates rows={rows} summary={summary} />
              ) : null}
            </>
          ) : (
            <AllSection rows={rows} summary={summary} action={checkButton} />
          )}
          <Held updates={updates} />
          <SelectionBar server={server} install={install} selection={selection} />
        </div>
        <div className="updates-side">
          <DockerAndDockwell server={server} updates={updates} summary={summary} release={release} install={install} />
          <HowItUpdates updates={updates} />
        </div>
      </div>
    </div>
  );
}

/** What every table of updates is drawn from. */
interface RowProps {
  server: Server;
  updates: ServerUpdates;
  install: Install;
  selection: Selection;
}

/** The packages that left the read but still say what they went to, of a section. */
function leavingIn(install: Install, of: (p: PackageUpdate) => boolean): PackageUpdate[] {
  return install.leaving.map((r) => r.pkg).filter(of);
}

/** A section's packages, with those that just left it still in their place for a moment. */
function withLeaving(packages: PackageUpdate[], install: Install, of: (p: PackageUpdate) => boolean): PackageUpdate[] {
  const left = leavingIn(install, of);
  if (left.length === 0) return packages;
  return [...packages, ...left].sort((a, b) => a.name.localeCompare(b.name));
}

/** Which packages are picked for "Update n selected", and the last one picked, for a range with shift. */
function useSelection(updates: ServerUpdates | undefined, install: Install) {
  const [picked, setPicked] = useState<ReadonlySet<string>>(new Set());
  const anchor = useRef<string>(undefined);
  const waiting = useMemo(() => new Set((updates?.packages ?? []).map((p) => p.name)), [updates?.packages]);
  // What isn't waiting any more, or has an install of its own under way, can't stay picked.
  const selected = useMemo(
    () => new Set([...picked].filter((n) => waiting.has(n) && pickable(install.stateOf(n)))),
    [picked, waiting, install],
  );
  return {
    selected,
    /** Picks or lets go of name, or with shift everything from the last one picked to it, in order. */
    toggle(order: string[], name: string, on: boolean, range: boolean) {
      const from = anchor.current ? order.indexOf(anchor.current) : -1;
      const to = order.indexOf(name);
      const names = range && from >= 0 && to >= 0 ? order.slice(Math.min(from, to), Math.max(from, to) + 1) : [name];
      setPicked((was) => {
        const next = new Set(was);
        for (const n of names) {
          if (on) next.add(n);
          else next.delete(n);
        }
        return next;
      });
      anchor.current = name;
    },
    setAll(names: string[], on: boolean) {
      setPicked((was) => {
        const next = new Set(was);
        for (const n of names) {
          if (on) next.add(n);
          else next.delete(n);
        }
        return next;
      });
    },
    clear() {
      setPicked(new Set());
      anchor.current = undefined;
    },
  };
}

type Selection = ReturnType<typeof useSelection>;

/** Whether a package in this state can be picked: one nothing is doing anything with. */
function pickable(s: RowState): boolean {
  return s.kind === "idle" || s.kind === "failed";
}

/** The one callout the tab leads with: what's most worth doing, or why what's here can't stand. */
function Lead({
  server,
  updates,
  summary,
  install,
}: {
  server: Server;
  updates: ServerUpdates;
  summary: UpdatesSummary;
  install: Install;
}) {
  const { t } = useLingui();
  const command = upgradeCommand(updates.manager);
  const auto = updates.automatic;
  const tool = auto?.on ? auto.tool : "";
  const name = server.name;
  if (install.busy) return <Installing server={server} install={install} />;
  const failure = install.failure;
  // A failure of one to three packages is said on their rows; of more, here, once.
  if (failure && failure.names.size > 3) {
    return (
      <Callout
        tone="critical"
        title={t`Installing updates on ${name} failed`}
        actions={
          <>
            <Button size="sm" onClick={() => install.plan(failure.ask)} {...install.may.off}>
              <Trans>Retry</Trans>
            </Button>
            <Button size="sm" variant="ghost" onClick={() => install.open(failure.id)}>
              <Trans>Details</Trans>
            </Button>
          </>
        }
      >
        <span className="updates-detail">{failure.why}</span>
      </Callout>
    );
  }
  if (updates.rebootRequired) {
    const what = updates.rebootPackages.join(", ");
    return (
      <Callout
        tone="warning"
        title={t`${name} needs a reboot`}
        actions={
          <CopyAction
            text="sudo reboot"
            label={t`Copy the reboot command`}
            copiedLabel={t`Reboot command copied`}
            variant="secondary"
          />
        }
      >
        {what
          ? t`What ${what} changed takes effect once it restarts; apps and websites go offline meanwhile.`
          : t`What was installed takes effect once it restarts; apps and websites go offline meanwhile.`}
      </Callout>
    );
  }
  const planning = install.planning;
  const plansAll = planning !== undefined && "all" in planning && planning.all;
  const plansSecurity = planning !== undefined && "all" in planning && !planning.all;
  // While one plan is made, the others wait for it, as the sheet shows one plan at a time.
  const offFor = (mine: boolean) => (planning !== undefined && !mine ? { disabled: true as const } : install.may.off);
  if (summary.security.length > 0) {
    const n = summary.security.length;
    const title = plural(n, {
      one: "# security update is ready to install",
      other: "# security updates are ready to install",
    });
    const reboots = summary.security.filter((p) => p.effect === PackageEffect.REBOOT).length;
    const rebooting =
      reboots === 0
        ? ""
        : n === 1
          ? t`It needs a reboot, and apps and websites go offline while ${name} restarts.`
          : plural(reboots, {
              one: `One needs a reboot, and apps and websites go offline while ${name} restarts.`,
              other: `# need a reboot, and apps and websites go offline while ${name} restarts.`,
            });
    if (install.offered) {
      return (
        <Callout
          tone="warning"
          title={title}
          actions={
            <>
              <Button
                size="sm"
                variant="primary"
                loading={plansSecurity}
                onClick={() => install.plan({ all: false })}
                {...offFor(plansSecurity)}
              >
                <Trans>Install security updates</Trans>
              </Button>
              <Button size="sm" loading={plansAll} onClick={() => install.plan({ all: true })} {...offFor(plansAll)}>
                <Trans>Install every update</Trans>
              </Button>
            </>
          }
        >
          {/* The plan says what it installs and that it takes a passkey; the lead says only what's urgent. */}
          {rebooting ? `${rebooting} ` : null}
          {tool ? (
            <Trans>
              <code>{tool}</code> installs them within about a day.
            </Trans>
          ) : null}
        </Callout>
      );
    }
    const copy = command ? (
      <CopyAction
        text={command}
        label={t`Copy the upgrade command`}
        copiedLabel={t`Upgrade command copied`}
        variant="secondary"
      />
    ) : undefined;
    return (
      <Callout tone="warning" title={title} actions={copy}>
        {rebooting ? `${rebooting} ` : null}
        {tool ? (
          command ? (
            <Trans>
              <code>{tool}</code> installs them within about a day; the command installs every update waiting now.
            </Trans>
          ) : (
            <Trans>
              <code>{tool}</code> installs them within about a day.
            </Trans>
          )
        ) : command ? (
          t`Dockwell installs updates with apt for now; the command installs every update waiting, not only these.`
        ) : (
          t`Dockwell installs updates with apt for now, so install these with ${name}'s own package manager.`
        )}
      </Callout>
    );
  }
  // Updates wait, none of them security fixes: installing them is one step away, quietly.
  if (install.offered && summary.other.length > 0) {
    const n = summary.other.length;
    return (
      <Callout
        tone="info"
        title={plural(n, { one: "# update is ready to install", other: "# updates are ready to install" })}
        actions={
          <Button size="sm" loading={plansAll} onClick={() => install.plan({ all: true })} {...offFor(plansAll)}>
            <Trans>Install every update</Trans>
          </Button>
        }
      />
    );
  }
  return null;
}

/** An install under way on the server: what it installs, where it is, and the sheet one step away. */
function Installing({ server, install }: { server: Server; install: Install }) {
  const { t } = useLingui();
  const name = server.name;
  const n = install.install?.packages.length ?? 0;
  const id = install.change?.id;
  const title =
    n > 0
      ? plural(n, { one: `Installing # package on ${name}`, other: `Installing # packages on ${name}` })
      : t`Installing updates on ${name}`;
  const where = install.change
    ? install.downloading
      ? t`Downloading the packages`
      : install.activity || t`Waiting for what else runs on ${name} to end`
    : t`Started from somewhere else`;
  return (
    <Callout
      tone="info"
      shape="spinner"
      title={title}
      actions={
        id ? (
          <Button size="sm" variant="ghost" onClick={() => install.open(id)}>
            <Trans>Open</Trans>
          </Button>
        ) : undefined
      }
    >
      <span className="updates-activity" aria-live="polite">
        {where}
      </span>
    </Callout>
  );
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

function SecuritySection({ rows, summary, action }: { rows: RowProps; summary: UpdatesSummary; action: ReactNode }) {
  const { t } = useLingui();
  const { server, updates, install } = rows;
  const checked = useListsAgo(updates);
  const n = summary.security.length;
  const from = summary.securityFrom;
  const source = from.length === 1 ? from[0] : "";
  const meta = n === 0 ? checked : source ? `${t`${n} from ${source}`} · ${checked}` : `${n} · ${checked}`;
  const packages = withLeaving(summary.security, install, (p) => p.security && !isDockerEngine(p.name));
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
      {packages.length > 0 ? (
        <UpdatesTable rows={rows} packages={packages} caption={t`Security updates for ${server.name}`} />
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
function AllSection({ rows, summary, action }: { rows: RowProps; summary: UpdatesSummary; action: ReactNode }) {
  const { t } = useLingui();
  const { server, updates, install } = rows;
  const checked = useListsAgo(updates);
  const all = withLeaving([...summary.security, ...summary.other], install, (p) => !isDockerEngine(p.name));
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
        <UpdatesTable rows={rows} packages={all} caption={t`Updates for ${server.name}`} />
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
function OtherUpdates({ rows, summary }: { rows: RowProps; summary: UpdatesSummary }) {
  const { t } = useLingui();
  const { server, install, selection } = rows;
  const id = useId();
  const others = summary.other;
  const packages = withLeaving(others, install, (p) => !p.security && !isDockerEngine(p.name));
  // Open on its own while something in it is picked or being installed, so its rows say so.
  const active = packages.some((p) => selection.selected.has(p.name) || install.stateOf(p.name).kind !== "idle");
  const [open, setOpen] = useState(active);
  useEffect(() => {
    if (active) setOpen(true);
  }, [active]);
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
            {[fromWords ? `${fromWords}, ${rebootWords}` : rebootWords, names].filter(Boolean).join(" · ")}
          </span>
        </span>
        <span className="updates-other-show">{open ? t`Hide` : t`Show all`}</span>
      </button>
      <div id={id} hidden={!open} className="updates-other-body">
        {open ? <UpdatesTable rows={rows} packages={packages} caption={t`Other updates for ${server.name}`} /> : null}
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

/** What installing a package does beyond replacing its files, as a short state. */
function useEffectWords(updates: ServerUpdates) {
  const { t } = useLingui();
  return (p: PackageUpdate): { effect: string; why: string; strong: boolean } => {
    switch (p.effect) {
      case PackageEffect.REBOOT:
        return { effect: t`Needs a reboot`, why: t`Takes effect once the server restarts`, strong: true };
      case PackageEffect.DOCKER_RESTART:
        return updates.dockerLiveRestore
          ? { effect: t`Restarts Docker`, why: t`Containers keep running`, strong: false }
          : { effect: t`Restarts Docker`, why: t`Containers stop; those set to restart start again`, strong: true };
    }
    return { effect: t`No reboot`, why: "", strong: false };
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

/** The version a package gets, its changed part marked, with what's installed beneath. */
function Versions({ p }: { p: PackageUpdate }) {
  const { t } = useLingui();
  const installed = p.installed;
  return (
    <div className="dw-cell updates-versions-cell">
      <Available p={p} />
      <span className="updates-installed">
        {installed ? (
          <Trans context="the version of a package installed now">
            from <span className="updates-mono">{installed}</span>
          </Trans>
        ) : (
          t({ message: "New", context: "a package not installed yet" })
        )}
      </span>
    </div>
  );
}

/** A package's name, with what it is beneath, or what's happening to it while something is. */
function Name({
  p,
  state,
  rows,
  failedAlone,
}: {
  p: PackageUpdate;
  state: RowState;
  rows: RowProps;
  failedAlone: boolean;
}) {
  const { t } = useLingui();
  const { install } = rows;
  const advisory = [p.advisory, p.severity].filter(Boolean).join(", ");
  let sub: ReactNode = (
    <>
      {p.summary ? <span className="updates-summary">{p.summary}</span> : null}
      {advisory ? <span className="updates-summary">{advisory}</span> : null}
    </>
  );
  if (state.kind === "updated") {
    const to = state.to;
    sub = (
      <span className="updates-summary updates-said-healthy">
        <Trans>
          Updated to <span className="updates-mono">{to}</span>
        </Trans>
      </span>
    );
  } else if (state.kind === "installing" && install.activity.includes(` ${p.name} `)) {
    sub = <span className="updates-summary updates-said-line">{install.activity}</span>;
  } else if (state.kind === "failed" && failedAlone && install.failure) {
    const failure = install.failure;
    sub = (
      <span className="updates-summary updates-said-critical">
        <span className="updates-said-line" title={failure.why}>
          {failure.why}
        </span>{" "}
        <button type="button" className="updates-link" onClick={() => install.open(failure.id)}>
          {t`Details`}
        </button>
      </span>
    );
  }
  return (
    <div className="dw-cell updates-name-cell">
      <span className="updates-name">{p.name}</span>
      {sub}
    </div>
  );
}

/** Where a package is, or its Update button: the row's last cell, the same width in every state. */
function RowAction({ p, state, rows }: { p: PackageUpdate; state: RowState; rows: RowProps }) {
  const { t } = useLingui();
  const { server, install } = rows;
  const rowOff = useRowOff();
  const name = p.name;
  const label = t`Update ${name}`;
  switch (state.kind) {
    case "planning":
      return (
        <Button size="sm" loading aria-label={label}>
          <Trans>Update</Trans>
        </Button>
      );
    case "reviewing":
      return <span className="updates-row-state">{t`In review`}</span>;
    case "waiting":
      return <StatusBadge status="stopped" label={t`Waiting`} labelIsState />;
    case "downloading":
      return <StatusBadge status="deploying" label={t`Downloading`} labelIsState />;
    case "installing":
      return <StatusBadge status="deploying" label={t`Installing`} labelIsState />;
    case "installed":
      return (
        <StatusBadge
          status="healthy"
          label={t({ message: "Installed", context: "a package an install has set up" })}
          labelIsState
        />
      );
    case "updated":
      return (
        <StatusBadge
          status="healthy"
          label={t({ message: "Updated", context: "a package an install took" })}
          labelIsState
        />
      );
    case "failed": {
      const failure = install.failure;
      return (
        <span className="updates-row-failed">
          <StatusBadge status="down" label={t`Failed`} labelIsState />
          {failure ? (
            <Button
              size="sm"
              aria-label={t`Retry updating ${name}`}
              onClick={() => install.plan(failure.ask)}
              {...install.may.off}
            >
              <Trans>Retry</Trans>
            </Button>
          ) : null}
        </span>
      );
    }
  }
  return (
    <Button size="sm" aria-label={label} onClick={() => install.plan({ names: [name] })} {...rowOff(rows, server)}>
      <Trans>Update</Trans>
    </Button>
  );
}

/** Why a row's Update is off: a role that can't, an install under way, or another plan being made. */
function useRowOff() {
  const { t } = useLingui();
  return (rows: RowProps, server: Server): { off?: string; disabled?: true } => {
    if (!rows.install.may.allowed) return rows.install.may.off;
    if (rows.install.busy) {
      const name = server.name;
      return { off: t`An install is running on ${name}. This one can start once it ends.` };
    }
    if (rows.install.planning) return { disabled: true };
    return {};
  };
}

/** A checkbox that can be part-checked, as a column's head is when some of its rows are. */
function Check({
  checked,
  partly = false,
  label,
  onPick,
  disabled,
}: {
  checked: boolean;
  partly?: boolean;
  label: string;
  onPick: (on: boolean, range: boolean) => void;
  disabled?: boolean;
}) {
  const ref = useRef<HTMLInputElement>(null);
  useEffect(() => {
    if (ref.current) ref.current.indeterminate = partly && !checked;
  }, [partly, checked]);
  return (
    <input
      ref={ref}
      type="checkbox"
      className="updates-check"
      checked={checked}
      aria-label={label}
      disabled={disabled}
      onChange={(e) => onPick(e.currentTarget.checked, (e.nativeEvent as MouseEvent).shiftKey === true)}
    />
  );
}

function UpdatesTable({ rows, packages, caption }: { rows: RowProps; packages: PackageUpdate[]; caption: string }) {
  const { t } = useLingui();
  const phone = useMediaQuery(PHONE_QUERY);
  const effectOf = useEffectWords(rows.updates);
  const { install, selection } = rows;
  const picks = install.offered && install.may.allowed;
  const states = new Map(packages.map((p) => [p.name, install.stateOf(p.name)]));
  const failedRows = [...states.values()].filter((s) => s.kind === "failed").length;
  const order = packages.filter((p) => pickable(states.get(p.name) ?? { kind: "idle" })).map((p) => p.name);
  const picked = order.filter((n) => selection.selected.has(n)).length;
  const head = (
    <Check
      checked={order.length > 0 && picked === order.length}
      partly={picked > 0}
      label={t`Select every package in ${caption}`}
      disabled={order.length === 0 || install.busy}
      onPick={(on) => selection.setAll(order, on)}
    />
  );
  const pick = (p: PackageUpdate) => {
    const state = states.get(p.name) ?? { kind: "idle" };
    const name = p.name;
    return (
      <Check
        checked={selection.selected.has(p.name)}
        label={t`Select ${name}`}
        disabled={!pickable(state) || install.busy}
        onPick={(on, range) => selection.toggle(order, p.name, on, range)}
      />
    );
  };
  const when = (p: PackageUpdate) => {
    const e = effectOf(p);
    return (
      <span className={e.strong ? "updates-effect updates-effect-strong" : "updates-effect"} title={e.why || undefined}>
        {e.effect}
      </span>
    );
  };
  const rowClass = (p: PackageUpdate) => {
    const s = states.get(p.name)?.kind ?? "idle";
    return cx("updates-row", s !== "idle" && `is-${s}`, selection.selected.has(p.name) && "is-picked");
  };
  if (phone) {
    return (
      <ul className="dw-panel updates-list" aria-label={caption}>
        {packages.map((p) => {
          const state = states.get(p.name) ?? { kind: "idle" };
          return (
            <li
              key={`${p.name}:${p.architecture}`}
              className={cx("updates-list-row", rowClass(p), picks && "has-pick")}
            >
              {picks ? <span className="updates-list-pick">{pick(p)}</span> : null}
              <span className="updates-list-what">
                <Name p={p} state={state} rows={rows} failedAlone={failedRows <= 3} />
                <Versions p={p} />
                {when(p)}
              </span>
              {install.offered ? (
                <span className="updates-list-action">
                  <RowAction p={p} state={state} rows={rows} />
                </span>
              ) : null}
            </li>
          );
        })}
      </ul>
    );
  }
  type Row = { key: string; p: PackageUpdate };
  const tableRows: Row[] = packages.map((p) => ({ key: `${p.name}:${p.architecture}`, p }));
  const columns: TableColumn<Row>[] = [
    ...(picks ? [{ key: "pick", header: head, width: 40, render: (r: Row) => pick(r.p) }] : []),
    {
      key: "pkg",
      header: t`Package`,
      render: (r) => (
        <Name p={r.p} state={states.get(r.p.name) ?? { kind: "idle" }} rows={rows} failedAlone={failedRows <= 3} />
      ),
    },
    { key: "version", header: t`Version`, width: "28%", render: (r) => <Versions p={r.p} /> },
    { key: "effect", header: t`When installed`, width: 128, render: (r) => when(r.p) },
    ...(install.offered
      ? [
          {
            key: "action",
            header: <span className="dw-sr">{t`Action`}</span>,
            width: 144,
            render: (r: Row) => <RowAction p={r.p} state={states.get(r.p.name) ?? { kind: "idle" }} rows={rows} />,
          },
        ]
      : []),
  ];
  return (
    <Table
      columns={columns}
      rows={tableRows}
      rowKey="key"
      caption={caption}
      className="updates-table"
      rowClassName={(r) => rowClass(r.p)}
    />
  );
}

function cx(...names: (string | false | undefined)[]): string {
  return names.filter(Boolean).join(" ");
}

/** "Update n selected", once anything is picked: at the end of the list, and in reach while it scrolls. */
function SelectionBar({ server, install, selection }: { server: Server; install: Install; selection: Selection }) {
  const { t } = useLingui();
  const n = selection.selected.size;
  if (n === 0 || !install.offered) return null;
  const names = [...selection.selected].sort();
  const planning = install.planning;
  const mine =
    planning !== undefined &&
    "names" in planning &&
    planning.names.length === n &&
    planning.names.every((x) => selection.selected.has(x));
  const name = server.name;
  const off = install.busy
    ? { off: t`An install is running on ${name}. These can start once it ends.` }
    : planning && !mine
      ? { disabled: true as const }
      : install.may.off;
  const ask: InstallAsk = { names };
  return (
    <section className="updates-selection" aria-label={t`Selected updates`}>
      <span className="updates-selection-count">{plural(n, { one: "# selected", other: "# selected" })}</span>
      <span className="updates-grow" />
      <Button size="sm" variant="ghost" onClick={() => selection.clear()}>
        <Trans>Clear</Trans>
      </Button>
      <Button
        size="sm"
        variant="primary"
        loading={mine}
        onClick={() => {
          install.plan(ask);
        }}
        {...off}
      >
        {plural(n, { one: "Update # selected", other: "Update # selected" })}
      </Button>
    </section>
  );
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

/** Docker's engine as the package manager has it, with its own update, and Dockwell against its newest release. */
function DockerAndDockwell({
  server,
  updates,
  summary,
  release,
  install,
}: {
  server: Server;
  updates: ServerUpdates;
  summary: UpdatesSummary;
  release: DockwellRelease | undefined;
  install: Install;
}) {
  const { t } = useLingui();
  const docker = summary.docker;
  const installed = server.inventory?.dockerVersion ?? "";
  const agent = plainVersion(release?.agent ?? "");
  const control = plainVersion(release?.controlPlane ?? "");
  const standing = releaseStanding(release);
  const newest = plainVersion(release?.newest ?? "");
  const engine = docker ? install.stateOf(docker.name) : undefined;
  // Once the read no longer lists it, the engine still says what it went to for a moment.
  const left = install.leaving.find((r) => isDockerEngine(r.pkg.name));
  const to = engine?.kind === "updated" ? engine.to : docker ? "" : (left?.to ?? "");
  const wentTo = to ? engineVersion(to) : "";
  const planning = install.planning;
  const plansDocker =
    docker !== undefined && planning !== undefined && "names" in planning && planning.names.includes(docker.name);
  const name = server.name;
  const off = install.busy
    ? { off: t`An install is running on ${name}. Docker can update once it ends.` }
    : planning && !plansDocker
      ? { disabled: true as const }
      : install.may.off;
  return (
    <Panel title={t`Docker and Dockwell`} flush>
      <ul className="updates-versions">
        <li className="updates-versions-row">
          <div className="updates-versions-text">
            <span className="updates-versions-name">{t`Docker Engine`}</span>
            {wentTo ? (
              <span className="updates-summary updates-said-healthy">
                <Trans>
                  Updated to <span className="updates-mono">{wentTo}</span>
                </Trans>
              </span>
            ) : docker ? (
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
          {docker && install.offered && engine ? (
            engine.kind === "idle" || engine.kind === "planning" ? (
              <Button
                size="sm"
                loading={plansDocker}
                onClick={() => install.plan({ names: summary.dockerNames })}
                {...off}
              >
                <Trans>Update Docker</Trans>
              </Button>
            ) : (
              <RowAction p={docker} state={engine} rows={{ server, updates, install, selection: noSelection }} />
            )
          ) : null}
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

const noSelection: Selection = { selected: new Set(), toggle: () => {}, setAll: () => {}, clear: () => {} };

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
