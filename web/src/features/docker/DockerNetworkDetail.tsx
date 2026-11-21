// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { useQuery as useConnectQuery, useTransport } from "@connectrpc/connect-query";
import {
  DockerNetworkAttachmentAction,
  type DockerObject,
  DockerObjectKind,
  DockerOwnership,
  DockerService,
  DockerStorageProtection,
} from "@dockwell/api";
import { Button, Callout, Loading, Panel, SelectField, Sheet, SkeletonField, TextField } from "@dockwell/ui";
import { useLingui } from "@lingui/react/macro";
import { keepPreviousData, useQuery } from "@tanstack/react-query";
import { useNavigate } from "@tanstack/react-router";
import { useState } from "react";
import { sentence } from "../../lib/errors";
import { useMay } from "../auth/may";
import { useChangeSheet } from "../changes/ChangeSheet";
import { ActionMenu, type MenuAction } from "../shell/ActionMenu";
import { EmptyState } from "../shell/EmptyState";
import { formatMeasurement, NetworkPools } from "./DockerStorage";
import { DockerTakeOn } from "./DockerTakeOn";
import { useDockerWatch } from "./live";
import { containerOf, Fact, LiveUpdates } from "./parts";
import { StorageAttachments, StorageDetailLoading, StoragePage } from "./storageParts";

function usableNetwork(row: DockerObject | undefined) {
  return (
    row?.detail.case === "network" &&
    row.detail.value.driver === "bridge" &&
    row.detail.value.scope === "local" &&
    !row.detail.value.protection
  );
}

function alreadyAttached(container: DockerObject | undefined, network: DockerObject | undefined) {
  return containerOf(container)?.endpoints.some((ep) => ep.network === network?.name);
}

function eligibleNetwork(row: DockerObject | undefined, container: DockerObject | undefined) {
  return (
    usableNetwork(row) &&
    !alreadyAttached(container, row) &&
    (row?.ownership !== DockerOwnership.MANAGED ||
      container?.managedResource?.kind === "container" ||
      row.managedResource?.scope === container?.managedResource?.scope)
  );
}

function useDockerChoices(serverId: string, kind: DockerObjectKind, search = "", enabled = true) {
  const transport = useTransport();
  return useQuery({
    queryKey: ["docker-attachment-choices", serverId, kind, search],
    queryFn: async ({ signal }) => {
      const client = createClient(DockerService, transport);
      const result: DockerObject[] = [];
      let pageToken = "";
      for (let page = 0; page < 5; page++) {
        const data = await client.listDockerObjects(
          { serverIds: [serverId], kind, pageToken, filter: { search } },
          { signal },
        );
        result.push(...data.objects);
        pageToken = data.nextPageToken;
        if (!pageToken) break;
      }
      return result;
    },
    enabled,
    placeholderData: keepPreviousData,
    retry: false,
    refetchOnWindowFocus: false,
  });
}

export function DockerNetworkDetail({ serverId, networkId }: { serverId: string; networkId: string }) {
  return <NetworkDetail key={`${serverId}/${networkId}`} serverId={serverId} networkId={networkId} />;
}

function NetworkDetail({ serverId, networkId }: { serverId: string; networkId: string }) {
  const { t, i18n } = useLingui();
  const may = useMay();
  const allowed = may(DockerService.method.getDockerObject).allowed;
  const mayAttach = may(DockerService.method.planDockerNetworkAttachment).allowed;
  const mayRemove = may(DockerService.method.planDockerMaintenance).allowed;
  const navigate = useNavigate();
  const transport = useTransport();
  const sheet = useChangeSheet();
  const query = useConnectQuery(
    DockerService.method.getDockerObject,
    {
      key: { serverId, engineId: networkId, kind: DockerObjectKind.NETWORK },
    },
    { enabled: allowed, retry: false },
  );
  const row = query.data?.object;
  const net = row?.detail.case === "network" ? row.detail.value : undefined;
  const choices = useDockerChoices(serverId, DockerObjectKind.CONTAINER, "", allowed && !!net?.attachments.length);
  const containers = new Map(choices.data?.map((item) => [item.key?.engineId, item]));
  const [picker, setPicker] = useState<{ container?: DockerObject }>();
  const [taking, setTaking] = useState<DockerObject>();
  const refresh = () => {
    void query.refetch();
    if (net?.attachments.length) void choices.refetch();
  };
  const watch = useDockerWatch(allowed ? [serverId] : [], [], refresh, DockerObjectKind.NETWORK);
  const plan = (container: DockerObject, network: DockerObject, action: DockerNetworkAttachmentAction) => {
    setPicker(undefined);
    sheet.open({
      kind: "plan",
      make: () =>
        createClient(DockerService, transport).planDockerNetworkAttachment({
          network: network.key,
          container: container.key,
          baseRevision: container.managedRevision,
          action,
        }),
      onClose: refresh,
    });
  };
  const removeReason =
    net?.protection === DockerStorageProtection.BUILT_IN
      ? t`Docker needs this built-in network`
      : net?.protection === DockerStorageProtection.INFRASTRUCTURE
        ? t`Dockwell needs this network`
        : net?.protection === DockerStorageProtection.SHARED_LINK
          ? t`This network is a shared link`
          : row?.ownership === DockerOwnership.MANAGED
            ? t`Managed networks are removed through a revision plan`
            : net?.attachmentCount
              ? t`Containers still use this network`
              : net?.attachmentsPartial || net?.attachmentCount === undefined
                ? t`Docker couldn't verify all attachments`
                : net.scope !== "local"
                  ? t`Only local networks can be removed here`
                  : undefined;
  const remove = () =>
    void navigate({
      to: "/servers/$serverId/docker/cleanup",
      params: { serverId },
      search: { group: "networks" },
    });
  const headerActions =
    row && net && (mayAttach || mayRemove) ? (
      <div className="docker-storage-actions">
        {mayAttach && usableNetwork(row) ? (
          <Button size="sm" variant="primary" icon="plus" onClick={() => setPicker({})}>{t`Connect container`}</Button>
        ) : null}
        {mayRemove && !removeReason ? (
          <Button size="sm" variant="secondary" icon="trash" onClick={remove}>{t`Review cleanup`}</Button>
        ) : mayRemove ? (
          <ActionMenu
            label={t`Network actions`}
            actions={[{ id: "cleanup", label: t`Review cleanup`, icon: "trash", off: removeReason, onSelect: remove }]}
          />
        ) : null}
      </div>
    ) : undefined;
  const attachmentActions = (id: string): MenuAction[] => {
    const item = containers.get(id);
    if (!row || !item || !mayAttach || !usableNetwork(row)) return [];
    if (item.ownership === DockerOwnership.EXTERNAL) {
      return [{ id: "take-on", label: t`Take on`, onSelect: () => setTaking(item) }];
    }
    const body = containerOf(item);
    if (
      item.ownership !== DockerOwnership.MANAGED ||
      body?.detailsTruncated ||
      (row.ownership === DockerOwnership.MANAGED &&
        item.managedResource?.kind !== "container" &&
        item.managedResource?.scope !== row.managedResource?.scope)
    )
      return [];
    const last = body?.endpoints.length === 1;
    return [
      {
        id: "disconnect",
        label: t`Disconnect`,
        off: last ? t`This is the workload's last network` : undefined,
        onSelect: () => plan(item, row, DockerNetworkAttachmentAction.DISCONNECT),
      },
      ...(last
        ? [
            {
              id: "connect",
              label: t`Connect another network`,
              icon: "plus" as const,
              onSelect: () => setPicker({ container: item }),
            },
          ]
        : []),
    ];
  };
  return (
    <StoragePage serverId={serverId} row={row} actions={headerActions} pending={allowed && query.isPending}>
      <LiveUpdates paused={watch.paused} retry={watch.retry} />
      {!allowed ? (
        <EmptyState icon="lock" title={t`Docker inventory needs an admin`} />
      ) : query.isPending ? (
        <StorageDetailLoading />
      ) : query.error ? (
        <Callout
          tone="warning"
          title={t`Network couldn't be read`}
          actions={<Button size="sm" onClick={refresh}>{t`Retry`}</Button>}
        >
          {sentence(query.error)}
        </Callout>
      ) : !row || !net ? (
        <EmptyState icon="network" title={t`This network is gone`} />
      ) : (
        <>
          <StorageAttachments
            serverId={serverId}
            title={t`Attached containers`}
            empty={t`No containers attached`}
            partial={net.attachmentsPartial}
            rows={net.attachments.map((endpoint) => ({ ...endpoint, id: endpoint.containerId }))}
            actions={(endpoint) => attachmentActions(endpoint.containerId)}
          />
          <Panel title={t`Network details`}>
            <dl className="docker-facts">
              <Fact label={t`Driver`} value={net.driver} mono />
              <Fact label={t`Scope`} value={net.scope} />
              {net.createdAt ? (
                <Fact label={t`Created`} value={formatMeasurement(net.createdAt.seconds, i18n.locale)} />
              ) : null}
              {net.internal ? <Fact label={t`Internal network`} value={t`On`} /> : null}
              <Fact label={t`IPv6`} value={net.ipv6Enabled ? t`On` : t`Off`} />
              {net.labelKeys.length ? <Fact label={t`Labels`} value={net.labelKeys.join(", ")} mono /> : null}
              {net.optionKeys.length ? <Fact label={t`Driver options`} value={net.optionKeys.join(", ")} mono /> : null}
              <Fact label={t`Address pools`} value={<NetworkPools row={row} />} />
            </dl>
          </Panel>
          {picker ? (
            <ConnectPicker
              serverId={serverId}
              network={row}
              preselected={picker.container}
              onClose={() => setPicker(undefined)}
              onPlan={(container, network) => plan(container, network, DockerNetworkAttachmentAction.CONNECT)}
            />
          ) : null}
          {taking ? <DockerTakeOn row={taking} onClose={() => setTaking(undefined)} onApplied={refresh} /> : null}
        </>
      )}
    </StoragePage>
  );
}

function ConnectPicker({
  serverId,
  network,
  preselected,
  onClose,
  onPlan,
}: {
  serverId: string;
  network: DockerObject;
  preselected?: DockerObject | undefined;
  onClose: () => void;
  onPlan: (container: DockerObject, network: DockerObject) => void;
}) {
  const { t } = useLingui();
  const [search, setSearch] = useState("");
  const [selected, setSelected] = useState("");
  const [taking, setTaking] = useState<DockerObject>();
  const query = useDockerChoices(serverId, preselected ? DockerObjectKind.NETWORK : DockerObjectKind.CONTAINER, search);
  const rows = (query.data ?? []).filter((row) =>
    preselected
      ? eligibleNetwork(row, preselected)
      : row.ownership !== DockerOwnership.SYSTEM &&
        !alreadyAttached(row, network) &&
        (row.ownership === DockerOwnership.EXTERNAL || eligibleNetwork(network, row)),
  );
  const choice = rows.find((row) => row.key?.engineId === selected);
  const container = preselected ?? choice;
  const target = preselected ? choice : network;
  if (taking) {
    return <DockerTakeOn row={taking} onClose={() => setTaking(undefined)} onApplied={() => void query.refetch()} />;
  }
  return (
    <Sheet
      open
      title={preselected ? t`Connect another network` : t`Connect container`}
      onClose={onClose}
      footer={
        <>
          <Button variant="secondary" onClick={onClose}>{t`Cancel`}</Button>
          <Button
            disabled={!container || !target || query.isPlaceholderData}
            onClick={() => {
              if (!container || !target) return;
              if (container.ownership === DockerOwnership.EXTERNAL) setTaking(container);
              else onPlan(container, target);
            }}
          >
            {container?.ownership === DockerOwnership.EXTERNAL ? t`Take on` : t`Review plan`}
          </Button>
        </>
      }
    >
      <div className="docker-storage-form">
        {preselected ? (
          <p>
            {t`Container`} <code>{preselected.name}</code>
          </p>
        ) : (
          <p>
            {t`Network`} <code>{network.name}</code>
          </p>
        )}
        <TextField
          label={preselected ? t`Search networks` : t`Search containers`}
          value={search}
          maxLength={255}
          onChange={(event) => {
            setSearch(event.target.value);
            setSelected("");
          }}
        />
        {query.isPending ? (
          <Loading label={t`Loading choices`} slow={t`Docker is slow to answer on this server`}>
            <SkeletonField />
          </Loading>
        ) : query.error ? (
          <Callout
            tone="warning"
            title={t`Choices couldn't be read`}
            actions={<Button size="sm" onClick={() => void query.refetch()}>{t`Retry`}</Button>}
          >
            {sentence(query.error)}
          </Callout>
        ) : (
          <SelectField
            label={preselected ? t`Network` : t`Container`}
            value={selected}
            disabled={query.isPlaceholderData}
            options={[
              { value: "", label: preselected ? t`Choose a network` : t`Choose a container` },
              ...rows.map((row) => ({ value: row.key?.engineId ?? "", label: row.name })),
            ]}
            onChange={(event) => setSelected(event.target.value)}
          />
        )}
        {!query.isPending && !query.isPlaceholderData && !query.error && !rows.length ? (
          <p>{t`No matching choices`}</p>
        ) : null}
      </div>
    </Sheet>
  );
}
