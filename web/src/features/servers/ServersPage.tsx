// SPDX-License-Identifier: AGPL-3.0-only

import { type Server, ServerService } from "@dockwell/api";
import {
  Button,
  cx,
  FleetGrid,
  type FleetGroup,
  type FleetServer,
  Icon,
  PHONE_QUERY,
  STATUS,
  StatusBadge,
  Table,
  useMediaQuery,
  useUIStrings,
} from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { useEffect, useMemo, useRef, useState } from "react";

import { RouterButtonLink } from "../../lib/links";
import { useApps } from "../apps/useServices";
import { useMay } from "../auth/may";
import { EmptyState } from "../shell/EmptyState";
import { Page } from "../shell/Page";
import { AddServerSheet } from "./AddServerSheet";
import { Connecting, FleetUnreadNote, usePageReconnecting } from "./FleetNotes";
import { LabelChips } from "./LabelChips";
import {
  defaultGroupKey,
  fleetLabels,
  groupBy,
  isProblem,
  type LabelGroup,
  labelSuggestions,
  matches,
  parseSelector,
  parseTerm,
  selectorText,
  type Term,
  termText,
} from "./labels";
import { ServerInspector } from "./ServerInspector";
import { byNeed, serverStatus } from "./server";
import { useServerTiles } from "./tiles";
import { useFleet, useFleetUnread } from "./useFleet";
import "./ServersPage.css";

/** Which server Servers inspects, or that none is, and which servers it lists, as its URL says. */
export interface ServersSearch {
  server?: string;
  /** The inspector was closed, so none opens on its own, a reload and Back included. */
  closed?: boolean;
  /** Add a server opens with the page, as the top bar's New offers it (#617). */
  add?: boolean;
  /** The servers it lists, by their labels, as a selector: "pool:web region:eu" (ADR 0091). */
  selector?: string;
  /** The key its tiles group by, or "none"; unset groups by region where servers have one. */
  group?: string;
}

/** What Group by says for no groups, in the URL. */
const NO_GROUPS = "none";

/** How many rows the table shows before "Show all", as Servers-List pages them. */
export const TABLE_PAGE = 20;

/**
 * Whether a server needs a look: it isn't reporting, hasn't connected, can't read Docker, or is strained,
 * which is whatever its badge doesn't call healthy, so the two can't disagree.
 */
function needsLook(server: Server): boolean {
  return serverStatus(server) !== "healthy";
}

/**
 * Every server, as Servers-List lays them out: each as a tile, then as a row with
 * its numbers and what runs there, and one inspected beside them. Without a server
 * in the URL, the first that needs a look is. On a phone, M-Servers: the tiles,
 * what needs attention, and the server chosen, with the way to its page.
 */
export function ServersPage({
  search,
  onSearch,
}: {
  search: ServersSearch;
  onSearch: (next: ServersSearch, how?: { replace?: boolean }) => void;
}) {
  const { t } = useLingui();
  const strings = useUIStrings();
  const fleet = useFleet();
  const unread = useFleetUnread();
  const status = usePageReconnecting(fleet.status);
  const phone = useMediaQuery(PHONE_QUERY);
  const [adding, setAddingHere] = useState(search.add === true);
  // Closing the sheet takes its word out of the address, so a reload doesn't open it again.
  const setAdding = (open: boolean) => {
    setAddingHere(open);
    if (!open && search.add) onSearch({ ...search, add: undefined }, { replace: true });
  };
  const mayAdd = useMay()(ServerService.method.createJoinTicket);
  const fleetServers = useMemo(
    () => (fleet.status === "connecting" ? [] : [...fleet.servers].sort((a, b) => a.name.localeCompare(b.name))),
    [fleet],
  );
  const terms = useMemo(() => parseSelector(search.selector ?? ""), [search.selector]);
  const servers = useMemo(() => fleetServers.filter((s) => matches(terms, s.labels)), [fleetServers, terms]);
  const used = useMemo(() => fleetLabels(fleetServers), [fleetServers]);
  const groupKey = search.group === NO_GROUPS ? undefined : (search.group ?? defaultGroupKey(used));
  // What a choice keeps of the URL: which servers it lists, and how.
  const kept = { selector: search.selector, group: search.group };
  // Without a server in the URL, the one that needs a look first opens, by the ranking the Overview
  // names one by, once: it goes into the URL, so it stays as others come and go, and a reload or Back keeps it.
  const firstLook = byNeed(servers, serverStatus).find(needsLook)?.id;
  const pick = search.server === undefined && !search.closed ? firstLook : undefined;
  useEffect(() => {
    if (pick) onSearch({ server: pick, selector: search.selector, group: search.group }, { replace: true });
  }, [pick, onSearch, search.selector, search.group]);

  if (fleet.status === "connecting") {
    return (
      // At the width it takes once the fleet arrives, so the page doesn't change size then.
      <Page width="wide" title={t`Servers`}>
        <Connecting error={fleet.error} />
      </Page>
    );
  }
  const count = strings.fleetGrid.servers(fleetServers.length);
  const controlServer = fleetServers.find((s) => s.controlServer);
  const control = controlServer ? { id: controlServer.id, name: controlServer.name } : undefined;
  const chosenId = search.server ?? pick;
  const chosen = chosenId ? servers.find((s) => s.id === chosenId) : undefined;
  // Each choice is a step Back goes through; closing is one too.
  const select = (id: string | undefined) => onSearch(id ? { ...kept, server: id } : { ...kept, closed: true });
  const listing = { server: search.server, closed: search.closed };
  const setSelector = (next: readonly Term[]) =>
    onSearch({ ...listing, group: search.group, selector: next.length > 0 ? selectorText(next) : undefined });
  const setGroup = (key: string | undefined) =>
    onSearch(
      { ...listing, selector: search.selector, group: key === defaultGroupKey(used) ? undefined : (key ?? NO_GROUPS) },
      { replace: true },
    );
  const groups = groupKey ? groupBy(servers, groupKey) : undefined;
  const regions = used.get("region")?.length ?? 0;
  const regionWords = plural(regions, { one: "# region", other: "# regions" });
  const lead = regions > 0 ? t`${count} in ${regionWords}` : count;

  return (
    // The list with its inspector beside it, as Servers-List lays them out, uses the room: a wide page
    // (Layout in design/system/README.md).
    <Page
      width="wide"
      title={t`Servers`}
      lead={lead}
      phoneSub={lead}
      status={status}
      actions={
        <Button variant="primary" icon="plus" onClick={() => setAdding(true)} {...mayAdd.off}>
          <Trans>Add server</Trans>
        </Button>
      }
      phoneAction={
        // A header icon has no room for a reason: it's there only for whoever may use it.
        mayAdd.allowed ? (
          <button
            type="button"
            className="mobile-header-button"
            aria-label={t`Add server`}
            onClick={() => setAdding(true)}
          >
            <Icon name="plus" size={20} />
          </button>
        ) : undefined
      }
    >
      {unread ? <FleetUnreadNote why={unread.why} heard={unread.heard} /> : null}
      {used.size > 0 || terms.length > 0 ? (
        <ServersSelector
          terms={terms}
          used={used}
          matching={servers.length}
          groupKey={groupKey}
          onTerms={setSelector}
          onGroup={setGroup}
        />
      ) : null}
      {fleetServers.length > 0 && servers.length === 0 ? (
        <section className="dw-panel servers-none">
          <span>
            <Trans>No server matches</Trans>
          </span>
          <Button variant="ghost" size="sm" onClick={() => setSelector([])}>
            <Trans>Clear the selector</Trans>
          </Button>
        </section>
      ) : servers.length === 0 ? (
        <EmptyState
          icon="server"
          title={<Trans>No servers yet</Trans>}
          action={
            <Button variant="primary" icon="plus" onClick={() => setAdding(true)} {...mayAdd.off}>
              <Trans>Add server</Trans>
            </Button>
          }
        />
      ) : phone ? (
        <PhoneServers
          servers={servers}
          chosen={chosen}
          select={select}
          unknown={unread !== undefined}
          groups={groups}
          groupKey={groupKey}
          selector={terms}
        />
      ) : (
        <WideServers
          servers={servers}
          chosen={chosen}
          select={select}
          unknown={unread !== undefined}
          groups={groups}
          groupKey={groupKey}
          selector={terms}
        />
      )}
      {/* ?add opens it only for someone who may add a server (ADR 0059). */}
      {adding && mayAdd.allowed ? <AddServerSheet control={control} onClose={() => setAdding(false)} /> : null}
    </Page>
  );
}

interface ListProps {
  servers: readonly Server[];
  chosen: Server | undefined;
  select: (id: string | undefined) => void;
  /** What the fleet last said can't stand, so nobody can say how any server is. */
  unknown: boolean;
  /** The servers by groupKey's value, for the tiles; undefined for one group. */
  groups?: LabelGroup[] | undefined;
  groupKey?: string | undefined;
  /** The selector they match, for the table's caption. */
  selector?: readonly Term[];
}

/**
 * The selector over the list, as Servers-List draws it: its terms as chips, a field
 * that takes another and offers the labels the fleet uses, how many servers match,
 * and Group by, region first.
 */
function ServersSelector({
  terms,
  used,
  matching,
  groupKey,
  onTerms,
  onGroup,
}: {
  terms: readonly Term[];
  used: Map<string, string[]>;
  matching: number;
  groupKey: string | undefined;
  onTerms: (next: readonly Term[]) => void;
  onGroup: (key: string | undefined) => void;
}) {
  const { t } = useLingui();
  const keys = [...used.keys()];
  return (
    <div className="servers-selector">
      <LabelChips
        className="servers-selector-field"
        label={t`Add a label to the selector`}
        icon="filter"
        chips={terms.map((term) => ({
          text: termText(term),
          key: `${term.not ? "!" : ""}${term.key}`,
          value: term.hasValue ? term.value : undefined,
        }))}
        onAdd={(text) => {
          const term = parseTerm(text);
          if (isProblem(term)) return;
          onTerms([...terms.filter((x) => termText(x) !== termText(term)), term]);
        }}
        onRemove={(i) => onTerms(terms.filter((_, j) => j !== i))}
        check={(text) => {
          const term = parseTerm(text);
          return isProblem(term) ? term : undefined;
        }}
        suggestions={labelSuggestions(used)}
        placeholder={t`label:value`}
        after={
          terms.length > 0 ? (
            <span className="label-chips-after">{plural(matching, { one: "# matches", other: "# match" })}</span>
          ) : null
        }
      />
      {keys.length > 0 ? (
        <label className="dw-btn dw-btn-secondary dw-btn-md servers-group">
          <Icon name="layers" size={16} />
          <span className="dw-sr">{t`Group the servers`}</span>
          <select value={groupKey ?? ""} onChange={(e) => onGroup(e.target.value || undefined)}>
            {keys.map((k) => (
              <option key={k} value={k}>
                {t`Group by ${k}`}
              </option>
            ))}
            <option value="">{t`No groups`}</option>
          </select>
          <Icon name="chevron-down" size={14} />
        </label>
      ) : null}
    </div>
  );
}

/**
 * The tiles and the table, with the inspector beside them while a server is chosen. It
 * stays in view as the page scrolls; where it wraps below the table, a choice brings it
 * into view, and closing it puts the keyboard back on the row it was about.
 */
function WideServers({ servers, chosen, select, unknown, groups, groupKey, selector }: ListProps) {
  const { t } = useLingui();
  const tiles = useServerTiles(servers, unknown);
  const tileGroups = useTileGroups(tiles, groups, groupKey);
  const [all, setAll] = useState(false);
  const beyond = chosen ? servers.findIndex((s) => s.id === chosen.id) >= TABLE_PAGE : false;
  const layout = useRef<HTMLDivElement>(null);
  const after = useRef<{ reveal?: boolean; focus?: string }>({});
  const choose = (id: string | undefined) => {
    // Keep a revealed row mounted when its inspector closes, so focus can return to it.
    if (beyond || (id !== undefined && servers.findIndex((s) => s.id === id) >= TABLE_PAGE)) setAll(true);
    after.current = id ? { reveal: true } : { focus: chosen?.id };
    select(id);
  };
  useEffect(() => {
    const { reveal, focus } = after.current;
    after.current = {};
    const box = layout.current;
    if (!box) return;
    if (focus) box.querySelector<HTMLElement>(`.servers-row-${CSS.escape(focus)}`)?.focus();
    if (!reveal) return;
    const inspector = box.querySelector<HTMLElement>(".servers-inspector");
    const main = box.querySelector<HTMLElement>(".servers-main");
    // Beside the table it's in view already; below it, as on a narrow screen, it isn't.
    if (inspector && main && inspector.offsetTop > main.offsetTop + 8) {
      inspector.scrollIntoView?.({ block: "nearest", behavior: "smooth" });
    }
  });
  return (
    <div ref={layout} className={cx("servers-layout", chosen && "is-inspecting")}>
      <div className="servers-main">
        {/* A map is for telling many apart at a glance. One server is its row, which says the same and more. */}
        {servers.length > 1 ? (
          <section className="dw-panel servers-map" aria-label={t`Servers at a glance`}>
            <FleetGrid
              servers={tileGroups ? undefined : tiles}
              groups={tileGroups}
              selectedId={chosen?.id}
              onSelect={(s) => choose(s.id)}
            />
          </section>
        ) : null}
        <ServersTable
          servers={servers}
          tiles={tiles}
          chosen={chosen}
          select={choose}
          unknown={unknown}
          selector={selector}
          all={all || beyond}
          onShowAll={() => setAll(true)}
        />
      </div>
      {chosen ? (
        <ServerInspector key={chosen.id} server={chosen} unknown={unknown} onClose={() => choose(undefined)} />
      ) : null}
    </div>
  );
}

/** Every server as a row: its address, its state, its numbers, and what runs there. */
function ServersTable({
  servers,
  tiles,
  chosen,
  select,
  selector,
  all,
  onShowAll,
}: ListProps & { tiles: readonly FleetServer[]; all: boolean; onShowAll: () => void }) {
  const { t } = useLingui();
  const strings = useUIStrings();
  const runs = useRunsByServer();
  const byId = new Map(tiles.map((tile) => [tile.id, tile]));
  const rows = all ? servers : servers.slice(0, TABLE_PAGE);
  const matching = selectorText(selector ?? []);
  const count = servers.length;
  const shown = rows.length;
  return (
    <section className="servers-table" aria-label={t`Server list`}>
      <Table<Server>
        density="compact"
        caption={matching ? t`Servers matching ${matching}, sorted by name` : t`Servers, sorted by name`}
        rows={rows}
        rowKey="id"
        rowClassName={(s) => `servers-row-${s.id}`}
        selectedKey={chosen?.id ?? null}
        onRowClick={(s) => select(s.id)}
        columns={[
          {
            key: "name",
            header: t`Server`,
            width: 200,
            render: (s) => (
              <span className="dw-cell">
                <span className="dw-cell-title servers-table-name">{s.name}</span>
                {byId.get(s.id)?.ip ? <code className="dw-cell-sub">{byId.get(s.id)?.ip}</code> : null}
              </span>
            ),
          },
          {
            key: "status",
            header: t`Status`,
            width: 160,
            render: (s) => {
              // Why it isn't well, as the old list said it, then what it is.
              const meta = [byId.get(s.id)?.note ?? "", s.controlServer ? t`Control server` : ""]
                .filter(Boolean)
                .join(" · ");
              return <StatusBadge status={byId.get(s.id)?.status ?? serverStatus(s)} meta={meta || undefined} />;
            },
          },
          {
            key: "cpu",
            header: t`CPU`,
            width: 72,
            align: "right",
            render: (s) => percent(byId.get(s.id)?.cpu),
          },
          {
            key: "memory",
            header: t`Memory`,
            width: 84,
            align: "right",
            render: (s) => percent(byId.get(s.id)?.mem),
          },
          {
            key: "runs",
            header: t`Runs`,
            muted: true,
            render: (s) => {
              const said = runs.get(s.id) ?? "";
              return said ? (
                <span className="servers-table-runs" title={said}>
                  {said}
                </span>
              ) : (
                <span aria-hidden="true">—</span>
              );
            },
          },
        ]}
      />
      <div className="servers-table-foot">
        {rows.length < count ? (
          <>
            <span>
              <Trans>
                Showing {shown} of {count} · sorted by name
              </Trans>
            </span>
            <Button variant="ghost" size="sm" onClick={onShowAll}>
              <Trans>Show all {count}</Trans>
            </Button>
          </>
        ) : (
          <span>
            {strings.fleetGrid.servers(count)} · <Trans>sorted by name</Trans>
          </span>
        )}
      </div>
    </section>
  );
}

/**
 * The tiles by the groups the servers are in, each named by its value, with those
 * without the key last, as "No region"; undefined while there's one group or none.
 */
function useTileGroups(
  tiles: readonly FleetServer[],
  groups: LabelGroup[] | undefined,
  key: string | undefined,
): FleetGroup[] | undefined {
  const { t } = useLingui();
  const none = key ? t`No ${key}` : "";
  return useMemo(() => {
    if (!groups || groups.length < 2) return undefined;
    const byId = new Map(tiles.map((tile) => [tile.id, tile]));
    return groups.map((g) => ({
      label: g.value ?? none,
      servers: g.servers.map((s) => byId.get(s.id)).filter((tile): tile is FleetServer => tile !== undefined),
    }));
  }, [tiles, groups, none]);
}

function percent(value: number | null | undefined) {
  return value == null ? <span aria-hidden="true">—</span> : `${value}%`;
}

/**
 * The apps on each server, by name, as their deployments say: a name that two apps
 * share, in two projects, once with how many, as "db ×2".
 */
function useRunsByServer(): Map<string, string> {
  // Apps as Apps lists them: a website's own services are on Websites (ADR 0039).
  const services = useApps();
  return useMemo(() => {
    const names = new Map<string, Map<string, number>>();
    for (const s of services.data ?? []) {
      const name = s.resource?.name ?? s.id;
      for (const d of s.deployments) {
        const on = names.get(d.serverId) ?? new Map<string, number>();
        on.set(name, (on.get(name) ?? 0) + 1);
        names.set(d.serverId, on);
      }
    }
    const out = new Map<string, string>();
    for (const [server, on] of names) {
      const said = [...on.entries()]
        .sort(([a], [b]) => a.localeCompare(b))
        .map(([name, n]) => (n > 1 ? `${name} ×${n}` : name));
      out.set(server, said.join(", "));
    }
    return out;
  }, [services.data]);
}

/**
 * On a phone: the tiles, the servers that need attention (or every server, when
 * none does), and the one chosen, above the tab bar, with the way to its page.
 */
function PhoneServers({ servers, chosen, select, unknown, groups, groupKey }: ListProps) {
  const { t } = useLingui();
  const strings = useUIStrings();
  const tiles = useServerTiles(servers, unknown);
  const tileGroups = useTileGroups(tiles, groups, groupKey);
  const attention = servers.filter(needsLook);
  const listed = attention.length > 0 ? attention : servers;
  const byId = new Map(tiles.map((tile) => [tile.id, tile]));
  const tile = chosen ? byId.get(chosen.id) : undefined;
  const cpuWords = (cpu: number) => t`CPU ${cpu}%`;
  return (
    <>
      {servers.length > 1 ? (
        <section className="dw-panel servers-map" aria-label={t`Servers at a glance`}>
          <FleetGrid
            servers={tileGroups ? undefined : tiles}
            groups={tileGroups}
            size="sm"
            readout={false}
            selectedId={chosen?.id}
            onSelect={(s) => select(s.id)}
          />
        </section>
      ) : null}
      <section className="servers-phone-section" aria-labelledby="servers-phone-heading">
        <div className="servers-phone-head">
          <h2 className="dw-text-heading" id="servers-phone-heading">
            {attention.length > 0 ? <Trans>Needs attention</Trans> : <Trans>Every server</Trans>}
          </h2>
          <span>{strings.fleetGrid.servers(listed.length)}</span>
        </div>
        <ul className="dw-panel servers-phone-list">
          {listed.map((s) => {
            const status = byId.get(s.id)?.status ?? serverStatus(s);
            const why = byId.get(s.id)?.note;
            return (
              <li key={s.id}>
                <button
                  type="button"
                  className={cx("servers-phone-row", s.id === chosen?.id && "is-chosen")}
                  aria-pressed={s.id === chosen?.id}
                  onClick={() => select(s.id)}
                >
                  <StatusBadge status={status} label="" />
                  <span className="servers-phone-text">
                    <span className="servers-phone-name">{s.name}</span>
                    <span className="servers-phone-sub">
                      <span
                        className={cx("servers-phone-word", status !== "healthy" && `dw-tone-${STATUS[status].tone}`)}
                      >
                        {strings.status[status]}
                      </span>
                      {why ? <span> · {why}</span> : null}
                    </span>
                  </span>
                </button>
              </li>
            );
          })}
        </ul>
      </section>
      {chosen && tile ? (
        <section className="servers-phone-chosen" aria-label={t`Chosen server`}>
          <div className="servers-phone-chosen-text">
            <span className="servers-phone-chosen-name">
              <StatusBadge status={tile.status} label="" />
              <span>{chosen.name}</span>
            </span>
            <span className="servers-phone-sub">
              {[strings.status[tile.status], tile.cpu != null ? cpuWords(tile.cpu) : "", tile.ip ?? ""]
                .filter(Boolean)
                .join(" · ")}
            </span>
          </div>
          <RouterButtonLink
            to="/servers/$serverId"
            params={{ serverId: chosen.id }}
            variant="primary"
            iconAfter="arrow-right"
          >
            <Trans>Open</Trans>
          </RouterButtonLink>
        </section>
      ) : null}
    </>
  );
}
