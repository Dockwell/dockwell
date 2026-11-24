// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate } from "@bufbuild/protobuf/wkt";
import { createClient } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import { ChangeService, ChangeState, LogService, type WebsiteInfo, WebsiteService } from "@dockwell/api";
import {
  Button,
  Callout,
  cx,
  Glyph,
  Icon,
  Loading,
  Panel,
  PHONE_QUERY,
  SelectField,
  Skeleton,
  SkeletonRows,
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
import type { DomainRead } from "../apps/domains";
import type { HealthView } from "../apps/health";
import { useMay } from "../auth/may";
import { useChangeSheet } from "../changes/ChangeSheet";
import { useFleetView } from "../servers/useFleet";
import type { MenuAction } from "../shell/ActionMenu";
import { EmptyState } from "../shell/EmptyState";
import {
  BatchBar,
  cardContextMenu,
  ListPager,
  type ListPlace,
  ListSearchField,
  ListSortField,
  RowActions,
  type RowSelection,
  SelectBox,
  SortHeader,
  selectionColumn,
  useBatchAction,
  useListPlace,
  useRowMenu,
  useRowSelection,
} from "../shell/ListTools";
import { type ListSearch, pagedRows, sortedRows } from "../shell/list";
import { Page } from "../shell/Page";
import { RefLink } from "../shell/RefLink";
import { Runtimes } from "./Runtimes";
import { Certificate, CertificateCell, type Row, SiteStanding, SiteStatus, useWebsiteRows } from "./Standing";
import { kindOf, targetOf } from "./website";
import "../apps/Apps.css";
import "./Websites.css";

/** The list's place, in its URL: its filter, its sort, its page, and the kind it shows. */
export interface WebsitesSearch extends ListSearch {
  kind?: string;
}

/** The columns the list sorts by, as its address names them. */
const SORT_KEYS = ["website", "changed"] as const;

/** What a website is, as the list filters by it: a reverse proxy sends to a URL or a port alike. */
function kindKey(site: WebsiteInfo): string {
  const kind = kindOf(site.spec);
  return kind === "port" || kind === "url" ? "proxy" : kind;
}

/**
 * Every website, as Websites-List draws it: its domain and what it sends to, what it
 * is, how it serves now, its certificate and when it last changed. How it serves is its
 * web server's health where it has one, and its domain's state where it sends to a port
 * on the server, which has nothing of its own to check. A screen of its own, so a
 * website's services stay out of Apps.
 */
export function WebsitesPage({
  children,
  search = {},
  onSearch,
}: {
  children?: ReactNode;
  search?: WebsitesSearch;
  onSearch?: (next: WebsitesSearch) => void;
}) {
  const { t } = useLingui();
  const phone = useMediaQuery(PHONE_QUERY);
  const { websites, rows, read, health } = useWebsiteRows();
  const { servers, fleet } = useFleetView();
  const navigate = useNavigate();
  const place = useListPlace(search, onSearch, SORT_KEYS);
  const { actionsFor, canDelete, remove } = useWebsiteActions();
  const target = useTargetWords();

  const only = !fleet && servers?.length === 1 ? servers[0] : undefined;
  const count = websites.data?.length ?? 0;
  const kinds = useMemo(() => [...new Set(rows.map((r) => kindKey(r.site)))], [rows]);
  const shown = useMemo(() => {
    const q = place.query;
    const filtered = rows.filter(
      (r) =>
        (!search.kind || kindKey(r.site) === search.kind) &&
        (q === "" ||
          r.site.domain.toLowerCase().includes(q) ||
          r.site.name.toLowerCase().includes(q) ||
          target(r.site).toLowerCase().includes(q)),
    );
    return sortedRows(filtered, place.sort, (r, key) =>
      key === "changed"
        ? r.site.updateTime
          ? timestampDate(r.site.updateTime).getTime()
          : null
        : r.site.domain || r.site.name,
    );
  }, [rows, search.kind, place.query, place.sort, target]);
  const page = pagedRows(shown, place.page);
  const selection = useRowSelection(
    page.rows.map((row) => row.id),
    useMay()(ChangeService.method.proposeChange).allowed,
  );
  const selected = page.rows.filter((row) => selection.has(row.id));
  const batch = useBatchAction<Row>()(
    selected,
    (row) => {
      const permission = canDelete(row);
      return permission.allowed ? undefined : { permission };
    },
    {
      id: "delete",
      icon: "trash",
      danger: true,
      label: (count) => (count ? t`Delete ${count}` : t`Delete`),
      onSelect: (targets) => remove(targets, selection.clear),
    },
  );
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
    body = phone ? (
      <Loading label={t`Loading websites`}>
        <Panel flush>
          <SkeletonRows rows={3} end="pill" />
        </Panel>
      </Loading>
    ) : (
      <WebsitesSkeleton label={t`Loading websites`} />
    );
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
          <Trans>Serve a folder or a PHP app, or send a domain on to a URL or a port.</Trans>
        </p>
      </EmptyState>
    );
  } else if (shown.length === 0) {
    body = (
      <EmptyState
        icon="search"
        title={t`Nothing matches`}
        action={
          <Button
            size="lg"
            onClick={() => {
              place.clearQuery();
              onSearch?.({ ...search, q: undefined, page: undefined, kind: undefined });
            }}
          >
            <Trans>Clear the filter</Trans>
          </Button>
        }
      >
        <p>
          {place.query ? (
            <Trans>
              No website here has <code>{place.typed.trim()}</code> in its domain or what it sends to.
            </Trans>
          ) : (
            <Trans>No website of this kind here.</Trans>
          )}
        </p>
      </EmptyState>
    );
  } else if (phone) {
    body = (
      <>
        <PhoneList rows={page.rows} health={health} read={read} actionsFor={actionsFor} selection={selection} />
        <ListPager page={page} onPage={place.setPage} />
      </>
    );
  } else {
    body = (
      <>
        <WebsitesTable
          rows={page.rows}
          caption={only ? t`Websites on ${only.name}` : t`Websites`}
          fleet={fleet}
          health={health}
          read={read}
          place={place}
          actionsFor={actionsFor}
          selection={selection}
          onOpen={(id) => void navigate({ to: "/websites/$websiteId", params: { websiteId: id } })}
        />
        <ListPager page={page} onPage={place.setPage} />
      </>
    );
  }

  return (
    <Page
      title={t`Websites`}
      // The line is held while they load, so the list doesn't move when it lands. With none, there is no line
      // to hold, and Files stands level with the title rather than under an empty one.
      lead={pending ? <Skeleton width={96} inline /> : count === 0 ? undefined : lead}
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
      <section
        className={cx("websites-section", selection.any && "has-selection")}
        aria-label={t`Websites`}
        ref={selection.list}
        tabIndex={-1}
        onKeyDown={selection.onKeyDown}
      >
        {phone && count > 0 ? (
          <div className="websites-phone-head">
            <h2 className="dw-text-heading">{lead}</h2>
            {newWebsite()}
          </div>
        ) : null}
        {count > 0 ? (
          <div className="list-bar">
            <ListSearchField
              label={t`Filter websites`}
              placeholder={t`Filter by domain`}
              size={phone ? "lg" : "sm"}
              value={place.typed}
              onChange={place.setTyped}
            />
            {phone ? <ListSortField place={place} columns={{ website: t`Website`, changed: t`Changed` }} /> : null}
            {kinds.length > 1 ? (
              <SelectField
                aria-label={t`Type`}
                size="sm"
                value={search.kind ?? ""}
                options={[
                  { value: "", label: t`Every type` },
                  ...kinds.map((kind) => ({ value: kind, label: kindWord(kind, t) })),
                ]}
                onChange={(event) => onSearch?.({ ...search, page: undefined, kind: event.target.value || undefined })}
              />
            ) : null}
          </div>
        ) : null}
        {body}
        <BatchBar count={selected.length} onClear={selection.clear} {...batch} />
      </section>
      {/* What code runs on, once there are websites or apps from folders, as the empty state has its own next step. */}
      {!pending && !websites.error ? <Runtimes rows={rows} health={health} /> : null}
      {children}
    </Page>
  );
}

/** A kind of website in a word or two, as the list's filter names it. */
function kindWord(kind: string, t: ReturnType<typeof useLingui>["t"]): string {
  switch (kind) {
    case "folder":
      return t`Static files`;
    case "php":
      return t`PHP`;
    case "proxy":
      return t`Reverse proxy`;
    default:
      return t`Unknown`;
  }
}

/** A website's menu: open it here, open the site itself, or delete it as the plan its page opens. */
function useWebsiteActions() {
  const { t } = useLingui();
  const navigate = useNavigate();
  const may = useMay();
  const sheet = useChangeSheet();
  const transport = useTransport();
  const canDelete = (row: Row) => may(WebsiteService.method.deleteWebsite, { project: row.site.id });
  const remove = (rows: readonly Row[], onApplied?: () => void) => {
    const [first, ...rest] = rows;
    if (!first) return;
    sheet.open({
      kind: "plan",
      make: () =>
        createClient(WebsiteService, transport).deleteWebsite({
          id: first.site.id,
          additionalIds: rest.map((row) => row.site.id),
        }),
      onClose: (change) => {
        if (change?.state === ChangeState.APPLIED) onApplied?.();
      },
    });
  };
  const actionsFor = (row: Row): MenuAction[] => {
    const domain = row.site.domain;
    return [
      {
        id: "open",
        icon: "browser",
        label: t`Open`,
        onSelect: () => void navigate({ to: "/websites/$websiteId", params: { websiteId: row.id } }),
      },
      ...(domain
        ? [{ id: "visit", icon: "external" as const, label: t`Open ${domain}`, href: `https://${domain}` }]
        : []),
      ...(row.site.serviceId && may(LogService.method.watchLogs, { environment: row.site.environmentId }).allowed
        ? [
            {
              id: "logs",
              icon: "file" as const,
              label: t`Logs`,
              onSelect: () => void navigate({ to: "/websites/$websiteId/logs", params: { websiteId: row.id } }),
            },
          ]
        : []),
      {
        id: "delete",
        icon: "trash",
        label: t`Delete website`,
        danger: true,
        ...canDelete(row).off,
        onSelect: () => remove([row]),
      },
    ];
  };
  return { actionsFor, canDelete, remove };
}

/** What a website is, in a word or two, as the list's Type column has it. */
function useTypeWord(): (site: WebsiteInfo) => string {
  const { t } = useLingui();
  return (site) => {
    switch (kindOf(site.spec)) {
      case "folder":
        return t`Static files`;
      case "php": {
        const version = site.spec?.serves.case === "php" ? site.spec.serves.value.version : "";
        return t`PHP ${version}`;
      }
      case "unknown":
        return t`Unknown`;
      default:
        return t`Reverse proxy`;
    }
  };
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
  place: ListPlace;
  actionsFor: (row: Row) => MenuAction[];
  selection: RowSelection;
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

function WebsitesTable({ rows, caption, fleet, health, read, place, actionsFor, selection, onOpen }: TableProps) {
  const { t, i18n } = useLingui();
  const typeWord = useTypeWord();
  const target = useTargetWords();
  const menu = useRowMenu<Row>();
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
            className="dw-cell-title websites-domain"
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
      server: (row) => (
        <code className="apps-servers apps-clip">
          <RefLink kind="server" id={row.site.serverId} look="plain">
            {row.site.serverName}
          </RefLink>
        </code>
      ),
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
    const byHead = heads.map((head) => ({
      ...head,
      ...(head.key === "website" || head.key === "changed"
        ? {
            header: <SortHeader label={head.header} sortKey={head.key} place={place} />,
            sort: place.sortOf(head.key),
          }
        : {}),
      ...(head.key === "changed" || head.key === "server" ? { muted: true } : {}),
      render: render[head.key] as (row: Row) => ReactNode,
    }));
    return [
      ...byHead,
      {
        key: "actions",
        header: <span className="dw-sr">{t`Actions`}</span>,
        width: 80,
        align: "right" as const,
        render: (row: Row) => (
          <RowActions
            label={t`Actions for ${row.site.domain || row.site.name}`}
            actions={actionsFor(row)}
            quick={["logs"]}
          />
        ),
      },
    ];
  }, [heads, placed, fleet, health, read, i18n, typeWord, target, place, actionsFor, t]);
  return (
    <>
      <Table<Row>
        className="websites-table"
        columns={[
          ...(selection.enabled
            ? [
                selectionColumn({
                  selection,
                  rows,
                  idOf: (row) => row.id,
                  labelOf: (row) => t`Select ${row.site.domain || row.site.name}`,
                  allLabel: t`Select all rows on this page`,
                }),
              ]
            : []),
          ...columns,
        ]}
        rows={rows}
        rowClassName={(row) => (selection.has(row.id) ? "is-checked" : "")}
        caption={caption}
        onRowClick={(row) => onOpen(row.id)}
        onRowContextMenu={(row, at, el) =>
          menu.open(row, at, el, {
            label: t`Actions for ${row.site.domain || row.site.name}`,
            actions: actionsFor(row),
          })
        }
      />
      {menu.element}
    </>
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
        render: () => null,
        skeleton: head.key === "website" ? "titled" : undefined,
      })),
    [heads],
  );
  return <Table className="websites-table" columns={columns} rows={[]} loading loadingLabel={label} />;
}

/** The list on a phone: a website's domain and a line of how it stands, as M-Apps draws apps. */
function PhoneList({
  rows,
  health,
  read,
  actionsFor,
  selection,
}: {
  rows: readonly Row[];
  health: HealthView;
  read: DomainRead;
  actionsFor: (row: Row) => MenuAction[];
  selection: RowSelection;
}) {
  const { t } = useLingui();
  const typeWord = useTypeWord();
  const menu = useRowMenu<Row>();
  return (
    <Panel flush className="apps-phone websites-phone">
      {rows.map((row) => (
        // biome-ignore lint/a11y/noStaticElementInteractions: The row link and menu button provide keyboard access.
        <div
          key={row.id}
          className={cx("websites-phone-row list-select-card", selection.has(row.id) && "is-checked")}
          onContextMenu={(event) =>
            cardContextMenu(event, (at, el) =>
              menu.open(row, at, el, {
                label: t`Actions for ${row.site.domain || row.site.name}`,
                actions: actionsFor(row),
              }),
            )
          }
        >
          {selection.enabled ? (
            <SelectBox
              label={t`Select ${row.site.domain || row.site.name}`}
              checked={selection.has(row.id)}
              onPick={(on, range) => selection.toggle(row.id, on, range)}
            />
          ) : null}
          <Link to="/websites/$websiteId" params={{ websiteId: row.id }} className="apps-phone-row">
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
                  {({ status, word }) => (
                    <span className={`apps-phone-word dw-tone-${STATUS[status].tone}`}>{word}</span>
                  )}
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
          <RowActions label={t`Actions for ${row.site.domain || row.site.name}`} actions={actionsFor(row)} />
        </div>
      ))}
      {menu.element}
    </Panel>
  );
}
