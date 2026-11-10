// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate } from "@bufbuild/protobuf/wkt";
import { useQuery } from "@connectrpc/connect-query";
import { Connection, ContainerService, ContainerState, type Server, type ServerContainer } from "@dockwell/api";
import {
  Button,
  cx,
  Glyph,
  Icon,
  Panel,
  PHONE_QUERY,
  STATUS,
  StatusBadge,
  Table,
  type TableColumn,
  TextField,
  useMediaQuery,
} from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link, useNavigate } from "@tanstack/react-router";
import { type ReactNode, useMemo, useState } from "react";

import { sentence } from "../../lib/errors";
import { useTimeWords } from "../../lib/time";
import { matches, mountSource, portWords, READ_EVERY_MS, statusOf, useContainerStateWord } from "./containers";
import "./ContainersPanel.css";
import { useMay } from "../auth/may";

/** A filter earns its place once there are more containers than read at a glance. */
const FILTER_FROM = 13;
/** How many rows show at a time, so a server of thousands stays a page. */
const PAGE = 100;

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
export function ContainersPanel({ server }: { server: Server }) {
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
  const [filter, setFilter] = useState("");
  const [shown, setShown] = useState(PAGE);
  const containers = list.data?.containers;
  const found = useMemo(() => (containers ?? []).filter((c) => matches(c, filter)), [containers, filter]);
  const count = containers?.length ?? 0;
  const running = containers?.filter((c) => c.state === ContainerState.RUNNING).length ?? 0;
  const own = list.data?.ownContainers ?? 0;
  const unlisted = list.data?.unlisted ?? 0;
  const projects = useMemo(() => new Set((containers ?? []).map((c) => c.composeProject)), [containers]);

  if (!allowed) return null;
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
    <p className="containers-note" aria-busy="true">
      <Trans>Reading what Docker runs here</Trans>
    </p>
  ) : (
    <>
      <div className="containers-lead">
        {count > 0 ? (
          <p>
            <Trans>Dockwell only reads these. Open one to take it on as an app.</Trans>
          </p>
        ) : (
          <p>
            <Trans>Docker runs nothing here that Dockwell didn't make.</Trans>
          </p>
        )}
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
      {count >= FILTER_FROM ? (
        <TextField
          className="containers-filter"
          icon="search"
          aria-label={t`Filter containers`}
          placeholder={t`Filter by name, image or project`}
          value={filter}
          onChange={(event) => {
            setFilter(event.target.value);
            setShown(PAGE);
          }}
        />
      ) : null}
      {count === 0 ? null : phone ? (
        <PhoneRows server={server.id} rows={found.slice(0, shown)} />
      ) : (
        <Rows server={server.id} rows={found.slice(0, shown)} projects={projects.size > 1} />
      )}
      {found.length > shown ? (
        <div className="containers-foot">
          <Button size="sm" variant="ghost" onClick={() => setShown((n) => n + PAGE)}>
            <Trans>Show more</Trans>
          </Button>
        </div>
      ) : null}
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
}: {
  server: string;
  rows: readonly ServerContainer[];
  /** More than one project among them, which makes a column worth its width. */
  projects: boolean;
}) {
  const { t } = useLingui();
  const navigate = useNavigate();
  const stateWord = useContainerStateWord();
  const columns = useMemo<TableColumn<ServerContainer>[]>(
    () => [
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
    ],
    [t, server, stateWord, projects],
  );
  return (
    <Table<ServerContainer>
      className="containers-table"
      columns={columns}
      rows={rows}
      caption={t`Containers Dockwell didn't make`}
      empty={t`No container matches this filter.`}
      onRowClick={(c) =>
        void navigate({ to: "/servers/$serverId/containers/$name", params: { serverId: server, name: c.name } })
      }
    />
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
