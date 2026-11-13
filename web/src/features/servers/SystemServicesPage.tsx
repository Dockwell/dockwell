// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate } from "@bufbuild/protobuf/wkt";
import {
  Connection,
  type Server,
  SystemInit,
  SystemServiceAction,
  SystemServiceGuard,
  type SystemServiceInfo,
  type SystemServiceList,
  SystemServiceState,
} from "@dockwell/api";
import {
  Advanced,
  Button,
  Callout,
  PHONE_QUERY,
  SegmentedControl,
  Sheet,
  StatusBadge,
  Table,
  type TableColumn,
  Tag,
  TextField,
  useMediaQuery,
} from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { useDeferredValue, useMemo, useState } from "react";

import { sentence } from "../../lib/errors";
import { formatBytes } from "../../lib/format";
import { useTimeWords } from "../../lib/time";
import { AgentOutdated, isAgentOutdated } from "./AgentOutdated";
import { ServerFrame } from "./ServerFrame";
import { SystemLog } from "./SystemLog";
import {
  inView,
  isMasked,
  isRunning,
  matchesService,
  primaryAction,
  STATE,
  type SystemView,
  startsAtBoot,
  startsWhenNeeded,
  type UnitActions,
  useSystemServices,
  useUnitActions,
  whyLine,
} from "./systemServices";
import { useControlPlaneNow } from "./useFleet";
import "./ServerPanels.css";
import "./ProcessesPage.css";
import "./SystemServicesPage.css";

/**
 * A server's system services (ADR 0067): its systemd units or OpenRC services,
 * failed first, then running ones, with a filter and a search. Each row has its
 * likely action, and a unit's sheet every action, its facts and its journal
 * (ADR 0082). An action is a plan, applied from the plan sheet.
 */
export function SystemServicesPage({ serverId }: { serverId: string }) {
  return <ServerFrame id={serverId}>{(server) => <SystemServices server={server} />}</ServerFrame>;
}

function SystemServices({ server }: { server: Server }) {
  const { t } = useLingui();
  const phone = useMediaQuery(PHONE_QUERY);
  const connected = server.connection === Connection.CONNECTED;
  const { list, error } = useSystemServices(server.id, connected);
  const [view, setView] = useState<SystemView>("all");
  const [query, setQuery] = useState("");
  const filter = useDeferredValue(query);
  const [open, setOpen] = useState<string>();
  const acts = useUnitActions(server.id);
  const all = list?.services ?? [];
  const failed = all.filter((s) => s.state === SystemServiceState.FAILED).length;
  const running = all.filter((s) => s.state === SystemServiceState.RUNNING).length;
  const shown = useMemo(() => all.filter((s) => inView(s, view) && matchesService(s, filter)), [all, view, filter]);
  const opened = all.find((s) => s.name === open);

  const views = [
    { value: "all" as const, label: t({ message: "All", context: "system services" }), count: all.length },
    { value: "failed" as const, label: t({ message: "Failed", context: "system services" }), count: failed },
    { value: "running" as const, label: t({ message: "Running", context: "system services" }), count: running },
  ];

  const failure = !error ? null : isAgentOutdated(error) ? (
    <AgentOutdated
      server={server}
    >{t`It's older than the control plane, so it can't list system services.`}</AgentOutdated>
  ) : (
    <p className="procs-note">{sentence(error)}</p>
  );
  // What shows when no row does: nothing typed in Failed is a server with nothing failed.
  const none =
    filter.trim() === "" && view === "failed"
      ? t`Nothing has failed on ${server.name}.`
      : filter.trim() === ""
        ? t`${server.name} lists no system services here.`
        : t`No system service matches.`;

  let body: React.ReactNode;
  if (!connected) {
    body = (
      <p className="procs-note" role="status">
        {t`The agent on ${server.name} isn't connected, so its system services can't be read now. This page reads again when it reconnects.`}
      </p>
    );
  } else if (!list) {
    body = failure ?? <p className="procs-note">{t`Reading…`}</p>;
  } else if (list.init === SystemInit.OTHER) {
    body = (
      <p className="procs-note" role="status">
        {t`${server.name} starts its services with an init Dockwell doesn't read. It reads systemd's and OpenRC's.`}
      </p>
    );
  } else if (phone) {
    body = <PhoneRows rows={shown} onOpen={setOpen} none={none} acts={acts} />;
  } else {
    body = (
      <ServiceTable
        rows={shown}
        onOpen={setOpen}
        caption={t`System services on ${server.name}`}
        none={none}
        server={server}
        list={list}
        acts={acts}
      />
    );
  }

  return (
    <section className="sys" aria-labelledby="sys-title">
      <h2 className="dw-sr" id="sys-title">
        {t`System services`}
      </h2>
      <div className="procs-bar">
        <SegmentedControl options={views} value={view} onChange={setView} label={t`Show`} size="sm" />
        <span className="procs-spacer" />
        {connected && list && !error ? (
          <span className="procs-live">
            <span className="dw-live">
              <span className="dw-live-dot" />
              <Trans>Live</Trans>
            </span>
            <Trans>every 10 s</Trans>
          </span>
        ) : null}
        <div className="procs-filter">
          <TextField
            size="sm"
            icon="search"
            placeholder={t`Filter by name or description`}
            aria-label={t`Filter system services`}
            value={query}
            onChange={(event) => setQuery(event.target.value)}
          />
        </div>
      </div>
      {list ? failure : null}
      {body}
      {opened && list ? (
        <ServiceSheet server={server} service={opened} list={list} acts={acts} onClose={() => setOpen(undefined)} />
      ) : null}
    </section>
  );
}

/** The line under a service's state: since when, and for a failed one, with what. */
function useStateLine() {
  const { t } = useLingui();
  const time = useTimeWords();
  const now = useControlPlaneNow(30_000);
  return (s: SystemServiceInfo): string => {
    const since = s.since ? time.ago(timestampDate(s.since), now) : "";
    if (s.state === SystemServiceState.FAILED) {
      const why = s.result ? (s.exitStatus ? t`${s.result}, status ${s.exitStatus}` : s.result) : "";
      return [since, why].filter(Boolean).join(" · ");
    }
    return since;
  };
}

/**
 * Whether a unit starts at boot, in words for the common states, and the init's
 * own word for the rest, as indirect or generated; the sheet says the word too.
 */
function useAtBoot() {
  const { t } = useLingui();
  return (word: string): string => {
    switch (word) {
      case "enabled":
      case "enabled-runtime":
        return t({ message: "Starts", context: "at boot" });
      case "disabled":
        return t({ message: "Doesn't start", context: "at boot" });
      case "static":
        return t({ message: "When needed", context: "at boot" });
      case "masked":
      case "masked-runtime":
        return t({ message: "Masked", context: "at boot" });
      case "":
        return "–";
      default:
        return word;
    }
  };
}

function State({ service, acts }: { service: SystemServiceInfo; acts?: UnitActions }) {
  const { i18n } = useLingui();
  const line = useStateLine()(service);
  const doing = useDoing();
  const state = STATE[service.state];
  const act = acts?.act?.name === service.name && acts.act.phase === "applying" ? acts.act : undefined;
  return (
    <div className="dw-cell procs-cell">
      {act ? (
        <StatusBadge status="deploying" label={doing(act.action)} labelIsState />
      ) : (
        <StatusBadge status={state.status} label={i18n._(state.word)} labelIsState />
      )}
      {line ? <span className="procs-sub">{line}</span> : null}
    </div>
  );
}

/** What an action under way reads as, on its unit's row: "Restarting…". */
function useDoing() {
  const { t } = useLingui();
  return (a: SystemServiceAction): string => {
    switch (a) {
      case SystemServiceAction.START:
        return t`Starting…`;
      case SystemServiceAction.STOP:
        return t`Stopping…`;
      case SystemServiceAction.RESTART:
        return t`Restarting…`;
      case SystemServiceAction.RELOAD:
        return t`Reloading…`;
      case SystemServiceAction.ENABLE:
        return t`Enabling…`;
      case SystemServiceAction.DISABLE:
        return t`Disabling…`;
      case SystemServiceAction.MASK:
        return t`Masking…`;
      case SystemServiceAction.UNMASK:
        return t`Unmasking…`;
    }
    return t`Applying…`;
  };
}

/** An action's button label, as its verb. */
function useVerb() {
  const { t } = useLingui();
  return (a: SystemServiceAction): string => {
    switch (a) {
      case SystemServiceAction.START:
        return t({ message: "Start", context: "system service" });
      case SystemServiceAction.STOP:
        return t({ message: "Stop", context: "system service" });
      case SystemServiceAction.RESTART:
        return t({ message: "Restart", context: "system service" });
      case SystemServiceAction.RELOAD:
        return t({ message: "Reload", context: "system service" });
      case SystemServiceAction.ENABLE:
        return t`Start at boot`;
      case SystemServiceAction.DISABLE:
        return t`Don't start at boot`;
      case SystemServiceAction.MASK:
        return t({ message: "Mask", context: "system service" });
      case SystemServiceAction.UNMASK:
        return t({ message: "Unmask", context: "system service" });
    }
    return "";
  };
}

/**
 * Why an action on a unit is off, in one line, or nothing: what Dockwell needs to
 * reach the server or run its apps, what the init refuses, an older agent, or
 * another action applying there (ADR 0082). The server refuses the same.
 */
function useWhyNot(server: Server, list: SystemServiceList | undefined, acts: UnitActions) {
  const { t } = useLingui();
  const name = server.name;
  return (s: SystemServiceInfo, a: SystemServiceAction): string | undefined => {
    if (list && !list.acts) return t`Its agent is older than the control plane, so it can't act on system services`;
    const cuts = a === SystemServiceAction.STOP || a === SystemServiceAction.DISABLE || a === SystemServiceAction.MASK;
    switch (s.guard) {
      case SystemServiceGuard.DOCKWELL:
        return t`Dockwell runs as this service`;
      case SystemServiceGuard.DOCKER:
        if (cuts) return t`Every app on ${name} runs in Docker`;
        break;
      case SystemServiceGuard.SSH:
        if (cuts) return t`It's how you reach ${name} over SSH`;
        break;
      case SystemServiceGuard.NETWORK:
        if (cuts) return t`${name} reaches the network through it`;
        break;
    }
    const starts = a === SystemServiceAction.START || a === SystemServiceAction.RESTART;
    if (starts && s.startRefused) return t`It starts when another unit needs it`;
    if (a === SystemServiceAction.STOP && s.stopRefused) return t`It stops with the unit that needs it`;
    if ((a === SystemServiceAction.ENABLE || a === SystemServiceAction.DISABLE) && startsWhenNeeded(s))
      return t`It starts when another unit needs it`;
    if (acts.busy && acts.act?.name !== s.name) {
      const other = acts.act?.name ?? "";
      return t`Waits for the action on ${other} to end`;
    }
    return undefined;
  };
}

/** A unit's one likely action, on its row. */
function RowAction({
  service,
  server,
  list,
  acts,
}: {
  service: SystemServiceInfo;
  server: Server;
  list: SystemServiceList | undefined;
  acts: UnitActions;
}) {
  const verb = useVerb();
  const whyNot = useWhyNot(server, list, acts);
  const { t } = useLingui();
  const failed = acts.failure?.name === service.name ? acts.failure : undefined;
  const action = failed?.action ?? primaryAction(service);
  const mine = acts.act?.name === service.name;
  const label = failed ? t({ message: "Retry", context: "system service" }) : verb(action);
  // A failed unit's action, and one under way or that failed, stays in view; the rest show on the row's hover or focus.
  const shown = failed !== undefined || mine || service.state === SystemServiceState.FAILED;
  return (
    <span className="sys-act" data-shown={shown ? "" : undefined}>
      <Button
        size="sm"
        variant="secondary"
        loading={mine}
        off={whyNot(service, action)}
        aria-label={`${label} ${service.name}`}
        onClick={(event) => {
          event.stopPropagation();
          acts.plan(service.name, action);
        }}
      >
        {label}
      </Button>
    </span>
  );
}

/**
 * A unit's name and what it says it is, and for one that failed, the line its own
 * process wrote that says why, or what an action on it said as it failed.
 */
function Name({ service, acts }: { service: SystemServiceInfo; acts?: UnitActions }) {
  const { t } = useLingui();
  const failure = acts?.failure?.name === service.name ? acts.failure.why : "";
  const why = failure || (service.state === SystemServiceState.FAILED ? whyLine(service.why) : "");
  return (
    <div className="dw-cell procs-cell">
      <span className="sys-name">
        <span className="procs-name">{service.name}</span>
        {service.needed ? <Tag words>{t`Dockwell needs it`}</Tag> : null}
      </span>
      {service.description ? <span className="procs-sub">{service.description}</span> : null}
      {why ? (
        <span className={failure ? "sys-why-line sys-failed" : "sys-why-line"} title={why}>
          {why}
        </span>
      ) : null}
    </div>
  );
}

function ServiceTable({
  rows,
  onOpen,
  caption,
  none,
  server,
  list,
  acts,
}: {
  rows: readonly SystemServiceInfo[];
  onOpen: (name: string) => void;
  caption: string;
  none: string;
  server: Server;
  list: SystemServiceList | undefined;
  acts: UnitActions;
}) {
  const { t, i18n } = useLingui();
  const atBoot = useAtBoot();
  const columns: TableColumn<SystemServiceInfo>[] = [
    { key: "name", header: t`Service`, render: (s) => <Name service={s} acts={acts} /> },
    { key: "state", header: t`State`, width: 220, render: (s) => <State service={s} acts={acts} /> },
    { key: "atBoot", header: t`At boot`, width: 112, muted: true, render: (s) => atBoot(s.atBoot) },
    {
      key: "memory",
      header: t`Memory`,
      width: 96,
      align: "right",
      render: (s) => (s.memoryBytes !== undefined ? formatBytes(s.memoryBytes, i18n.locale) : "–"),
    },
    { key: "pid", header: t`PID`, width: 84, align: "right", mono: true, muted: true, render: (s) => s.mainPid || "–" },
    {
      key: "action",
      header: <span className="dw-sr">{t`Action`}</span>,
      width: 120,
      align: "right",
      render: (s) => <RowAction service={s} server={server} list={list} acts={acts} />,
    },
  ];
  return (
    <Table
      className="procs-table"
      columns={columns}
      rows={rows}
      rowKey="name"
      onRowClick={(s) => onOpen(s.name)}
      caption={caption}
      empty={<p className="procs-note">{none}</p>}
    />
  );
}

function PhoneRows({
  rows,
  onOpen,
  none,
  acts,
}: {
  rows: readonly SystemServiceInfo[];
  onOpen: (name: string) => void;
  none: string;
  acts: UnitActions;
}) {
  const { t } = useLingui();
  if (rows.length === 0) return <p className="procs-note">{none}</p>;
  return (
    <ul className="procs-rows" aria-label={t`System services`}>
      {rows.map((s) => (
        <li key={s.name} className="procs-row">
          <button type="button" className="sys-row" onClick={() => onOpen(s.name)}>
            <Name service={s} acts={acts} />
            <State service={s} acts={acts} />
          </button>
        </li>
      ))}
    </ul>
  );
}

/**
 * A unit's sheet: why it failed, with Restart; its actions, each off with why where
 * it can't be taken; its facts; and its journal (ADR 0082).
 */
function ServiceSheet({
  server,
  service,
  list,
  acts,
  onClose,
}: {
  server: Server;
  service: SystemServiceInfo;
  list: SystemServiceList;
  acts: UnitActions;
  onClose: () => void;
}) {
  const { t, i18n } = useLingui();
  const line = useStateLine()(service);
  const atBoot = useAtBoot();
  const verb = useVerb();
  const whyNot = useWhyNot(server, list, acts);
  const time = useTimeWords();
  const now = useControlPlaneNow(30_000);
  const state = STATE[service.state];
  const openrc = list.init === SystemInit.OPENRC;
  const masked = isMasked(service);
  const running = isRunning(service);
  const failed = service.state === SystemServiceState.FAILED;
  const mine = (a: SystemServiceAction) => acts.act?.name === service.name && acts.act.action === a;
  const button = (a: SystemServiceAction, variant: "primary" | "secondary" = "secondary") => (
    <Button
      key={a}
      size="sm"
      variant={variant}
      loading={mine(a)}
      off={whyNot(service, a)}
      onClick={() => acts.plan(service.name, a)}
    >
      {verb(a)}
    </Button>
  );
  const primary = primaryAction(service);
  const actions: SystemServiceAction[] = [];
  if (!masked && running) {
    actions.push(SystemServiceAction.STOP);
    if (service.canReload && !openrc) actions.push(SystemServiceAction.RELOAD);
  }
  const boot =
    masked || startsWhenNeeded(service)
      ? undefined
      : startsAtBoot(service, list.init)
        ? SystemServiceAction.DISABLE
        : SystemServiceAction.ENABLE;
  const failure = acts.failure?.name === service.name ? acts.failure : undefined;
  const failedAgo = service.since ? time.ago(timestampDate(service.since), now) : "";
  const why = whyLine(service.why);
  return (
    <Sheet open title={service.name} subtitle={t`On ${server.name}`} onClose={onClose}>
      <div className="sys-sheet">
        {failure ? (
          <Callout
            tone="critical"
            title={t`It didn't work`}
            actions={
              <Button size="sm" variant="ghost" onClick={() => acts.open(failure.changeId)}>
                <Trans>Open the change</Trans>
              </Button>
            }
          >
            <code className="sys-why">{failure.why}</code>
          </Callout>
        ) : failed ? (
          <Callout
            tone="critical"
            title={failedAgo ? t`Failed ${failedAgo}` : t`Failed`}
            actions={button(SystemServiceAction.RESTART, "primary")}
          >
            {why ? <code className="sys-why">{why}</code> : service.result ? <code>{service.result}</code> : null}
          </Callout>
        ) : null}
        {service.description ? <p className="sys-desc">{service.description}</p> : null}
        <div className="sys-actions">
          {failed && !failure ? null : button(primary, "primary")}
          {actions.map((a) => button(a))}
          {boot !== undefined ? button(boot) : null}
        </div>
        <dl className="facts">
          <dt>{t`State`}</dt>
          <dd>
            <StatusBadge status={state.status} label={i18n._(state.word)} labelIsState />
            {line ? <span className="procs-sub"> {line}</span> : null}
          </dd>
          {service.detail ? (
            <>
              <dt>{t`Init state`}</dt>
              <dd>
                <code>{service.detail}</code>
              </dd>
            </>
          ) : null}
          <dt>{t`At boot`}</dt>
          <dd>
            {service.atBoot ? (
              <>
                {atBoot(service.atBoot)} <code>{service.atBoot}</code>
              </>
            ) : (
              t`Not started at boot`
            )}
          </dd>
          {service.mainPid ? (
            <>
              <dt>{t`Main process`}</dt>
              <dd>
                <code>{service.mainPid}</code>
              </dd>
            </>
          ) : null}
          {service.memoryBytes !== undefined ? (
            <>
              <dt>{t`Memory`}</dt>
              <dd>{formatBytes(service.memoryBytes, i18n.locale)}</dd>
            </>
          ) : null}
          {service.needed ? (
            <>
              <dt>{t`Dockwell`}</dt>
              <dd>{t`Needs it on ${server.name}`}</dd>
            </>
          ) : null}
        </dl>
        {list.acts ? <SystemLog serverId={server.id} server={server.name} unit={service.name} /> : null}
        {openrc ? null : (
          <Advanced
            label={t({ message: "More", context: "system service actions" })}
            summary={masked ? t`Masked: nothing starts it` : t`Mask, so nothing starts it`}
          >
            <div className="sys-actions">
              {masked ? button(SystemServiceAction.UNMASK) : button(SystemServiceAction.MASK)}
            </div>
          </Advanced>
        )}
      </div>
    </Sheet>
  );
}
