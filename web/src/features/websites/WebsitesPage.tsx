// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate } from "@bufbuild/protobuf/wkt";
import type { WebsiteInfo } from "@dockwell/api";
import {
  Button,
  Callout,
  cx,
  Glyph,
  Icon,
  Panel,
  PHONE_QUERY,
  STATUS,
  Table,
  type TableColumn,
  useMediaQuery,
} from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link, useNavigate } from "@tanstack/react-router";
import { type ReactNode, useMemo } from "react";

import { sentence } from "../../lib/errors";
import { formatDateTime, formatDay } from "../../lib/format";
import { RouterButtonLink } from "../../lib/links";
import { ListSkeleton } from "../apps/AppsPage";
import type { DomainRead } from "../apps/domains";
import type { HealthView } from "../apps/health";
import { useFleetView } from "../servers/useFleet";
import { EmptyState } from "../shell/EmptyState";
import { Page } from "../shell/Page";
import { Certificate, CertificateCell, type Row, SiteStanding, SiteStatus, useWebsiteRows } from "./Standing";
import { kindOf, targetOf } from "./website";
import "../apps/Apps.css";
import "./Websites.css";

/**
 * Every website, as Websites-List draws it: its domain and what it sends to, what it
 * is, how it serves now, its certificate and when it last changed. How it serves is its
 * web server's health where it has one, and its domain's state where it sends to a port
 * on the server, which has nothing of its own to check. A screen of its own, so a
 * website's services stay out of Apps.
 */
export function WebsitesPage({ children }: { children?: ReactNode }) {
  const { t } = useLingui();
  const phone = useMediaQuery(PHONE_QUERY);
  const { websites, rows, read, health } = useWebsiteRows();
  const { servers, fleet } = useFleetView();
  const navigate = useNavigate();

  const only = !fleet && servers?.length === 1 ? servers[0] : undefined;
  const count = websites.data?.length ?? 0;
  const lead = only
    ? plural(count, { one: `# site on ${only.name}`, other: `# sites on ${only.name}` })
    : plural(count, { one: "# site", other: "# sites" });
  const pending = websites.isPending;
  const newWebsite = (size?: "lg") => (
    <RouterButtonLink to="/websites/new" variant="primary" icon="plus" {...(size ? { size } : {})}>
      <Trans>New website</Trans>
    </RouterButtonLink>
  );

  let body: ReactNode;
  if (pending) {
    body = phone ? <ListSkeleton label={t`Loading websites`} /> : <WebsitesSkeleton label={t`Loading websites`} />;
  } else if (websites.error) {
    body = (
      <Callout
        tone="critical"
        title={t`Can't load the websites`}
        actions={
          <Button onClick={() => void websites.refetch()} loading={websites.isFetching}>
            <Trans>Try again</Trans>
          </Button>
        }
      >
        {sentence(websites.error)} <Trans>What your websites serve isn't affected.</Trans>
      </Callout>
    );
  } else if (count === 0) {
    body = (
      <EmptyState icon="browser" title={<Trans>No websites yet</Trans>} action={newWebsite("lg")}>
        <p>
          <Trans>Send a domain to a URL, or to a port on your server. You review a plan before anything changes.</Trans>
        </p>
      </EmptyState>
    );
  } else if (phone) {
    body = <PhoneList rows={rows} health={health} read={read} />;
  } else {
    body = (
      <WebsitesTable
        rows={rows}
        caption={only ? t`Websites on ${only.name}` : t`Websites`}
        fleet={fleet}
        health={health}
        read={read}
        onOpen={(id) => void navigate({ to: "/websites/$websiteId", params: { websiteId: id } })}
      />
    );
  }

  return (
    <Page
      title={t`Websites`}
      // The line is held while they load, so the list doesn't move when it lands.
      lead={pending || count === 0 ? <span aria-hidden="true">&nbsp;</span> : lead}
      className="websites-page"
      actions={
        <>
          {only ? (
            <RouterButtonLink to="/servers/$serverId/files" params={{ serverId: only.id }} icon="folder">
              <Trans>Files</Trans>
            </RouterButtonLink>
          ) : null}
          {/* The empty state offers it, and a view has one primary button. */}
          {pending || count === 0 ? null : newWebsite()}
        </>
      }
    >
      <section className="websites-section" aria-label={t`Websites`}>
        {phone && count > 0 ? (
          <div className="websites-phone-head">
            <h2 className="dw-text-heading">{lead}</h2>
            {newWebsite()}
          </div>
        ) : null}
        {body}
      </section>
      {children}
    </Page>
  );
}

/** What a website is, in a word or two, as the list's Type column has it. */
function useTypeWord(): (site: WebsiteInfo) => string {
  const { t } = useLingui();
  return (site) => (kindOf(site.spec) === "unknown" ? t`Unknown` : t`Reverse proxy`);
}

/** Where a website sends, as the line under its domain says it. */
function useTargetWords(): (site: WebsiteInfo) => string {
  const { t } = useLingui();
  return (site) => {
    if (kindOf(site.spec) === "port") {
      const port = site.spec?.serves.case === "proxy" ? site.spec.serves.value.port : 0;
      return t`Port ${port} on the server`;
    }
    return targetOf(site.spec);
  };
}

interface TableProps {
  rows: readonly Row[];
  caption: string;
  fleet: boolean;
  health: HealthView;
  read: DomainRead;
  onOpen: (id: string) => void;
}

/** From this wide, what a website is has a column of its own. */
const TYPE_COLUMN_QUERY = "(min-width: 1100px)";

/** From this wide, on a fleet, so has where it runs; below, it's in the line under its domain. */
const SERVER_COLUMN_QUERY = "(min-width: 1280px)";

/**
 * The columns' heads, which the list and its skeleton share, so nothing moves when it
 * lands. Narrower, the columns of fixed width give way, so the domain always has room.
 */
function useColumnHeads(fleet: boolean) {
  const { t } = useLingui();
  const typed = useMediaQuery(TYPE_COLUMN_QUERY);
  const placed = useMediaQuery(SERVER_COLUMN_QUERY) && fleet;
  return useMemo(() => {
    const heads: { key: string; header: string; width?: number | string; align?: "right" }[] = [
      { key: "website", header: t`Website` },
    ];
    if (typed) heads.push({ key: "type", header: t`Type`, width: 200 });
    heads.push(
      { key: "status", header: t`Status`, width: 128 },
      { key: "certificate", header: t`Certificate`, width: 184 },
    );
    if (placed) heads.push({ key: "server", header: t`Runs on`, width: 140 });
    heads.push({ key: "changed", header: t`Changed`, width: 104, align: "right" });
    return { heads, placed };
  }, [t, typed, placed]);
}

function WebsitesTable({ rows, caption, fleet, health, read, onOpen }: TableProps) {
  const { i18n } = useLingui();
  const typeWord = useTypeWord();
  const target = useTargetWords();
  const { heads, placed } = useColumnHeads(fleet);
  const columns = useMemo<TableColumn<Row>[]>(() => {
    // Where it runs goes under its domain, on a fleet, when it has no column of its own.
    const sub = (row: Row) =>
      fleet && !placed && row.site.serverName ? `${target(row.site)} · ${row.site.serverName}` : target(row.site);
    const render: Record<string, (row: Row) => ReactNode> = {
      website: (row) => (
        <span className="dw-cell">
          {/* The row takes the keyboard; the link is for a new tab, and for screen readers' lists of links. */}
          <Link
            to="/websites/$websiteId"
            params={{ websiteId: row.id }}
            className="dw-cell-title websites-domain apps-clip"
            title={row.site.domain || row.site.name}
            tabIndex={-1}
          >
            {row.site.domain || row.site.name}
          </Link>
          <span className="dw-cell-sub websites-target apps-clip" title={sub(row)}>
            {sub(row)}
          </span>
        </span>
      ),
      type: (row) => typeWord(row.site),
      status: (row) => <SiteStatus row={row} health={health} read={read} />,
      certificate: (row) => <CertificateCell row={row} read={read} />,
      server: (row) => <code className="apps-servers apps-clip">{row.site.serverName}</code>,
      changed: (row) => {
        if (!row.site.updateTime) return <span aria-hidden="true">—</span>;
        const when = timestampDate(row.site.updateTime);
        return (
          <time dateTime={when.toISOString()} title={formatDateTime(when, i18n.locale)}>
            {formatDay(when, i18n.locale)}
          </time>
        );
      },
    };
    return heads.map((head) => ({
      ...head,
      ...(head.key === "changed" || head.key === "server" ? { muted: true } : {}),
      render: render[head.key] as (row: Row) => ReactNode,
    }));
  }, [heads, placed, fleet, health, read, i18n, typeWord, target]);
  return (
    <Table<Row>
      className="websites-table"
      columns={columns}
      rows={rows}
      caption={caption}
      onRowClick={(row) => onOpen(row.id)}
    />
  );
}

/** While the list is on its way: the table with its heads, so nothing moves when it lands. */
function WebsitesSkeleton({ label }: { label: string }) {
  const { fleet } = useFleetView();
  const { heads } = useColumnHeads(fleet);
  const columns = useMemo<TableColumn<{ id: string }>[]>(
    () =>
      heads.map((head) => ({
        ...head,
        render: () => <span className="apps-skeleton-bar" style={{ width: head.key === "website" ? "44%" : "64%" }} />,
      })),
    [heads],
  );
  return (
    <div aria-busy="true">
      <span className="dw-sr">{label}</span>
      <Table
        className="websites-table apps-skeleton"
        columns={columns}
        rows={[{ id: "1" }, { id: "2" }, { id: "3" }]}
        rowClassName={() => "apps-skeleton-row"}
      />
    </div>
  );
}

/** The list on a phone: a website's domain and a line of how it stands, as M-Apps draws apps. */
function PhoneList({ rows, health, read }: { rows: readonly Row[]; health: HealthView; read: DomainRead }) {
  const typeWord = useTypeWord();
  return (
    <Panel flush className="apps-phone websites-phone">
      {rows.map((row) => (
        <Link key={row.id} to="/websites/$websiteId" params={{ websiteId: row.id }} className="apps-phone-row">
          <SiteStanding row={row} health={health} read={read}>
            {({ status }) => (
              <span className={cx("apps-phone-glyph", `dw-tone-${STATUS[status].tone}`)}>
                <Glyph shape={STATUS[status].shape} size={10} />
              </span>
            )}
          </SiteStanding>
          <span className="apps-phone-text">
            <span className="apps-phone-name websites-domain">{row.site.domain || row.site.name}</span>
            <span className="apps-phone-sub">
              <SiteStanding row={row} health={health} read={read}>
                {({ status, word }) => <span className={`apps-phone-word dw-tone-${STATUS[status].tone}`}>{word}</span>}
              </SiteStanding>
              <span aria-hidden="true">·</span>
              <span>{typeWord(row.site)}</span>
              {row.domain && row.look ? (
                <>
                  <span aria-hidden="true">·</span>
                  <Certificate domain={row.domain} look={row.look} read={read} />
                </>
              ) : null}
            </span>
          </span>
          <Icon name="chevron-right" size={16} className="apps-phone-chevron" />
        </Link>
      ))}
    </Panel>
  );
}
