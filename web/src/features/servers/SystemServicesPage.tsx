// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate } from "@bufbuild/protobuf/wkt";
import { Connection, type Server, SystemInit, type SystemServiceInfo, SystemServiceState } from "@dockwell/api";
import {
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
import { ServerFrame } from "./ServerFrame";
import { inView, matchesService, STATE, type SystemView, useSystemServices } from "./systemServices";
import { useControlPlaneNow } from "./useFleet";
import "./ServerPanels.css";
import "./ProcessesPage.css";
import "./SystemServicesPage.css";

/**
 * A server's system services (ADR 0067): its systemd units or OpenRC services,
 * failed first, then running ones, with a filter and a search, and a unit's facts
 * a row away. Reading only; acting on a unit is a later slice.
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

  let body: React.ReactNode;
  if (!connected) {
    body = (
      <p className="procs-note" role="status">
        {t`The agent on ${server.name} isn't connected, so its system services can't be read now. This page reads again when it reconnects.`}
      </p>
    );
  } else if (!list) {
    body = error ? <p className="procs-note">{sentence(error)}</p> : <p className="procs-note">{t`Reading…`}</p>;
  } else if (list.init === SystemInit.OTHER) {
    body = (
      <p className="procs-note" role="status">
        {t`${server.name} starts its services with an init Dockwell doesn't read. It reads systemd's and OpenRC's.`}
      </p>
    );
  } else if (phone) {
    body = <PhoneRows rows={shown} onOpen={setOpen} />;
  } else {
    body = <ServiceTable rows={shown} onOpen={setOpen} caption={t`System services on ${server.name}`} />;
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
      {list && error ? <p className="procs-note">{sentence(error)}</p> : null}
      {body}
      {opened ? <ServiceSheet server={server} service={opened} onClose={() => setOpen(undefined)} /> : null}
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

function State({ service }: { service: SystemServiceInfo }) {
  const { i18n } = useLingui();
  const line = useStateLine()(service);
  const state = STATE[service.state];
  return (
    <div className="dw-cell procs-cell">
      <StatusBadge status={state.status} label={i18n._(state.word)} labelIsState />
      {line ? <span className="procs-sub">{line}</span> : null}
    </div>
  );
}

function Name({ service }: { service: SystemServiceInfo }) {
  const { t } = useLingui();
  return (
    <div className="dw-cell procs-cell">
      <span className="sys-name">
        <span className="procs-name">{service.name}</span>
        {service.needed ? <Tag words>{t`Dockwell needs it`}</Tag> : null}
      </span>
      {service.description ? <span className="procs-sub">{service.description}</span> : null}
    </div>
  );
}

function ServiceTable({
  rows,
  onOpen,
  caption,
}: {
  rows: readonly SystemServiceInfo[];
  onOpen: (name: string) => void;
  caption: string;
}) {
  const { t, i18n } = useLingui();
  const columns: TableColumn<SystemServiceInfo>[] = [
    { key: "name", header: t`Service`, render: (s) => <Name service={s} /> },
    { key: "state", header: t`State`, width: 220, render: (s) => <State service={s} /> },
    { key: "atBoot", header: t`At boot`, width: 112, muted: true, render: (s) => s.atBoot || "–" },
    {
      key: "memory",
      header: t`Memory`,
      width: 96,
      align: "right",
      render: (s) => (s.memoryBytes !== undefined ? formatBytes(s.memoryBytes, i18n.locale) : "–"),
    },
    { key: "pid", header: t`PID`, width: 84, align: "right", mono: true, muted: true, render: (s) => s.mainPid || "–" },
  ];
  return (
    <Table
      className="procs-table"
      columns={columns}
      rows={rows}
      rowKey="name"
      onRowClick={(s) => onOpen(s.name)}
      caption={caption}
      empty={<p className="procs-note">{t`No system service matches.`}</p>}
    />
  );
}

function PhoneRows({ rows, onOpen }: { rows: readonly SystemServiceInfo[]; onOpen: (name: string) => void }) {
  const { t } = useLingui();
  if (rows.length === 0) return <p className="procs-note">{t`No system service matches.`}</p>;
  return (
    <ul className="procs-rows" aria-label={t`System services`}>
      {rows.map((s) => (
        <li key={s.name} className="procs-row">
          <button type="button" className="sys-row" onClick={() => onOpen(s.name)}>
            <Name service={s} />
            <State service={s} />
          </button>
        </li>
      ))}
    </ul>
  );
}

/** A service's facts, as its agent last read them. */
function ServiceSheet({
  server,
  service,
  onClose,
}: {
  server: Server;
  service: SystemServiceInfo;
  onClose: () => void;
}) {
  const { t, i18n } = useLingui();
  const line = useStateLine()(service);
  const state = STATE[service.state];
  return (
    <Sheet open title={service.name} subtitle={t`On ${server.name}`} onClose={onClose}>
      <div className="sys-sheet">
        {service.description ? <p>{service.description}</p> : null}
        <dl className="facts">
          <dt>{t`State`}</dt>
          <dd>
            <StatusBadge status={state.status} label={i18n._(state.word)} labelIsState />
            {line ? <span className="procs-sub"> {line}</span> : null}
          </dd>
          {service.detail ? (
            <>
              <dt>{t`As its init says`}</dt>
              <dd>
                <code>{service.detail}</code>
              </dd>
            </>
          ) : null}
          <dt>{t`At boot`}</dt>
          <dd>{service.atBoot ? <code>{service.atBoot}</code> : t`Not started at boot`}</dd>
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
      </div>
    </Sheet>
  );
}
