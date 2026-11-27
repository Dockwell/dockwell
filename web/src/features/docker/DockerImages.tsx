// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate } from "@bufbuild/protobuf/wkt";
import { useQuery } from "@connectrpc/connect-query";
import {
  ChangeService,
  DockerCleanupKind,
  type DockerImage,
  DockerImageState,
  DockerObjectKind,
  DockerService,
  type Server,
} from "@dockwell/api";
import {
  Button,
  Callout,
  cx,
  Icon,
  Loading,
  PaginationBar,
  Panel,
  SegmentedControl,
  Skeleton,
  SkeletonCard,
  SkeletonFacts,
  SkeletonRows,
  StatusBadge,
  Table,
  type TableColumn,
  Tag,
  TextField,
  useMediaQuery,
} from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link, useNavigate } from "@tanstack/react-router";
import { useEffect, useState } from "react";
import { reasonOf, sentence } from "../../lib/errors";
import { formatBytes, formatDateTime } from "../../lib/format";
import { RouterButtonLink } from "../../lib/links";
import { useHeldRows } from "../../lib/loading";
import { useMay } from "../auth/may";
import { CopyButton } from "../servers/CopyButton";
import { ServerFrame } from "../servers/ServerFrame";
import type { MenuAction } from "../shell/ActionMenu";
import { EmptyState } from "../shell/EmptyState";
import {
  BatchBar,
  cardContextMenu,
  SelectBox,
  selectionColumn,
  useBatchAction,
  useListSearchShortcut,
  useRowMenu,
  useRowSelection,
} from "../shell/ListTools";
import { useImageActions } from "./DockerImageActions";
import { DockerPruneSheet } from "./DockerPruneSheet";
import { shortImageId } from "./imageIdentity";
import { sameServerIds, useDockerWatch } from "./live";
import { Fact, LiveUpdates, MeasuringSize, Unread } from "./parts";
import { useSelectedCleanup } from "./selectedCleanup";
import "./Docker.css";
import "./DockerArtifacts.css";

function ImageStatus({ state }: { state: DockerImageState }) {
  const { t } = useLingui();
  switch (state) {
    case DockerImageState.USED:
      return <StatusBadge status="running" label={t`Used`} labelIsState />;
    case DockerImageState.UNUSED:
      return <StatusBadge status="stopped" label={t`Unused`} labelIsState />;
    case DockerImageState.DANGLING:
      return <StatusBadge status="degraded" label={t`Dangling`} labelIsState />;
    default:
      return <StatusBadge status="unknown" label={t`Unknown`} labelIsState />;
  }
}

type ImageFilter = "all" | "used" | "unused" | "dangling";

const imageStates: Readonly<Record<ImageFilter, DockerImageState>> = {
  all: DockerImageState.UNSPECIFIED,
  used: DockerImageState.USED,
  unused: DockerImageState.UNUSED,
  dangling: DockerImageState.DANGLING,
};

interface ImageRow {
  id: string;
  serverId: string;
  engineId: string;
  /** Whether the disk measurement finished. Image usage is read separately. */
  diskMeasured: boolean;
  image: DockerImage;
}

function selection(row: ImageRow) {
  return {
    serverId: row.serverId,
    imageId: row.engineId,
    tags: row.image.tags,
    platform: row.image.platform,
    alias: row.image.tags.find((tag) => !tag.includes("@")) ?? "",
  };
}

function useImageRowVerbs(refresh: () => void, selected: readonly ImageRow[] = []) {
  const { t } = useLingui();
  const navigate = useNavigate();
  const may = useMay();
  const maintenance = may(DockerService.method.planDockerMaintenance);
  const createApp = may(ChangeService.method.proposeChange);
  const cleanup = useSelectedCleanup(DockerCleanupKind.IMAGE, selected, maintenance.allowed);
  const verbs = (row: ImageRow): MenuAction[] => {
    const reference = selection(row).alias || row.image.digests[0];
    return [
      ...(reference
        ? [
            {
              id: "run",
              label: t`Run as an app`,
              icon: "play" as const,
              ...createApp.off,
              onSelect: () =>
                void navigate({ to: "/apps/new", search: { source: "image", image: reference, server: row.serverId } }),
            },
          ]
        : []),
      {
        id: "delete",
        label: t`Delete`,
        icon: "trash",
        danger: true,
        ...maintenance.off,
        ...(maintenance.allowed ? cleanup.rowOff(row) : {}),
        onSelect: () => cleanup.remove([row], refresh),
      },
    ];
  };
  return { maintenance, cleanup, verbs };
}

export function DockerImages({ servers, fleet }: { servers: readonly Pick<Server, "id" | "name">[]; fleet: boolean }) {
  const { t, i18n } = useLingui();
  const navigate = useNavigate();
  const searchInput = useListSearchShortcut();
  const compact = useMediaQuery(fleet ? "(max-width: 1120px)" : "(max-width: 1000px)");
  const mayPrune = useMay()(DockerService.method.planDockerMaintenance).allowed;
  const [pruning, setPruning] = useState(false);
  const [input, setInput] = useState("");
  const [search, setSearch] = useState("");
  const [filter, setFilter] = useState<ImageFilter>("all");
  const [pages, setPages] = useState([""]);
  const pageToken = pages.at(-1) ?? "";
  useEffect(() => {
    const value = input.trim();
    if (value === search) return;
    const timer = setTimeout(() => {
      setSearch(value);
      setPages([""]);
    }, 200);
    return () => clearTimeout(timer);
  }, [input, search]);
  const serverIds = servers.map((s) => s.id);
  const query = useQuery(
    DockerService.method.listDockerObjects,
    { serverIds, kind: DockerObjectKind.IMAGE, filter: { search, imageState: imageStates[filter] }, pageToken },
    {
      retry: false,
      refetchOnWindowFocus: false,
      placeholderData: (previous, previousQuery) =>
        !pageToken && previous && sameServerIds(previousQuery, serverIds)
          ? {
              ...previous,
              objects: previous.objects.filter((row) => {
                if (row.detail.case !== "image") return false;
                const img = row.detail.value;
                const wanted = imageStates[filter];
                if (wanted !== DockerImageState.UNSPECIFIED && img.state !== wanted) return false;
                if (!search) return true;
                const q = search.toLowerCase();
                return [row.key?.engineId, ...img.tags, ...img.digests].join(" ").toLowerCase().includes(q);
              }),
              nextPageToken: "",
            }
          : undefined,
      refetchInterval: (query) =>
        query.state.data?.objects.some(
          (row) => row.detail.case === "image" && row.detail.value.state === DockerImageState.UNSPECIFIED,
        )
          ? 15_000
          : false,
    },
  );
  const refresh = () => {
    if (pageToken) setPages([""]);
    else void query.refetch();
  };
  const actions = useImageActions(servers, refresh);
  const rows: ImageRow[] = (query.data?.objects ?? []).flatMap((row) =>
    row.key && row.detail.case === "image"
      ? [
          {
            id: `${row.key.serverId}/${row.key.engineId}`,
            serverId: row.key.serverId,
            engineId: row.key.engineId,
            diskMeasured: row.observedAt !== undefined,
            image: row.detail.value,
          },
        ]
      : [],
  );
  const chosenRows = useRowSelection(
    rows.map((row) => row.id),
    mayPrune,
    searchInput,
  );
  const selected = rows.filter((row) => chosenRows.has(row.id));
  const { maintenance, cleanup, verbs } = useImageRowVerbs(refresh, selected);
  const watch = useDockerWatch(
    serverIds,
    [],
    () => {
      void query.refetch();
      cleanup.refresh();
    },
    DockerObjectKind.IMAGE,
  );
  const batch = useBatchAction<ImageRow>()(
    selected,
    (row) => (maintenance.allowed ? cleanup.batchStatus(row) : { permission: maintenance }),
    {
      id: "delete",
      label: (count) => (count ? t`Delete ${count}` : t`Delete`),
      icon: "trash",
      danger: true,
      onSelect: (targets, notes) =>
        cleanup.remove(
          targets,
          () => {
            chosenRows.clear();
            refresh();
          },
          notes,
        ),
    },
  );
  const menuFor = (row: ImageRow) => ({
    label: actions.labelFor(selection(row)),
    actions: actions.withActions(selection(row), false, verbs(row)),
  });
  const context = useRowMenu(menuFor, { rows, keyOf: (row) => row.id });
  const openContext = (row: ImageRow, at: { x: number; y: number }, el: HTMLElement) => {
    cleanup.checkMenu(row);
    context.open(row, at, el);
  };
  const dots = (row: ImageRow) => actions.rowMenu(selection(row), false, verbs(row), () => cleanup.checkMenu(row));
  const open = (row: ImageRow) =>
    void navigate({
      to: "/servers/$serverId/docker/images/$imageId",
      params: { serverId: row.serverId, imageId: row.engineId },
    });
  const selectFilter = (value: ImageFilter) => {
    setFilter(value);
    setPages([""]);
  };
  const next = query.data?.nextPageToken;
  const held = useHeldRows(
    "docker.images",
    query.isSuccess && !search && filter === "all" && !pageToken ? rows.length : undefined,
  );
  const slow = fleet
    ? t`Docker is slow to list images on these servers`
    : t`Docker is slow to list images on this server`;
  const serverName = (id: string) => servers.find((s) => s.id === id)?.name ?? id;
  const link = (row: ImageRow) => (
    <Link
      className="dw-link docker-row-link docker-cell-text"
      title={row.image.tags[0] ?? row.engineId}
      to="/servers/$serverId/docker/images/$imageId"
      params={{ serverId: row.serverId, imageId: row.engineId }}
    >
      <code>{row.image.tags[0] ?? shortImageId(row.engineId)}</code>
    </Link>
  );
  const moreTags = (row: ImageRow) =>
    row.image.tags.length > 1 ? (
      <span className="docker-muted docker-cell-text" title={row.image.tags.slice(1).join("\n")}>
        {plural(row.image.tags.length - 1, { one: "and # more tag", other: "and # more tags" })}
      </span>
    ) : null;
  const columns: TableColumn<ImageRow>[] = [
    ...(chosenRows.enabled
      ? [
          selectionColumn({
            selection: chosenRows,
            rows,
            idOf: (row) => row.id,
            labelOf: (row) => t`Select ${selection(row).alias || shortImageId(row.engineId)}`,
            allLabel: t`Select all rows on this page`,
          }),
        ]
      : []),
    {
      key: "image",
      header: t`Image`,
      width: "auto",
      skeleton: "titled",
      render: (row) => (
        <div className="docker-image-name">
          {link(row)}
          {moreTags(row)}
          {actions.rowResult(selection(row))}
        </div>
      ),
    },
    ...(fleet
      ? [
          {
            key: "server",
            header: t`Server`,
            width: "14%",
            render: (row: ImageRow) => (
              <Link
                className="dw-link docker-cell-text"
                title={serverName(row.serverId)}
                to="/servers/$serverId/docker"
                params={{ serverId: row.serverId }}
              >
                {serverName(row.serverId)}
              </Link>
            ),
          },
        ]
      : []),
    { key: "state", header: t`State`, width: 108, render: (row) => <ImageState row={row} /> },
    {
      key: "platform",
      header: t`Platform`,
      width: 112,
      mono: true,
      render: (row) => <span className="docker-image-platform">{row.image.platform || t`Unknown`}</span>,
    },
    {
      key: "total",
      header: t`Total`,
      width: 82,
      align: "right",
      render: (row) => <ImageBytes bytes={row.image.totalBytes} pending={!row.diskMeasured} />,
    },
    {
      key: "unique",
      header: t`Unique`,
      width: 82,
      align: "right",
      render: (row) => <ImageBytes bytes={row.image.uniqueBytes} pending={!row.diskMeasured} />,
    },
    {
      key: "created",
      header: t`Created`,
      width: 148,
      skeleton: "short",
      render: (row) => {
        const value = row.image.createdAt
          ? formatDateTime(timestampDate(row.image.createdAt), i18n.locale)
          : t`Unknown`;
        return (
          <span className="docker-cell-text" title={value}>
            {value}
          </span>
        );
      },
    },
    {
      key: "actions",
      header: <span className="dw-sr">{t`Actions`}</span>,
      width: 56,
      align: "right",
      skeleton: "action",
      render: dots,
    },
  ];
  const empty = rows.length === 0 && query.isSuccess && !next && !query.data.unavailable.length;
  return (
    <>
      <div className="docker-section-heading">
        <h2>
          <Trans>Images</Trans>
        </h2>
        <div className="docker-heading-actions">
          <Button
            icon="refresh"
            size="sm"
            variant="ghost"
            onClick={() => {
              refresh();
              actions.refreshCapabilities();
            }}
            loading={query.isFetching && !query.isPending}
            aria-label={t`Refresh images`}
            title={t`Refresh images`}
          />
          {actions.openSearch ? (
            <Button variant="secondary" size="sm" icon="search" onClick={actions.openSearch}>
              <Trans>Search registries</Trans>
            </Button>
          ) : null}
          {actions.canPull ? (
            <Button variant="primary" size="sm" icon="download" onClick={actions.openPull}>
              <Trans>Pull image</Trans>
            </Button>
          ) : actions.capabilityPending ? (
            <Loading label={t`Loading image actions`}>
              <Skeleton shape="pill" width={110} height={30} />
            </Loading>
          ) : null}
          {actions.archiveMenu(rows.map(selection))}
        </div>
      </div>
      <div className="docker-filters">
        <SegmentedControl
          label={t`Image state`}
          value={filter}
          options={[
            { value: "all", label: t`All` },
            { value: "used", label: t`Used` },
            { value: "unused", label: t`Unused` },
            { value: "dangling", label: t`Dangling` },
          ]}
          onChange={selectFilter}
        />
        <TextField
          ref={searchInput}
          icon="search"
          value={input}
          maxLength={255}
          onChange={(event) => setInput(event.target.value)}
          placeholder={t`Tag, digest or ID`}
          aria-label={t`Search images`}
        />
        {mayPrune ? (
          <Button
            className="docker-filters-action"
            icon="trash"
            size="sm"
            variant="secondary"
            onClick={() => setPruning(true)}
          >
            <Trans>Remove unused</Trans>
          </Button>
        ) : null}
      </div>
      <LiveUpdates paused={watch.paused} retry={watch.retry} />
      {actions.leases}
      {actions.sheets}
      {pruning ? (
        <DockerPruneSheet
          kind={DockerCleanupKind.IMAGE}
          servers={servers}
          onClose={() => setPruning(false)}
          onApplied={refresh}
          onReviewAgain={() => setPruning(true)}
        />
      ) : null}
      <Unread values={query.data?.unavailable ?? []} servers={servers} retry={refresh} />
      {query.error ? (
        <Callout tone="warning" title={t`Images couldn't be read`}>
          {reasonOf(query.error) === "docker_resync"
            ? t`The image list changed. Refresh it to continue.`
            : sentence(query.error)}
        </Callout>
      ) : null}
      {query.isPending && !query.error ? (
        compact ? (
          <Loading label={t`Loading local images`} slow={slow}>
            <Panel flush>
              <SkeletonRows rows={held} end="pill" />
            </Panel>
          </Loading>
        ) : (
          <Panel flush>
            <Table
              className="docker-table docker-image-table"
              columns={columns}
              rows={[]}
              caption={t`Local images`}
              loading
              loadingRows={held}
              loadingLabel={t`Loading local images`}
              slow={slow}
            />
          </Panel>
        )
      ) : null}
      {empty && (search || filter === "all") ? (
        <EmptyState icon="archive" title={search ? t`No matching images` : t`No local images`} />
      ) : null}
      {empty && !search && filter !== "all" ? (
        <EmptyState
          icon="archive"
          title={
            filter === "used" ? t`No used images` : filter === "unused" ? t`No unused images` : t`No dangling images`
          }
          action={
            <Button size="sm" onClick={() => selectFilter("all")}>
              <Trans>Show all images</Trans>
            </Button>
          }
        />
      ) : null}
      {rows.length ? (
        <section
          aria-label={t`Local images`}
          className={cx("list-select-region", chosenRows.any && "has-selection")}
          ref={chosenRows.list}
          tabIndex={-1}
          onKeyDown={chosenRows.onKeyDown}
        >
          {compact ? (
            <ul className="docker-artifact-list" aria-label={t`Local images`}>
              {rows.map((row) => (
                <li
                  key={row.id}
                  className={cx("docker-image-row list-select-card", chosenRows.has(row.id) && "is-checked")}
                  onContextMenu={(event) => cardContextMenu(event, (at, el) => openContext(row, at, el))}
                >
                  <div className="docker-artifact-name">
                    {chosenRows.enabled ? (
                      <SelectBox
                        label={t`Select ${selection(row).alias || shortImageId(row.engineId)}`}
                        checked={chosenRows.has(row.id)}
                        onPick={(on, range) => chosenRows.toggle(row.id, on, range)}
                      />
                    ) : null}
                    <Icon name="archive" />
                    <div>
                      {link(row)}
                      {fleet ? <div className="docker-muted">{serverName(row.serverId)}</div> : null}
                      {moreTags(row)}
                      {actions.rowResult(selection(row))}
                    </div>
                  </div>
                  <div className="docker-image-row-actions">
                    <ImageState row={row} />
                    {dots(row)}
                  </div>
                  <dl className="docker-image-data">
                    <Fact label={t`Platform`} value={row.image.platform || t`Unknown`} mono />
                    <Fact
                      label={t`Total`}
                      value={<ImageBytes bytes={row.image.totalBytes} pending={!row.diskMeasured} />}
                    />
                    <Fact
                      label={t`Unique`}
                      value={<ImageBytes bytes={row.image.uniqueBytes} pending={!row.diskMeasured} />}
                    />
                    <Fact
                      label={t`Created`}
                      value={
                        row.image.createdAt
                          ? formatDateTime(timestampDate(row.image.createdAt), i18n.locale)
                          : t`Unknown`
                      }
                    />
                  </dl>
                </li>
              ))}
            </ul>
          ) : (
            <Panel flush>
              <Table
                className="docker-table docker-image-table"
                columns={columns}
                rows={rows}
                rowKey="id"
                rowClassName={(row) => (chosenRows.has(row.id) ? "is-checked" : "")}
                onRowClick={open}
                onRowContextMenu={(row, at, el) => openContext(row, at, el)}
                caption={t`Local images`}
              />
            </Panel>
          )}
          {context.element}
          <BatchBar count={selected.length} onClear={chosenRows.clear} {...batch} />
        </section>
      ) : null}
      {next || pageToken ? (
        <PaginationBar
          label={t`Image pages`}
          summary={<Trans>Page {pages.length}</Trans>}
          hasPrevious={Boolean(pageToken)}
          hasNext={Boolean(next)}
          onPrevious={() => setPages((old) => old.slice(0, -1))}
          onNext={() => next && setPages((old) => [...old, next])}
          previousLabel={<Trans>Previous</Trans>}
          nextLabel={<Trans>Next</Trans>}
        />
      ) : null}
    </>
  );
}

/** Its state, or, while Docker still counts what uses it, the shape of a state on its way. */
function ImageState({ row }: { row: ImageRow }) {
  const { t } = useLingui();
  if (row.image.state === DockerImageState.UNSPECIFIED) {
    return (
      <span role="img" aria-label={t`Measuring`}>
        <Skeleton shape="pill" width={64} inline />
      </span>
    );
  }
  return <ImageStatus state={row.image.state} />;
}

function ImageBytes({ bytes, pending }: { bytes: bigint | undefined; pending: boolean }) {
  const { t, i18n } = useLingui();
  if (bytes !== undefined) return formatBytes(bytes, i18n.locale);
  if (!pending) return t`Unknown`;
  return <MeasuringSize />;
}

export function DockerImageDetail({ serverId, imageId }: { serverId: string; imageId: string }) {
  const { t, i18n } = useLingui();
  const allowed = useMay()(DockerService.method.getDockerObject).allowed;
  const read = useQuery(
    DockerService.method.getDockerObject,
    { key: { serverId, engineId: imageId, kind: DockerObjectKind.IMAGE } },
    { enabled: allowed, retry: false },
  );
  const watch = useDockerWatch(allowed ? [serverId] : [], [], () => void read.refetch(), DockerObjectKind.IMAGE);
  const row = read.data?.object;
  const image = row?.detail.case === "image" ? row.detail.value : undefined;
  const actions = useImageActions([{ id: serverId, name: "" }], () => void read.refetch());
  const { cleanup, verbs } = useImageRowVerbs(() => void read.refetch());
  const selected = {
    serverId,
    imageId,
    tags: image?.tags ?? [],
    platform: image?.platform ?? "",
    alias: image?.tags.find((tag) => !tag.includes("@")) ?? "",
  };
  return (
    <ServerFrame id={serverId} phoneHead={{ phoneTitle: t`Image` }}>
      {(server) => (
        <section className="docker-detail-body">
          <div className="docker-section-heading docker-artifact-heading">
            <h2>
              {allowed && read.isPending ? (
                // Its name is on its way: a tag replaces the ID once read, so the ID isn't shown meanwhile.
                <>
                  <span className="dw-sr">{shortImageId(imageId)}</span>
                  <Skeleton text="heading" width={220} />
                </>
              ) : (
                <code>{image?.tags[0] ?? shortImageId(imageId)}</code>
              )}
            </h2>
            <div className="docker-heading-actions">
              <RouterButtonLink
                to="/servers/$serverId/docker/images"
                params={{ serverId: server.id }}
                variant="secondary"
                size="sm"
              >
                <Trans>All images</Trans>
              </RouterButtonLink>
              {image
                ? actions.rowMenu(
                    selected,
                    true,
                    verbs({ id: imageId, serverId, engineId: imageId, diskMeasured: false, image }),
                    () => cleanup.checkMenu({ serverId, engineId: imageId }),
                  )
                : null}
            </div>
          </div>
          {actions.rowResult(selected)}
          <LiveUpdates paused={watch.paused} retry={watch.retry} />
          {actions.sheets}
          {!allowed ? <EmptyState icon="lock" title={t`Docker inventory needs an admin`} /> : null}
          {allowed && read.isPending ? (
            <Loading
              label={t`Loading the image`}
              slow={t`Docker is slow to answer on this server`}
              className="docker-detail-loading"
            >
              <SkeletonCard title={t`Image details`}>
                <SkeletonFacts count={5} layout="rows" className="docker-facts" />
              </SkeletonCard>
              <SkeletonCard title={t`Used by`} flush>
                <SkeletonRows rows={2} lead="icon" lines={1} dense />
              </SkeletonCard>
              <SkeletonCard title={t`Layers`} lines={3} />
            </Loading>
          ) : null}
          {read.error ? (
            <Callout title={t`Image couldn't be read`} tone="warning">
              {sentence(read.error)}
            </Callout>
          ) : null}
          {read.isSuccess && !image ? <EmptyState icon="archive" title={t`Image is gone`} /> : null}
          {image ? (
            <>
              <Panel title={t`Image details`} actions={<ImageStatus state={image.state} />}>
                <dl className="docker-facts">
                  <Fact
                    label={t`Image ID`}
                    value={
                      <span className="docker-image-identity">
                        <code>{shortImageId(imageId)}</code>
                        <CopyButton text={imageId} label={t`Copy full image ID`} />
                      </span>
                    }
                  />
                  <Fact label={t`Platform`} value={image.platform || t`Unknown`} mono />
                  <Fact
                    label={t`Total size`}
                    value={<ImageBytes bytes={image.totalBytes} pending={!row?.observedAt} />}
                  />
                  <Fact
                    label={t`Unique size`}
                    value={<ImageBytes bytes={image.uniqueBytes} pending={!row?.observedAt} />}
                  />
                  <Fact
                    label={t`Created`}
                    value={image.createdAt ? formatDateTime(timestampDate(image.createdAt), i18n.locale) : t`Unknown`}
                  />
                </dl>
                {image.tags.length || image.digests.length ? <ImageAliases image={image} /> : null}
              </Panel>
              <Panel title={t`Used by`}>
                {image.containers.length || image.deployments.length ? (
                  <ul className="docker-servers">
                    {image.containers.map((container) => (
                      <li key={container.key?.engineId ?? container.name}>
                        <Icon name="service" />
                        <Link
                          className="dw-link docker-row-link"
                          to="/servers/$serverId/docker/containers/$containerId"
                          params={{ serverId, containerId: container.key?.engineId ?? "" }}
                        >
                          <code>{container.name}</code>
                        </Link>
                      </li>
                    ))}
                    {image.deployments.map((deployment) => (
                      <li key={`${deployment.serviceId}/${deployment.revision}`}>
                        <Icon name="undo" />
                        <Link
                          className="dw-link docker-row-link"
                          to="/apps/$serviceId/deployments"
                          params={{ serviceId: deployment.serviceId }}
                        >
                          {deployment.serviceName}
                        </Link>
                        <span className="docker-muted">
                          <Trans>Retained revision {String(deployment.revision)}</Trans>
                        </span>
                      </li>
                    ))}
                  </ul>
                ) : (
                  <span className="docker-muted">
                    <Trans>No containers or retained deployments</Trans>
                  </span>
                )}
              </Panel>
              <Panel title={t`Layers`}>
                {image.layers.length ? (
                  <ol className="docker-layer-list">
                    {image.layers.map((layer, index) => (
                      // biome-ignore lint/suspicious/noArrayIndexKey: The image history is immutable and can share layer IDs.
                      <li key={`${layer.diffId}/${index}`}>
                        <code>{layer.diffId}</code>
                        {layer.sizeBytes === undefined ? null : (
                          <span className="docker-cleanup-meta">{formatBytes(layer.sizeBytes, i18n.locale)}</span>
                        )}
                      </li>
                    ))}
                  </ol>
                ) : (
                  <span className="docker-muted">
                    <Trans>No layers reported</Trans>
                  </span>
                )}
              </Panel>
              <Panel title={t`History`} actions={<Icon name="lock" aria-label={t`Instructions redacted`} />}>
                {image.historyUnavailable ? (
                  <span className="docker-muted">
                    <Trans>History unavailable</Trans>
                  </span>
                ) : (
                  <>
                    <span className="docker-muted">
                      <Trans>Build instructions are redacted</Trans>
                    </span>
                    <ol className="docker-history-list">
                      {image.history.map((entry, index) => (
                        // biome-ignore lint/suspicious/noArrayIndexKey: Redacted history entries can have identical timestamps and sizes.
                        <li key={index}>
                          <span>
                            {entry.createdAt
                              ? formatDateTime(timestampDate(entry.createdAt), i18n.locale)
                              : t`Unknown date`}
                          </span>
                          <code>
                            {entry.sizeBytes === undefined
                              ? t`Unknown size`
                              : formatBytes(entry.sizeBytes, i18n.locale)}
                          </code>
                        </li>
                      ))}
                    </ol>
                  </>
                )}
              </Panel>
              {image.detailsTruncated ? (
                <p className="docker-muted">
                  <Trans>Only the first entries are shown</Trans>
                </p>
              ) : null}
            </>
          ) : null}
        </section>
      )}
    </ServerFrame>
  );
}

function ImageAliases({ image }: { image: DockerImage }) {
  return (
    <details className="docker-aliases">
      <summary>
        <Trans>Tags and digests</Trans>
      </summary>
      <ul className="docker-detail-list">
        {Array.from(new Set([...image.tags, ...image.digests])).map((alias) => (
          <li key={alias}>
            <Tag className="docker-image-alias">{alias}</Tag>
          </li>
        ))}
      </ul>
    </details>
  );
}
