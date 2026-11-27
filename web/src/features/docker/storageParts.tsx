// SPDX-License-Identifier: AGPL-3.0-only

import { type ContainerState, type DockerObject, DockerOwnership, DockerStorageProtection } from "@dockwell/api";
import {
  Icon,
  Loading,
  Panel,
  PHONE_QUERY,
  SkeletonCard,
  SkeletonFacts,
  SkeletonRows,
  Table,
  type TableColumn,
  Tag,
  useMediaQuery,
} from "@dockwell/ui";
import { useLingui } from "@lingui/react/macro";
import { Link, useNavigate } from "@tanstack/react-router";
import type { ReactNode } from "react";

import { useFleetView } from "../servers/useFleet";
import { ActionMenu, type MenuAction } from "../shell/ActionMenu";
import { Page } from "../shell/Page";
import { ContainerStatus } from "./DockerContainers";
import "./Docker.css";
import "./DockerStorage.css";

export function StorageOwnership({ row }: { row: DockerObject }) {
  const { t } = useLingui();
  const protection = row.detail.case === "network" || row.detail.case === "volume" ? row.detail.value.protection : 0;
  const managed =
    row.ownership === DockerOwnership.MANAGED ||
    protection === DockerStorageProtection.INFRASTRUCTURE ||
    protection === DockerStorageProtection.SHARED_LINK;
  const label =
    protection === DockerStorageProtection.BUILT_IN ? t`Built in` : managed ? t`Managed by Dockwell` : t`Not managed`;
  const reason = protection === DockerStorageProtection.RETAINED_VOLUME ? t`Retained for recovery` : undefined;
  return (
    <Tag words {...(managed ? { tone: "cobalt" } : {})}>
      <span title={reason}>{label}</span>
    </Tag>
  );
}

export function StoragePage({
  serverId,
  volumes = false,
  row,
  actions,
  pending,
  children,
}: {
  serverId: string;
  volumes?: boolean;
  row: DockerObject | undefined;
  actions?: ReactNode;
  pending?: boolean;
  children: ReactNode;
}) {
  const { t } = useLingui();
  const server = useFleetView().servers?.find((entry) => entry.id === serverId);
  const inventory = volumes ? "/servers/$serverId/docker/volumes" : "/servers/$serverId/docker/networks";
  const label = volumes ? t`Volumes` : t`Networks`;
  const badge = row ? <StorageOwnership row={row} /> : undefined;
  const phone = useMediaQuery(PHONE_QUERY);
  return (
    <Page
      title={row?.name ?? (volumes ? t`Volume` : t`Network`)}
      width="wide"
      pending={pending ?? false}
      machineTitle={!!row}
      className="docker-storage-page"
      badge={badge}
      phoneSub={badge}
      breadcrumb={
        <nav className="page-breadcrumbs" aria-label={t`Breadcrumb`}>
          <Link to="/servers/$serverId/docker" params={{ serverId }}>
            {server?.name ?? t`Docker`}
          </Link>
          <span aria-hidden="true">/</span>
          <Link to={inventory} params={{ serverId }}>
            {label}
          </Link>
        </nav>
      }
      backLink={
        <Link className="mobile-header-button" aria-label={label} to={inventory} params={{ serverId }}>
          <Icon name="arrow-left" size={18} />
        </Link>
      }
      actions={actions}
    >
      <div className="docker-workspace docker-storage-detail">
        {phone ? actions : null}
        {children}
      </div>
    </Page>
  );
}

export function StorageDetailLoading({ volumes = false }: { volumes?: boolean }) {
  const { t } = useLingui();
  return (
    <Loading
      label={volumes ? t`Loading the volume` : t`Loading the network`}
      slow={t`Docker is slow to answer on this server`}
      className="docker-detail-loading"
    >
      <SkeletonCard title={volumes ? t`Mounted by` : t`Attached containers`} flush>
        <SkeletonRows rows={3} lead="icon" lines={1} end="pill" dense />
      </SkeletonCard>
      <SkeletonCard title={volumes ? t`Volume details` : t`Network details`}>
        <SkeletonFacts count={5} layout="rows" className="docker-facts" />
      </SkeletonCard>
    </Loading>
  );
}

interface Attachment {
  id: string;
  containerId: string;
  name: string;
  state: ContainerState;
  ipv4?: string;
  ipv6?: string;
  mountTarget?: string;
  readOnly?: boolean;
}

export function StorageAttachments({
  serverId,
  title,
  empty,
  partial,
  rows,
  volumes = false,
  actions,
  actionsLoading = false,
}: {
  serverId: string;
  title: string;
  empty: string;
  partial: boolean;
  rows: readonly Attachment[];
  volumes?: boolean;
  actions?: (row: Attachment) => MenuAction[];
  actionsLoading?: boolean;
}) {
  const { t } = useLingui();
  const phone = useMediaQuery(PHONE_QUERY);
  const navigate = useNavigate();
  const target = (row: Attachment) => ({
    to: "/servers/$serverId/docker/containers/$containerId" as const,
    params: { serverId, containerId: row.containerId },
    search: { name: row.name },
  });
  const link = (row: Attachment) => (
    <Link className="dw-link docker-row-link docker-storage-name" {...target(row)}>
      <Icon name="service" size={16} />
      <span>{row.name}</span>
    </Link>
  );
  const menu = (row: Attachment) => (
    <ActionMenu
      label={t`Actions for ${row.name}`}
      loading={actionsLoading}
      actions={[
        { id: "open", label: t`Open container`, icon: "service", onSelect: () => void navigate(target(row)) },
        ...(actions?.(row) ?? []),
      ]}
    />
  );
  const address = (value?: string) =>
    value ? (
      <code>{value}</code>
    ) : (
      <span className="docker-muted">
        ·<span className="dw-sr">{t`No address`}</span>
      </span>
    );
  const mode = (row: Attachment) => (row.readOnly ? <Tag words>{t`Read only`}</Tag> : null);
  const columns: TableColumn<Attachment>[] = [
    { key: "name", header: t`Container`, width: "33%", render: link },
    ...(volumes
      ? [
          { key: "target", header: t`Mount target`, render: (row: Attachment) => <code>{row.mountTarget}</code> },
          { key: "mode", header: t`Access`, width: 100, render: mode },
        ]
      : [
          { key: "ipv4", header: "IPv4", render: (row: Attachment) => address(row.ipv4) },
          { key: "ipv6", header: "IPv6", render: (row: Attachment) => address(row.ipv6) },
        ]),
    { key: "state", header: t`State`, width: 108, render: (row) => <ContainerStatus state={row.state} /> },
    { key: "actions", header: <span className="dw-sr">{t`Actions`}</span>, width: 56, render: menu },
  ];
  return (
    <Panel title={title} flush={rows.length > 0}>
      {partial ? <p className="docker-storage-attachment-note">{t`Some attachments couldn't be listed`}</p> : null}
      {!rows.length ? (
        partial ? null : (
          <p className="docker-muted">{empty}</p>
        )
      ) : phone ? (
        <ul className="docker-card-list">
          {rows.map((row) => (
            <li key={row.id} className="docker-card">
              <div className="docker-card-head">
                {link(row)}
                {menu(row)}
              </div>
              <div className="docker-storage-attachment-meta">
                <ContainerStatus state={row.state} />
                {mode(row)}
              </div>
              {volumes ? (
                <code>{row.mountTarget}</code>
              ) : row.ipv4 || row.ipv6 ? (
                <dl className="docker-facts">
                  {row.ipv4 ? (
                    <div className="docker-fact">
                      <dt>IPv4</dt>
                      <dd>
                        <code>{row.ipv4}</code>
                      </dd>
                    </div>
                  ) : null}
                  {row.ipv6 ? (
                    <div className="docker-fact">
                      <dt>IPv6</dt>
                      <dd>
                        <code>{row.ipv6}</code>
                      </dd>
                    </div>
                  ) : null}
                </dl>
              ) : null}
            </li>
          ))}
        </ul>
      ) : (
        <Table columns={columns} rows={rows} caption={title} className="docker-storage-attachment-table" />
      )}
    </Panel>
  );
}
