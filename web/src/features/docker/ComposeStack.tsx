// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { createConnectQueryKey, useQuery, useTransport } from "@connectrpc/connect-query";
import {
  BackupService,
  ChangeService,
  ChangeState,
  ComposeService,
  type ComposeWorkspaceResource,
  type DockerObject,
  DockerObjectKind,
  DockerOwnership,
  DockerService,
  FileService,
  type GetComposeWorkspaceResponse,
  LogService,
  type ServiceInfo,
} from "@dockwell/api";
import {
  Button,
  Callout,
  Icon,
  Panel,
  SelectField,
  Sheet,
  StatusBadge,
  Table,
  type TableColumn,
  TabNav,
  Tag,
  useMediaQuery,
} from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { useQueryClient } from "@tanstack/react-query";
import { Link, useNavigate } from "@tanstack/react-router";
import { lazy, Suspense, useState } from "react";

import { sentence } from "../../lib/errors";
import { RouterButtonLink, RouterTabLink } from "../../lib/links";
import { appOf } from "../apps/app";
import { useServices } from "../apps/useServices";
import { useMay } from "../auth/may";
import { useChangeSheet } from "../changes/ChangeSheet";
import { useFleetView } from "../servers/useFleet";
import { EmptyState } from "../shell/EmptyState";
import { Page } from "../shell/Page";
import { ComposeEditor, ComposeResourceKind, KeptValues } from "./ComposeEditor";
import { ComposeOperations, useComposeWorkloadPlan } from "./ComposeOperations";
import { composePlacementGroups, copyKey, onComposeServer, selectedVolumeCopies } from "./composePlacement";
import { ComposeOwnership, ComposeStatus, composeOf, stackPath } from "./DockerComposes";
import { ContainerActions, ContainerStatus, dockerPort } from "./DockerContainers";
import { useDockerWatch } from "./live";
import { containerOf, LiveUpdates } from "./parts";
import type { ComposeDetailSearch, ComposeRunAction } from "./search";
import { useBackupRows } from "./useComposeBackups";

import "./Compose.css";

const inventory = "/servers/$serverId/docker/composes";
const Logs = lazy(async () => ({ default: (await import("../logs/LogsPage")).LogsPage }));
const BackUpSheet = lazy(async () => ({ default: (await import("../backups/BackUpSheet")).BackUpSheet }));

interface StackProps {
  serverId: string;
  composeId: string;
  search: ComposeDetailSearch;
  onSearch: (search: ComposeDetailSearch) => void;
}

export function ComposeStack({ serverId, composeId, search, onSearch }: StackProps) {
  const { t } = useLingui();
  const may = useMay();
  const sheet = useChangeSheet();
  const transport = useTransport();
  const navigate = useNavigate();
  const queries = useQueryClient();
  const [taking, setTaking] = useState(false);
  const [backup, setBackup] = useState<string>();
  const [deleting, setDeleting] = useState(false);
  const [volumeSelection, setVolumeSelection] = useState<{ snapshot: string; copies: ReadonlySet<string> }>();
  const [workload, setWorkload] = useState<ComposeRunAction>();
  const planWorkload = useComposeWorkloadPlan();
  const fleet = useFleetView().servers ?? [];
  const server = fleet.find((item) => item.id === serverId);
  const serverName = (id: string) => fleet.find((item) => item.id === id)?.name ?? id;
  const object = useQuery(
    DockerService.method.getDockerObject,
    {
      key: { serverId, engineId: composeId, kind: DockerObjectKind.COMPOSE },
    },
    {
      retry: false,
      enabled: may(DockerService.method.getDockerObject).allowed,
      refetchOnWindowFocus: false,
    },
  );
  const row = object.data?.object;
  const stack = composeOf(row);
  const environment = stack?.environmentId ?? "";
  const workspace = useQuery(
    ComposeService.method.getComposeWorkspace,
    { environmentId: environment },
    {
      enabled: environment !== "" && may(ComposeService.method.getComposeWorkspace, { environment }).allowed,
      refetchOnWindowFocus: false,
      retry: false,
    },
  );
  const outside = row?.ownership === DockerOwnership.EXTERNAL;
  const resources = workspace.data?.workspace?.resources ?? [];
  const placed = onComposeServer(resources, serverId);
  const services = placed.filter((resource) => resource.kind === "service");
  const serverCount = new Set(resources.flatMap((resource) => resource.serverIds)).size;
  const deleteGroups = composePlacementGroups(placed, serverId);
  const serviceGroups = composePlacementGroups(services, serverId);
  const snapshot = workspace.data?.snapshot ?? "";
  const volumeCopies = volumeSelection?.snapshot === snapshot ? volumeSelection.copies : new Set<string>();
  const chosen = selectedVolumeCopies(placed, volumeCopies);
  const canTake = outside && may(ComposeService.method.adoptCompose).allowed;
  const canEdit = environment !== "" && may(ComposeService.method.planComposeWorkspace, { environment }).allowed;
  const canBackup = environment !== "" && may(BackupService.method.createBackup, { environment }).allowed;
  const volumes = resources
    .filter((resource) => resource.kind === "volume" && resource.serverIds.includes(serverId))
    .map((volume) => ({
      id: volume.resourceId,
      name: volume.name,
      environment,
    }));
  const canDelete = canEdit && services.length > 0;
  const closeAction = () => {
    if (search.action) {
      const { action: _, ...rest } = search;
      onSearch(rest);
    }
  };
  const closeDelete = () => {
    setDeleting(false);
    setVolumeSelection(undefined);
    closeAction();
  };
  const closeWorkload = () => {
    setWorkload(undefined);
    closeAction();
  };
  const run =
    workload ??
    (search.action === "start" || search.action === "stop" || search.action === "restart" ? search.action : undefined);
  const allowedRun =
    environment &&
    run &&
    may(run === "restart" ? ChangeService.method.restartService : ChangeService.method.proposeChange, { environment })
      .allowed;
  const closeTake = () => {
    setTaking(false);
    closeAction();
  };
  const loading = object.isPending || (environment !== "" && workspace.isPending);
  const params = { serverId, composeId };
  const label = row?.name ?? t`Compose project`;
  const refresh = () => {
    void object.refetch();
    if (environment) void workspace.refetch();
  };
  const watch = useDockerWatch(
    may(DockerService.method.watchDockerObjects).allowed ? [serverId] : [],
    [],
    () => {
      refresh();
      void queries.invalidateQueries({
        queryKey: createConnectQueryKey({ schema: DockerService.method.listDockerObjects, cardinality: undefined }),
      });
    },
    true,
  );
  const applied = (change: { state: ChangeState } | undefined) => {
    if (change?.state === ChangeState.APPLIED) refresh();
  };
  const deleteStack = () => {
    if (!workspace.data || chosen.partial) return;
    closeDelete();
    sheet.open({
      kind: "plan",
      make: () =>
        createClient(ComposeService, transport).planComposeWorkspace({
          environmentId: environment,
          snapshot,
          serverId,
          explicitRemovals: [...placed.filter((item) => item.kind !== "volume"), ...chosen.all].map(
            ({ kind, name }) => ({ kind, name }),
          ),
        }),
      onClose: (change) => {
        if (change?.state === ChangeState.APPLIED) void navigate({ to: inventory, params: { serverId } });
      },
    });
  };
  const reloadBase = async (): Promise<GetComposeWorkspaceResponse | undefined> => {
    const fresh = await workspace.refetch();
    if (fresh.error) throw fresh.error;
    return fresh.data;
  };
  const actions = row ? (
    <div className="compose-actions">
      {canTake ? (
        <Button variant="primary" icon="download" onClick={() => setTaking(true)}>
          <Trans>Take on</Trans>
        </Button>
      ) : null}
      {!outside && services.length ? (
        <ComposeOperations
          row={row}
          showText
          onWorkload={(action) => {
            if (action !== "restart" && serviceGroups.length > 1) setWorkload(action);
            else planWorkload(row, services, action, refresh);
          }}
        />
      ) : null}
      {canBackup && volumes.length ? (
        <Button icon="archive" onClick={() => setBackup(volumes[0]?.id)}>
          <Trans>Back up</Trans>
        </Button>
      ) : null}
      {canDelete ? (
        <Button
          variant="ghost"
          icon="trash"
          onClick={() => {
            setVolumeSelection(undefined);
            setDeleting(true);
          }}
        >
          <Trans>Delete stack</Trans>
        </Button>
      ) : null}
    </div>
  ) : undefined;
  return (
    <Page
      title={label}
      width="wide"
      machineTitle={outside}
      className="compose-page"
      actions={actions}
      badge={
        row ? (
          <div className="compose-actions">
            <ComposeStatus row={row} />
            <ComposeOwnership row={row} />
            {serverCount > 1 ? <Tag words>{t`On ${serverCount} servers`}</Tag> : null}
          </div>
        ) : undefined
      }
      phoneSub={
        row ? (
          <div className="compose-actions">
            <ComposeStatus row={row} />
            <ComposeOwnership row={row} />
            {serverCount > 1 ? <Tag words>{t`On ${serverCount} servers`}</Tag> : null}
          </div>
        ) : undefined
      }
      breadcrumb={
        <nav className="page-breadcrumbs" aria-label={t`Breadcrumb`}>
          <Link to="/servers/$serverId/docker" params={{ serverId }}>
            {server?.name ?? t`Docker`}
          </Link>
          <span aria-hidden="true">/</span>
          <Link to={inventory} params={{ serverId }}>
            <Trans>Composes</Trans>
          </Link>
        </nav>
      }
      backLink={
        <Link className="mobile-header-button" aria-label={t`Back to Composes`} to={inventory} params={{ serverId }}>
          <Icon name="arrow-left" size={18} />
        </Link>
      }
    >
      <div className="compose-detail">
        {row ? <div className="compose-phone-actions">{actions}</div> : null}
        <LiveUpdates paused={watch.paused} retry={watch.retry} />
        {outside && stack?.sourceDirectory ? (
          <Panel
            title={t`Source directory`}
            actions={
              may(FileService.method.listFiles).allowed ? (
                <RouterButtonLink
                  to="/servers/$serverId/files"
                  params={{ serverId }}
                  search={{ path: stack.sourceDirectory }}
                  size="sm"
                  variant="ghost"
                  icon="folder"
                >
                  <Trans>Open Files</Trans>
                </RouterButtonLink>
              ) : undefined
            }
          >
            <code className="compose-directory">{stack.sourceDirectory}</code>
          </Panel>
        ) : null}
        {(object.error && !row) || (workspace.error && !workspace.data) ? (
          <Callout
            tone="critical"
            title={t`Can't read this stack`}
            actions={
              <Button size="sm" icon="refresh" onClick={refresh}>
                <Trans>Retry</Trans>
              </Button>
            }
          >
            {sentence(object.error ?? workspace.error)}
          </Callout>
        ) : loading ? (
          <p aria-busy="true" className="compose-muted">
            <Trans>Reading stack</Trans>
          </p>
        ) : row ? (
          <>
            <div className="compose-tabs">
              <TabNav label={t`Compose workspace`}>
                <RouterTabLink
                  to={stackPath}
                  params={params}
                  search={{}}
                  activeOptions={{ exact: true, includeSearch: true }}
                >
                  <Trans>Services</Trans>
                </RouterTabLink>
                <RouterTabLink
                  to={stackPath}
                  params={params}
                  search={{ view: "file" }}
                  activeOptions={{ exact: true, includeSearch: true }}
                >
                  <Trans>Compose file</Trans>
                </RouterTabLink>
                {may(LogService.method.watchLogs).allowed ? (
                  <RouterTabLink
                    to={stackPath}
                    params={params}
                    search={search.view === "logs" ? search : { view: "logs" }}
                    activeOptions={{ exact: true, includeSearch: true }}
                  >
                    <Trans>Logs</Trans>
                  </RouterTabLink>
                ) : null}
                <RouterTabLink
                  to={stackPath}
                  params={params}
                  search={{ view: "environment" }}
                  activeOptions={{ exact: true, includeSearch: true }}
                >
                  <Trans>Environment</Trans>
                </RouterTabLink>
                <RouterTabLink
                  to={stackPath}
                  params={params}
                  search={{ view: "backups" }}
                  activeOptions={{ exact: true, includeSearch: true }}
                >
                  <Trans>Backups</Trans>
                </RouterTabLink>
              </TabNav>
            </div>
            {search.view === "file" && workspace.data ? (
              <ComposeEditor key={environment} opened={workspace.data} reload={reloadBase} serverId={serverId} />
            ) : search.view === "environment" && workspace.data ? (
              <ComposeEnvironment resources={resources} omissions={workspace.data.omissions} />
            ) : search.view === "backups" && workspace.data ? (
              <ComposeBackups volumes={volumes} serverId={serverId} canBackup={canBackup} onBackup={setBackup} />
            ) : (
              <StackServices
                key={composeId}
                serverId={serverId}
                project={stack?.engineProject ?? ""}
                environment={environment}
                resources={resources}
                search={search}
                onSearch={onSearch}
                refreshStack={refresh}
                takeOn={canTake ? () => setTaking(true) : undefined}
              />
            )}
          </>
        ) : (
          <EmptyState icon="layers" title={<Trans>This stack is gone</Trans>} />
        )}
      </div>
      {(taking || search.action === "take-on") && canTake && stack ? (
        <TakeOnCompose
          project={stack.engineProject}
          server={server?.name ?? ""}
          serverId={serverId}
          onClose={closeTake}
          onApplied={(change) => {
            applied(change);
            if (change?.state === ChangeState.APPLIED) void navigate({ to: inventory, params: { serverId } });
          }}
        />
      ) : null}
      {backup && volumes.length ? (
        <Suspense fallback={null}>
          <BackUpSheet volumes={volumes} serverId={serverId} chosen={backup} onClose={() => setBackup(undefined)} />
        </Suspense>
      ) : null}
      {allowedRun && row && services.length ? (
        <Sheet
          open
          title={run === "start" ? t`Start stack` : run === "stop" ? t`Stop stack` : t`Restart stack`}
          onClose={closeWorkload}
          footer={
            <>
              <Button variant="ghost" onClick={closeWorkload}>
                <Trans>Cancel</Trans>
              </Button>
              <Button
                variant="primary"
                icon="commit"
                onClick={() => {
                  closeWorkload();
                  planWorkload(row, services, run, refresh);
                }}
              >
                <Trans>Review plan</Trans>
              </Button>
            </>
          }
        >
          <div className="compose-sheet-fields">
            {run !== "restart" && serviceGroups.length > 1 ? (
              <Callout tone="warning" title={t`These workloads are placed on several servers`}>
                <Trans>Start and Stop apply to each listed placement.</Trans>
              </Callout>
            ) : null}
            {(run === "restart" ? serviceGroups.filter(([id]) => id === serverId) : serviceGroups).map(
              ([id, items]) => (
                <Panel key={id} title={serverName(id)}>
                  <ul className="compose-keep-list">
                    {items.map((item) => (
                      <li key={item.resourceId}>
                        <code>{item.name}</code>
                        <ComposeResourceKind kind={item.kind} />
                      </li>
                    ))}
                  </ul>
                </Panel>
              ),
            )}
          </div>
        </Sheet>
      ) : null}
      {(deleting || search.action === "delete") && canDelete ? (
        <Sheet
          open
          title={t`Delete stack`}
          onClose={closeDelete}
          footer={
            <>
              <Button variant="ghost" onClick={closeDelete}>
                <Trans>Cancel</Trans>
              </Button>
              <Button variant="danger" icon="commit" disabled={chosen.partial} onClick={deleteStack}>
                <Trans>Review deletion</Trans>
              </Button>
            </>
          }
        >
          <div className="compose-sheet-fields">
            <Callout tone="warning" title={t`Remove the stack's workloads and networks`}>
              {chosen.partial ? (
                <Trans>Select every placement of a volume to remove it.</Trans>
              ) : chosen.all.length ? (
                <Trans>The selected volumes and their data will be removed.</Trans>
              ) : (
                <Trans>Volumes and their data stay.</Trans>
              )}
            </Callout>
            {deleteGroups.map(([id, items]) => (
              <Panel key={id} title={serverName(id)}>
                <ul className="compose-keep-list">
                  {items.map((item) => (
                    <li key={item.resourceId}>
                      {item.kind === "volume" ? (
                        <label className="compose-choice">
                          <input
                            type="checkbox"
                            checked={volumeCopies.has(copyKey(item, id))}
                            onChange={(event) => {
                              const key = copyKey(item, id);
                              const next = new Set(volumeCopies);
                              if (event.target.checked) next.add(key);
                              else next.delete(key);
                              setVolumeSelection({ snapshot, copies: next });
                            }}
                          />
                          <span>
                            <Trans>Include volume {item.name}</Trans>
                          </span>
                        </label>
                      ) : (
                        <code>{item.name}</code>
                      )}
                      <ComposeResourceKind kind={item.kind} />
                    </li>
                  ))}
                </ul>
              </Panel>
            ))}
          </div>
        </Sheet>
      ) : null}
    </Page>
  );
}

interface StackServiceRow {
  id: string;
  name: string;
  resourceId: string;
  container?: DockerObject;
}

function StackServices({
  serverId,
  project,
  environment,
  resources,
  search,
  onSearch,
  refreshStack,
  takeOn,
}: {
  serverId: string;
  project: string;
  environment: string;
  resources: readonly ComposeWorkspaceResource[];
  search: ComposeDetailSearch;
  onSearch: StackProps["onSearch"];
  refreshStack: () => void;
  takeOn?: (() => void) | undefined;
}) {
  const { t } = useLingui();
  const compact = useMediaQuery("(max-width: 1000px)");
  const [tokens, setTokens] = useState([""]);
  const list = useQuery(
    DockerService.method.listDockerObjects,
    {
      serverIds: [serverId],
      kind: DockerObjectKind.CONTAINER,
      filter: {
        composeProject: environment ? `managed:${environment}` : project,
        ownership: environment ? DockerOwnership.MANAGED : DockerOwnership.EXTERNAL,
      },
      pageSize: 20,
      pageToken: tokens.at(-1) ?? "",
    },
    {
      enabled:
        (environment || project) !== "" && (search.view === undefined || (!environment && search.view === "logs")),
      retry: false,
      refetchOnWindowFocus: false,
    },
  );
  const observed = list.data?.objects.filter((row) => row.key && !containerOf(row)?.oneOff) ?? [];
  const services = resources.filter((item) => item.kind === "service");
  if (search.view === "logs" && environment) {
    return (
      <StackLogs
        sources={services.map((s) => ({ id: s.resourceId, name: s.name }))}
        search={search}
        onSearch={onSearch}
      />
    );
  }
  if (search.view !== undefined && search.view !== "logs") {
    return (
      <EmptyState
        icon="lock"
        title={t`Take on this project to manage it`}
        action={
          takeOn ? (
            <Button icon="download" onClick={takeOn}>
              <Trans>Take on</Trans>
            </Button>
          ) : undefined
        }
      />
    );
  }
  if (list.error) {
    return (
      <Callout tone="critical" title={t`Can't read these services`}>
        {sentence(list.error)}
      </Callout>
    );
  }
  if (list.isPending) {
    return (
      <p aria-busy="true" className="compose-muted">
        <Trans>Reading services</Trans>
      </p>
    );
  }
  const rows: StackServiceRow[] = observed.map((row) => ({
    id: row.key?.engineId ?? "",
    name: row.managedResource?.name || containerOf(row)?.composeService || row.name,
    resourceId: row.managedResourceId,
    container: row,
  }));
  if (tokens.length === 1 && !list.data?.nextPageToken) {
    for (const service of services) {
      if (
        service.serverIds.includes(serverId) &&
        !observed.some((row) => row.managedResourceId === service.resourceId)
      ) {
        rows.push({ id: service.resourceId, name: service.name, resourceId: service.resourceId });
      }
    }
  }
  rows.sort((a, b) => a.name.localeCompare(b.name) || a.id.localeCompare(b.id));
  const serviceName = ({ name, resourceId }: StackServiceRow) =>
    resourceId ? (
      <Link className="dw-link compose-row-title" to="/apps/$serviceId" params={{ serviceId: resourceId }} title={name}>
        <Icon name="service" size={16} />
        <span className="compose-clip">{name}</span>
      </Link>
    ) : (
      <strong className="compose-clip" title={name}>
        {name}
      </strong>
    );
  const instance = ({ container }: StackServiceRow) =>
    container?.key ? (
      <Link
        className="dw-link compose-instance compose-clip"
        title={container.name}
        to="/servers/$serverId/docker/containers/$containerId"
        params={{ serverId, containerId: container.key.engineId }}
        search={{ name: container.name }}
      >
        {container.name}
      </Link>
    ) : (
      <span className="compose-muted">
        <Trans>No container</Trans>
      </span>
    );
  const image = ({ container }: StackServiceRow) => {
    const value = container && containerOf(container)?.image;
    if (!value) return null;
    const [ref = value, digest] = value.split("@", 2);
    const parts = ref.split("/");
    if (!compact && (parts.length > 2 || digest)) {
      const name = parts.at(-1);
      const registry = parts.length > 2 ? `${parts[0]}/…` : parts.slice(0, -1).join("/");
      return (
        <code className="compose-image" title={value}>
          <span className="compose-clip">{name}</span>
          <span className="compose-clip compose-muted">
            {[registry, digest?.slice(0, 19)].filter(Boolean).join(" · ")}
          </span>
        </code>
      );
    }
    return (
      <code className="compose-image compose-clip" title={value}>
        {value}
      </code>
    );
  };
  const state = ({ container }: StackServiceRow) =>
    container ? (
      <ContainerStatus row={container} />
    ) : (
      <StatusBadge status="unknown" label={t`Unavailable`} labelIsState />
    );
  const ports = ({ container }: StackServiceRow) => {
    const body = container && containerOf(container);
    const values = body?.ports.map(dockerPort) ?? [];
    return values.length ? (
      <div className="compose-ports" title={values.join(", ")}>
        {[...new Set(values)].map((value) => (
          <code className="compose-clip" key={value}>
            {value}
          </code>
        ))}
      </div>
    ) : (
      <span className="compose-muted">{container ? t`None` : t`Unavailable`}</span>
    );
  };
  const restarts = ({ container }: StackServiceRow) => {
    const body = container && containerOf(container);
    return body?.startedAt ? body.restartCount : <span className="compose-muted">{t`Unavailable`}</span>;
  };
  const operations = ({ container }: StackServiceRow) =>
    container && environment ? (
      <ContainerActions
        row={container}
        shortcuts={false}
        workloadServers={resources.find((item) => item.resourceId === container.managedResourceId)?.serverIds.length}
        onApplied={() => {
          refreshStack();
          void list.refetch();
        }}
      />
    ) : null;
  const columns: TableColumn<StackServiceRow>[] = [
    {
      key: "service",
      header: t`Service`,
      width: "21%",
      render: (row) => (
        <div className="compose-service-name">
          {serviceName(row)}
          {instance(row)}
        </div>
      ),
    },
    { key: "image", header: t`Image`, width: "26%", render: image },
    { key: "ports", header: t`Published ports`, render: ports },
    { key: "state", header: t`State`, width: 106, render: state },
    { key: "restarts", header: t`Restarts`, width: 105, align: "right", render: restarts },
    ...(environment
      ? [{ key: "actions", header: <span className="dw-sr">{t`Actions`}</span>, width: 62, render: operations }]
      : []),
  ];
  return (
    <>
      {search.view === "logs" ? (
        <StackLogs
          sources={observed.map((row) => ({
            id: row.key?.engineId ?? "",
            name: containerOf(row)?.composeService || row.name,
            server: serverId,
          }))}
          search={search}
          onSearch={onSearch}
        />
      ) : rows.length === 0 ? (
        <EmptyState icon="service" title={t`No services to show`} />
      ) : compact ? (
        <ul className="compose-cards compose-service-cards" aria-label={t`Services`}>
          {rows.map((row) => (
            <li key={row.id}>
              <div className="compose-card-title">
                {serviceName(row)}
                {operations(row)}
              </div>
              {instance(row)}
              {image(row)}
              <div className="compose-row-meta">
                {state(row)}
                <span className="compose-muted">
                  <Trans>Restarts</Trans> {restarts(row)}
                </span>
              </div>
              <div className="compose-service-ports">
                <span className="compose-muted">
                  <Trans>Published ports</Trans>
                </span>
                {ports(row)}
              </div>
            </li>
          ))}
        </ul>
      ) : (
        <Table className="compose-table" columns={columns} rows={rows} caption={t`Services`} />
      )}
      {tokens.length > 1 || list.data?.nextPageToken ? (
        <div className="compose-actions compose-paging">
          <Button size="sm" disabled={tokens.length === 1} onClick={() => setTokens(tokens.slice(0, -1))}>
            <Trans>Previous page</Trans>
          </Button>
          <Button
            size="sm"
            disabled={!list.data?.nextPageToken}
            onClick={() => setTokens([...tokens, list.data?.nextPageToken ?? ""])}
          >
            <Trans>Next page</Trans>
          </Button>
        </div>
      ) : null}
    </>
  );
}

function StackLogs({
  sources,
  search,
  onSearch,
}: {
  sources: readonly { id: string; name: string; server?: string }[];
  search: ComposeDetailSearch;
  onSearch: StackProps["onSearch"];
}) {
  const { t } = useLingui();
  const selected = sources.find((source) => source.id === search.service) ?? sources[0];
  if (!selected) return <EmptyState icon="terminal" title={t`No service logs here`} />;
  return (
    <div className="compose-detail compose-logs">
      <SelectField
        label={t`Service logs`}
        value={selected.id}
        options={sources.map((source) => ({ value: source.id, label: source.name }))}
        onChange={(event) => onSearch({ view: "logs", service: event.target.value })}
      />
      <Suspense fallback={null}>
        <Logs
          key={selected.id}
          id={selected.id}
          server={selected.server ?? ""}
          search={search}
          onSearch={(next) => onSearch({ ...next, view: "logs", service: selected.id })}
        />
      </Suspense>
    </div>
  );
}

function ComposeEnvironment({
  resources,
  omissions,
}: {
  resources: readonly ComposeWorkspaceResource[];
  omissions: GetComposeWorkspaceResponse["omissions"];
}) {
  const { t } = useLingui();
  const services = useServices().data ?? [];
  const apps = new Map(services.map((app) => [app.id, app] as const));
  const fields = (app: ServiceInfo | undefined) => Object.keys((app ? appOf(app)?.env : undefined) ?? {}).sort();
  return (
    <div className="compose-detail">
      {resources
        .filter((res) => res.kind === "service")
        .map((item) => {
          const kept = omissions.filter(
            (omit) => omit.name === item.name && omit.kind === "service" && omit.path.startsWith("environment."),
          );
          const other = fields(apps.get(item.resourceId)).filter(
            (key) => !kept.some((entry) => entry.path === `environment.${key}`),
          );
          return (
            <Panel
              key={item.resourceId}
              title={item.name}
              actions={
                <RouterButtonLink
                  size="sm"
                  variant="ghost"
                  to="/apps/$serviceId/environment"
                  params={{ serviceId: item.resourceId }}
                >
                  <Trans>Open environment</Trans>
                </RouterButtonLink>
              }
            >
              {kept.length ? <KeptValues omissions={kept} /> : null}
              {other.length ? (
                <ul className="compose-keep-list">
                  {other.map((name) => (
                    <li key={name}>
                      <code>{name}</code>
                      <Tag words>
                        <Trans>Set in Compose</Trans>
                      </Tag>
                    </li>
                  ))}
                </ul>
              ) : null}
              {!kept.length && !other.length ? <span className="compose-muted">{t`No variables set`}</span> : null}
            </Panel>
          );
        })}
    </div>
  );
}

function ComposeBackups({
  volumes,
  serverId,
  canBackup,
  onBackup,
}: {
  volumes: readonly { id: string; name: string; environment: string }[];
  serverId: string;
  canBackup: boolean;
  onBackup: (id: string) => void;
}) {
  const { t } = useLingui();
  if (!volumes.length) return <EmptyState icon="archive" title={t`No managed volumes to back up`} />;
  return (
    <ul className="compose-cards" aria-label={t`Backup sources`}>
      {volumes.map((volume) => (
        <ComposeBackupRow
          key={volume.id}
          volume={volume}
          serverId={serverId}
          canBackup={canBackup}
          onBackup={onBackup}
        />
      ))}
    </ul>
  );
}

function ComposeBackupRow({
  volume,
  serverId,
  canBackup,
  onBackup,
}: {
  volume: { id: string; name: string };
  serverId: string;
  canBackup: boolean;
  onBackup: (id: string) => void;
}) {
  const { t } = useLingui();
  const { backup, method, pending } = useBackupRows(volume.id, serverId);
  return (
    <li>
      <div className="compose-row-meta">
        <span className="compose-row-title">
          <Icon name="archive" size={16} />
          <code>{volume.name}</code>
        </span>
        {method ? <Tag words>{method}</Tag> : null}
      </div>
      {backup ? (
        <Link className="compose-backup-link" to="/backups/$backupId" params={{ backupId: backup.id }}>
          <Trans>Latest backup</Trans>
        </Link>
      ) : (
        <span className="compose-muted">{pending ? t`Checking backup source` : t`No completed backup`}</span>
      )}
      {canBackup ? (
        <div className="compose-actions">
          <Button size="sm" icon="archive" onClick={() => onBackup(volume.id)}>
            <Trans>Preview backup</Trans>
          </Button>
        </div>
      ) : null}
    </li>
  );
}

function TakeOnCompose({
  project,
  server,
  serverId,
  onClose,
  onApplied,
}: {
  project: string;
  server: string;
  serverId: string;
  onClose: () => void;
  onApplied: (change: { state: ChangeState } | undefined) => void;
}) {
  const { t } = useLingui();
  const transport = useTransport();
  const sheet = useChangeSheet();
  const [hostAccess, setHostAccess] = useState(false);
  const [guessablePassword, setGuessablePassword] = useState(false);
  const review = () => {
    onClose();
    sheet.open({
      kind: "plan",
      make: () =>
        createClient(ComposeService, transport).adoptCompose({
          name: project,
          server: serverId,
          hostAccess,
          guessablePassword,
        }),
      onClose: onApplied,
    });
  };
  return (
    <Sheet
      open
      title={t`Take on ${project}`}
      onClose={onClose}
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            <Trans>Cancel</Trans>
          </Button>
          <Button variant="primary" icon="commit" onClick={review}>
            <Trans>Review plan</Trans>
          </Button>
        </>
      }
    >
      <div className="compose-sheet-fields">
        <Callout tone="info" title={t`Take on the whole project`}>
          <Trans>Containers on {server} keep their state, networks and data.</Trans>
        </Callout>
        <details className="compose-disclosure">
          <summary>
            <Icon name="chevron-right" size={14} className="compose-disclosure-caret" />
            <Icon name="settings" size={15} />
            <Trans>Advanced</Trans>
          </summary>
          <label>
            <input type="checkbox" checked={hostAccess} onChange={(event) => setHostAccess(event.target.checked)} />{" "}
            <Trans>Keep existing host access</Trans>
          </label>
          <label>
            <input
              type="checkbox"
              checked={guessablePassword}
              onChange={(event) => setGuessablePassword(event.target.checked)}
            />{" "}
            <Trans>Keep guessable database passwords</Trans>
          </label>
        </details>
      </div>
    </Sheet>
  );
}
