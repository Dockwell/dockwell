// SPDX-License-Identifier: AGPL-3.0-only

import type { ServiceInfo } from "@dockwell/api";
import { Button, cx, Glyph, Icon, Panel, PHONE_QUERY, STATUS, Table, useMediaQuery } from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link, useNavigate } from "@tanstack/react-router";
import { type ReactNode, useMemo } from "react";

import { formatDateTime } from "../../lib/format";
import { RouterButtonLink } from "../../lib/links";
import { useTimeWords } from "../../lib/time";
import { ListSkeleton, useAppsLead } from "../apps/AppsPage";
import { type AppGroup, groupApps, groupDeployedAt, imageOf, nameOf, shortImage, sortGroups } from "../apps/app";
import { useDomainNames } from "../apps/domains";
import { HealthBadge, phoneWordClass, useHealthWords } from "../apps/Health";
import { type AppHealth, type HealthView, useHealth, worstHealth } from "../apps/health";
import { WhatRuns } from "../apps/ImageName";
import { runtimeOf, runtimeWords } from "../apps/runtime";
import { usePlaces, useServices } from "../apps/useServices";
import { useAgoNow, useFleetView } from "../servers/useFleet";
import { useRuntimes } from "../websites/website";
import "../apps/Apps.css";
import "./Overview.css";
import { ReadFailed } from "../shell/ReadFailed";

/** How many apps the overview lists before View all. */
const SHOWN = 8;

/** A row: an app, or a project of several, as Apps folds it. */
interface Row {
  id: string;
  group: AppGroup;
}

const rowHealth = (row: Row, health: HealthView): AppHealth => worstHealth(row.group.services.map((s) => health.of(s)));

/** A row's name: an app's own, or its project's. */
function rowName(row: Row): string {
  const [first] = row.group.services;
  return row.group.name || (first ? nameOf(first) : "");
}

/**
 * The apps on the overview, as Overview-Single lists them: each with how it runs
 * and where it answers, what needs a look first, and the rest one click away.
 * A project of several apps is one row, as Apps folds it.
 */
export function OverviewApps({ fleet = false }: { fleet?: boolean }) {
  const { t } = useLingui();
  const phone = useMediaQuery(PHONE_QUERY);
  const services = useServices();
  const places = usePlaces();
  const health = useHealth();
  const { servers } = useFleetView();
  const all = services.data;
  const groups = useMemo(
    () => sortGroups(groupApps(all ?? [], places.place), servers, (s) => health.of(s)),
    [all, places.place, servers, health],
  );
  const rows = useMemo<Row[]>(() => groups.slice(0, SHOWN).map((group) => ({ id: group.key, group })), [groups]);
  const { lead } = useAppsLead(all ?? [], "", health);

  let body: ReactNode;
  if (services.isPending || places.isPending) {
    body = <ListSkeleton label={t`Loading apps`} />;
  } else if (services.error) {
    body = (
      <ReadFailed
        error={services.error}
        title={t`Can't load the apps`}
        actions={
          <Button onClick={() => void services.refetch()} loading={services.isFetching}>
            <Trans>Try again</Trans>
          </Button>
        }
      >
        <Trans>What runs on your servers isn't affected.</Trans>
      </ReadFailed>
    );
  } else if (rows.length === 0) {
    body = (
      <Panel>
        <div className="overview-empty">
          <p>
            <Trans>No apps yet. Run an image, a Compose file or an app from the catalog.</Trans>
          </p>
          <RouterButtonLink to="/apps/new" size="sm" icon="plus">
            <Trans>New app</Trans>
          </RouterButtonLink>
        </div>
      </Panel>
    );
  } else {
    body = phone ? <PhoneRows rows={rows} health={health} /> : <AppsRows rows={rows} health={health} fleet={fleet} />;
  }

  return (
    <section className="overview-section" aria-labelledby="overview-apps">
      <div className="overview-section-head">
        <h2 className="dw-text-heading" id="overview-apps">
          <Trans>Apps</Trans>
        </h2>
        {all && all.length > 0 ? <span className="overview-section-lead">{lead}</span> : null}
        {all && all.length > 0 ? (
          <Link to="/apps" className="overview-link">
            <Trans>View all</Trans>
          </Link>
        ) : null}
      </div>
      {body}
    </section>
  );
}

/** Where an app or a project's apps run, by server name, as each deployment names it. */
function serversOf(group: Row["group"]): string[] {
  const names = new Set<string>();
  for (const service of group.services) for (const d of service.deployments) if (d.serverName) names.add(d.serverName);
  return [...names].sort((a, b) => a.localeCompare(b));
}

/**
 * The apps as a table. On a fleet, as Overview-Fleet has it, where each runs
 * takes the place of when it was deployed.
 */
function AppsRows({ rows, health, fleet }: { rows: readonly Row[]; health: HealthView; fleet: boolean }) {
  const { t, i18n } = useLingui();
  const time = useTimeWords();
  const navigate = useNavigate();
  const domainNames = useDomainNames();
  const domainsOf = (group: AppGroup) => domainNames(group.environment, group.services);
  const words = useHealthWords();
  const newest = useMemo(() => {
    let out: Date | undefined;
    for (const row of rows) {
      const when = groupDeployedAt(row.group);
      if (!out || when > out) out = when;
    }
    return out;
  }, [rows]);
  const now = useAgoNow(newest);
  const open = (row: Row) => {
    const [first] = row.group.services;
    if (row.group.stack || !first) void navigate({ to: "/apps", search: { q: rowName(row) } });
    else void navigate({ to: "/apps/$serviceId", params: { serviceId: first.id } });
  };
  return (
    <Table<Row>
      className="overview-apps"
      density="compact"
      caption={fleet ? t`Apps on your servers` : t`Apps on this server`}
      rows={rows}
      onRowClick={open}
      columns={[
        {
          key: "app",
          header: t`App`,
          render: (row) => {
            const [first] = row.group.services;
            const count = row.group.services.length;
            return (
              <span className="dw-cell">
                <span className="dw-cell-title apps-clip" title={rowName(row)}>
                  {rowName(row)}
                </span>
                {row.group.stack || !first ? (
                  <span className="apps-stack-sub">
                    <Icon name="layers" size={12} />
                    {plural(count, { one: "# app", other: "# apps" })}
                  </span>
                ) : (
                  <WhatRuns service={first} className="dw-cell-sub" />
                )}
              </span>
            );
          },
        },
        {
          key: "status",
          header: t`Status`,
          width: 136,
          render: (row) => {
            const runs = rowHealth(row, health);
            return <HealthBadge health={runs} className="apps-status" title={words.word(runs)} />;
          },
        },
        ...(fleet
          ? [
              {
                key: "runs",
                header: t`Runs on`,
                width: "26%",
                render: (row: Row) => {
                  const names = serversOf(row.group);
                  if (names.length === 0) return <span aria-hidden="true">—</span>;
                  return (
                    <span className="overview-runs" title={names.join(", ")}>
                      <span>{plural(names.length, { one: "# server", other: "# servers" })}</span>
                      <code className="apps-clip">{names.join(", ")}</code>
                    </span>
                  );
                },
              },
            ]
          : []),
        {
          key: "domain",
          header: t`Domain`,
          width: "32%",
          render: (row) => {
            const names = domainsOf(row.group);
            // Not read yet, or can't be: nobody can say it has none.
            if (names === undefined) return null;
            const [domain, ...more] = names;
            if (!domain) return <span aria-hidden="true">—</span>;
            const others = more.length;
            return (
              <span className="overview-domain" title={[domain, ...more].join(", ")}>
                <code className="apps-clip">{domain}</code>
                {others > 0 ? <span className="overview-more">{t`+${others}`}</span> : null}
              </span>
            );
          },
        },
        ...(fleet
          ? []
          : [
              {
                key: "deployed",
                header: t`Deployed`,
                width: 104,
                align: "right" as const,
                muted: true,
                render: (row: Row) => {
                  const when = groupDeployedAt(row.group);
                  return (
                    <time dateTime={when.toISOString()} title={formatDateTime(when, i18n.locale)}>
                      {time.ago(when, now)}
                    </time>
                  );
                },
              },
            ]),
      ]}
    />
  );
}

/** On a phone, as M-Overview-Single lists them: a mark, a name and how it runs. */
function PhoneRows({ rows, health }: { rows: readonly Row[]; health: HealthView }) {
  const words = useHealthWords();
  const runtimes = useRuntimes().data;
  // An app from a folder says what it runs on and where, as Apps lists it (ADR 0058).
  const whatRuns = (service: ServiceInfo): string => {
    const runtime = runtimeOf(service);
    return runtime ? `${runtimeWords(runtime, runtimes)} · ${runtime.folder}` : shortImage(imageOf(service));
  };
  return (
    <Panel flush className="apps-phone">
      {rows.map((row) => {
        const runs = rowHealth(row, health);
        const [first] = row.group.services;
        const count = row.group.services.length;
        const tone = STATUS[runs.status].tone;
        const inner = (
          <>
            <span className={cx("apps-phone-glyph", `dw-tone-${tone}`)}>
              <Glyph shape={STATUS[runs.status].shape} size={10} />
            </span>
            <span className="apps-phone-text">
              <span className="apps-phone-name">{rowName(row)}</span>
              <span className="apps-phone-sub">
                <span className={phoneWordClass(tone)}>{words.word(runs)}</span>
                {row.group.stack || !first ? (
                  <>
                    <span aria-hidden="true">·</span>
                    <span>{plural(count, { one: "# app", other: "# apps" })}</span>
                  </>
                ) : (
                  <>
                    <span aria-hidden="true">·</span>
                    <span className="apps-phone-image">{whatRuns(first)}</span>
                  </>
                )}
              </span>
            </span>
            <Icon name="chevron-right" size={16} className="apps-phone-chevron" />
          </>
        );
        return row.group.stack || !first ? (
          <Link key={row.id} to="/apps" search={{ q: rowName(row) }} className="apps-phone-row">
            {inner}
          </Link>
        ) : (
          <Link key={row.id} to="/apps/$serviceId" params={{ serviceId: first.id }} className="apps-phone-row">
            {inner}
          </Link>
        );
      })}
    </Panel>
  );
}
