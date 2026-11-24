// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate } from "@bufbuild/protobuf/wkt";
import { useQuery } from "@connectrpc/connect-query";
import { Connection, ContainerService, ContainerState, type Server, type ServerContainer } from "@dockwell/api";
import {
  Button,
  cx,
  Glyph,
  Icon,
  Loading,
  Panel,
  PHONE_QUERY,
  SkeletonRows,
  STATUS,
  StatusBadge,
  Table,
  type TableColumn,
  useMediaQuery,
} from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link, useNavigate } from "@tanstack/react-router";
import { type ReactNode, useMemo } from "react";

import { sentence } from "../../lib/errors";
import { useTimeWords } from "../../lib/time";
import { ActionMenu, type MenuAction } from "../shell/ActionMenu";
import {
  ListPager,
  type ListPlace,
  type ListRouteProps,
  ListSearchField,
  sortableColumns,
  useListPlace,
  useRowMenu,
} from "../shell/ListTools";
import { pagedRows, sortedRows } from "../shell/list";
import { matches, mountSource, portWords, READ_EVERY_MS, statusOf, useContainerStateWord } from "./containers";
import { useTakeOn } from "./takeOn";
import "./ContainersPanel.css";
import { useMay } from "../auth/may";

const SORT_KEYS = ["name", "state", "image", "project"] as const;

/** Up to a few of a list, and how many more there are. */
function Some({ items, more }: { items: readonly string[]; more: number }) {
  const { t } = useLingui();
  const left = items.length - 2 + more;
  if (items.length === 0) return <span className="containers-none">—</span>;
  return (
    <span className="containers-some">
      {items.slice(0, 2).join(", ")}
      {left > 0 ? <span className="containers-more">{t`+${left}`}</span> : null}
    </span>
  );
}

/**
 * The containers on a server that Dockwell didn't make, for someone who brought a
 * server with them. It says what they are and that nothing here touches them, and
 * where Dockwell's own went. Each opens on its details and logs. Past what reads at a
 * glance it grows a filter, and a project column once there is more than one project.
 */
export function ContainersPanel({ server, search, onSearch }: { server: Server } & ListRouteProps) {
  const { t } = useLingui();
  const phone = useMediaQuery(PHONE_QUERY);
  // The containers on a server are an admin's to read, and the panel is left out for anyone else (ADR 0059).
  const allowed = useMay()(ContainerService.method.listContainers).allowed;
  // A server whose agent isn't reporting can't say what runs there: it isn't asked.
  const connected = server.connection === Connection.CONNECTED;
  const blind = connected && server.dockerUnreadableSince !== undefined;
  const list = useQuery(
    ContainerService.method.listContainers,
    { server: server.id },
    { refetchInterval: READ_EVERY_MS, retry: false, enabled: allowed && connected && !blind },
  );
  const place = useListPlace(search, onSearch, SORT_KEYS);
  const containers = list.data?.containers;
  const found = sortedRows(
    (containers ?? []).filter((container) => matches(container, place.query)),
    place.sort,
    (container, key) => {
      if (key === "state") return container.state;
      if (key === "image") return container.image;
      if (key === "project") return container.composeProject;
      return container.name;
    },
  );
  const page = pagedRows(found, place.page);
  const count = containers?.length ?? 0;
  const running = containers?.filter((c) => c.state === ContainerState.RUNNING).length ?? 0;
  const own = list.data?.ownContainers ?? 0;
  const unlisted = list.data?.unlisted ?? 0;
  const projects = useMemo(() => new Set((containers ?? []).map((c) => c.composeProject)), [containers]);

  if (!allowed) return null;
  // With none to list, the page has nothing to say of them, so the panel stays out, as Server-Overview.dc.html has it.
  if (connected && !blind && !list.error && list.data && count === 0) return null;
  // What it last read can't stand once it can't be read, so that is all this says.
  const body: ReactNode = !connected ? (
    <p className="containers-note">
      {server.connection === Connection.PENDING
        ? t`${server.name} is waiting for its agent, so what Docker runs there can't be read yet.`
        : t`${server.name} isn't reporting, so what Docker runs there can't be read now.`}
    </p>
  ) : blind ? (
    <p className="containers-note">{t`Docker isn't answering on ${server.name}, so what runs there can't be read now.`}</p>
  ) : list.error ? (
    <p className="containers-note">{sentence(list.error)}</p>
  ) : !list.data ? (
    <Loading label={t`Reading what Docker runs here`} slow={t`Docker is slow to answer on ${server.name}`}>
      <SkeletonRows rows={3} lead="icon" end="pill" />
    </Loading>
  ) : (
    <>
      <div className="containers-lead">
        <p>
          <Trans>Dockwell only reads these. Open one to take it on as an app.</Trans>
        </p>
        <p>
          {own > 0
            ? plural(own, {
                one: "Dockwell runs # more container here, which isn't listed.",
                other: "Dockwell runs # more containers here, which aren't listed.",
              })
            : null}{" "}
          <Trans>
            Apps you run with Dockwell are under <Link to="/apps">Apps</Link>.
          </Trans>
        </p>
      </div>
      {count > 0 ? (
        <ListSearchField
          className="containers-filter"
          label={t`Filter containers`}
          placeholder={t`Filter by name, image or project`}
          value={place.typed}
          onChange={place.setTyped}
        />
      ) : null}
      {count === 0 ? null : phone ? (
        <PhoneRows server={server.id} rows={page.rows} />
      ) : (
        <Rows server={server.id} rows={page.rows} projects={projects.size > 1} place={place} />
      )}
      <ListPager page={page} onPage={place.setPage} />
      {unlisted > 0 ? (
        <p className="containers-note">
          {plural(unlisted, {
            one: "This server has # more container than fit here.",
            other: "This server has # more containers than fit here.",
          })}{" "}
          <Trans>
            See them all with <code>docker ps -a</code> on the server.
          </Trans>
        </p>
      ) : null}
    </>
  );
  return (
    <Panel
      flush
      className="containers"
      title={t`Containers Dockwell didn't make`}
      actions={
        connected && !blind && count > 0 && !list.error ? (
          <span className="containers-count">
            {plural(count, { one: `# container, ${running} running`, other: `# containers, ${running} running` })}
          </span>
        ) : null
      }
    >
      {body}
    </Panel>
  );
}

function Rows({
  server,
  rows,
  projects,
  place,
}: {
  server: string;
  rows: readonly ServerContainer[];
  /** More than one project among them, which makes a column worth its width. */
  projects: boolean;
  place: ListPlace;
}) {
  const { t } = useLingui();
  const navigate = useNavigate();
  const stateWord = useContainerStateWord();
  const { take } = useTakeOn(server);
  const mayAdopt = useMay()(ContainerService.method.adoptContainer).allowed;
  const context = useRowMenu<ServerContainer>();
  const open = (container: ServerContainer) =>
    void navigate({ to: "/servers/$serverId/containers/$name", params: { serverId: server, name: container.name } });
  const actionsFor = (container: ServerContainer): MenuAction[] => [
    { id: "open", label: t`Open`, onSelect: () => open(container) },
    ...(mayAdopt ? [{ id: "adopt", label: t`Take on`, onSelect: () => take(container) }] : []),
  ];
  const columns: TableColumn<ServerContainer>[] = [
    {
      key: "name",
      header: t`Name`,
      render: (c) => (
        <Link to="/servers/$serverId/containers/$name" params={{ serverId: server, name: c.name }}>
          {c.name}
        </Link>
      ),
    },
    {
      key: "state",
      header: t`State`,
      width: 132,
      render: (c) => <StatusBadge status={statusOf(c.state)} label={stateWord(c.state)} labelIsState />,
    },
    { key: "image", header: t`Image`, mono: true, render: (c) => shortImage(c.image) },
    ...(projects
      ? [
          {
            key: "project",
            header: t`Project`,
            mono: true,
            render: (c: ServerContainer) => c.composeProject || <span className="containers-none">—</span>,
          },
        ]
      : []),
    {
      key: "ports",
      header: t`Ports`,
      mono: true,
      render: (c) => <Some items={c.ports.map(portWords)} more={c.morePorts} />,
    },
    {
      key: "volumes",
      header: t`Volumes`,
      mono: true,
      render: (c) => <Some items={c.mounts.map(mountSource)} more={c.moreMounts} />,
    },
    {
      // Taking it on, with the name and place Dockwell would give it; its own page picks others.
      key: "take",
      header: <span className="dw-sr">{t`Take on`}</span>,
      width: 144,
      align: "right",
      render: (c) => (
        <span className="list-row-actions">
          <Button
            size="sm"
            variant="secondary"
            aria-label={t`Take ${c.name} on as an app`}
            onClick={(event) => {
              event.stopPropagation();
              take(c);
            }}
          >
            {t`Take on`}
          </Button>
          <ActionMenu label={t`Actions for ${c.name}`} actions={actionsFor(c)} />
        </span>
      ),
    },
  ];
  return (
    <>
      <Table<ServerContainer>
        className="containers-table"
        columns={sortableColumns(columns, place, SORT_KEYS)}
        rows={rows}
        caption={t`Containers Dockwell didn't make`}
        empty={t`No container matches this filter.`}
        onRowClick={open}
        onRowContextMenu={(container, at, row) =>
          context.open(container, at, row, {
            label: t`Actions for ${container.name}`,
            actions: actionsFor(container),
          })
        }
      />
      {context.element}
    </>
  );
}

/** The list on a phone: stacked rows of a name and a line of what it is. */
function PhoneRows({ server, rows }: { server: string; rows: readonly ServerContainer[] }) {
  const stateWord = useContainerStateWord();
  if (rows.length === 0) {
    return (
      <p className="containers-note">
        <Trans>No container matches this filter.</Trans>
      </p>
    );
  }
  return (
    <ul className="containers-phone">
      {rows.map((c) => {
        const spec = STATUS[statusOf(c.state)];
        return (
          <li key={c.id}>
            <Link
              to="/servers/$serverId/containers/$name"
              params={{ serverId: server, name: c.name }}
              className="containers-phone-row"
            >
              <span className={cx("containers-phone-glyph", `dw-tone-${spec.tone}`)}>
                <Glyph shape={spec.shape} size={10} />
              </span>
              <span className="containers-phone-text">
                <span className="containers-phone-name">{c.name}</span>
                <span className="containers-phone-sub">
                  {stateWord(c.state)} · {shortImage(c.image)}
                </span>
              </span>
              <Icon name="chevron-right" size={16} />
            </Link>
          </li>
        );
      })}
    </ul>
  );
}

/** An image as a list has room for it: an ID that Docker lists once a tag has moved on is 12 digits of it. */
export function shortImage(image: string): string {
  return image.startsWith("sha256:") ? image.slice(0, 19) : image;
}

/** When a container was made, for its page. */
export function useMadeWords(): (container: ServerContainer) => string {
  const time = useTimeWords();
  return (container) => (container.createTime ? time.ago(timestampDate(container.createTime), Date.now()) : "");
}
