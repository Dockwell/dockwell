// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import {
  ChangeService,
  ChangeState,
  DockerNetworkCheckCode,
  DockerObjectKind,
  DockerService,
  type Server,
} from "@dockwell/api";
import { Button, Callout, Loading, SelectField, Sheet, SkeletonField, TextField } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { useQuery } from "@tanstack/react-query";
import { useNavigate } from "@tanstack/react-router";
import { useEffect, useState } from "react";
import { sentence } from "../../lib/errors";
import { usePlaces } from "../apps/useServices";
import { useMay } from "../auth/may";
import { useChangeSheet } from "../changes/ChangeSheet";

export function DockerStorageCreate({
  servers,
  volumes,
  onClose,
  refresh,
}: {
  volumes: boolean;
  servers: readonly Pick<Server, "id" | "name">[];
  onClose: () => void;
  refresh: () => void;
}) {
  const { t } = useLingui();
  const places = usePlaces();
  const mayCheck = useMay()(DockerService.method.checkDockerNetwork).allowed;
  const transport = useTransport();
  const sheet = useChangeSheet();
  const navigate = useNavigate();
  const [name, setName] = useState("");
  const [serverChoice, setServerChoice] = useState("");
  const [environmentChoice, setEnvironmentChoice] = useState("");
  const [isolated, setIsolated] = useState(false);
  const [ipv6, setIPv6] = useState(false);
  const [subnet, setSubnet] = useState("");
  const [gateway, setGateway] = useState("");
  const [subnet6, setSubnet6] = useState("");
  const [gateway6, setGateway6] = useState("");
  const server = serverChoice || (servers.length === 1 ? (servers[0]?.id ?? "") : "");
  const environment = environmentChoice || (places.environments.length === 1 ? (places.environments[0]?.id ?? "") : "");
  const network = {
    driver: "bridge",
    internal: isolated,
    ...(ipv6 || subnet || gateway
      ? {
          ipam: {
            enableIpv6: ipv6,
            pools: [
              ...(subnet || gateway ? [{ subnet, gateway }] : []),
              ...(ipv6 && (subnet6 || gateway6) ? [{ subnet: subnet6, gateway: gateway6 }] : []),
            ],
          },
        }
      : {}),
  };
  const [candidate, setCandidate] = useState("");
  const valid = /^[A-Za-z0-9][A-Za-z0-9_.-]{0,62}$/.test(name) && !!server && !!environment;
  const wanted =
    !volumes && valid ? JSON.stringify({ server, candidate: { environmentId: environment, name, spec: network } }) : "";
  useEffect(() => {
    if (wanted === candidate) return;
    const timer = setTimeout(() => setCandidate(wanted), 220);
    return () => clearTimeout(timer);
  }, [wanted, candidate]);
  const checked = useQuery({
    queryKey: ["docker-network-check", candidate],
    queryFn: ({ signal }) => {
      const parsed = JSON.parse(candidate) as {
        server: string;
        candidate: { environmentId: string; name: string; spec: typeof network };
      };
      return createClient(DockerService, transport).checkDockerNetwork(
        { serverIds: [parsed.server], candidate: parsed.candidate },
        { signal },
      );
    },
    enabled: mayCheck && !!candidate,
    retry: false,
  });
  const checking = mayCheck && !!wanted && (wanted !== candidate || checked.isFetching);
  const finding = !checking && wanted === candidate ? checked.data?.findings[0] : undefined;
  const message =
    finding?.code === DockerNetworkCheckCode.NAME_IN_USE
      ? t`This network name is in use`
      : finding?.code === DockerNetworkCheckCode.SUBNET_OVERLAP
        ? t`Overlaps ${finding.conflictingSubnet} on ${finding.conflictingNetwork}`
        : finding?.code === DockerNetworkCheckCode.INVALID_POOL
          ? t`Enter one valid subnet per address family`
          : finding?.code === DockerNetworkCheckCode.INVALID_GATEWAY
            ? t`Choose a gateway inside its subnet`
            : finding
              ? t`The Engine refused these settings`
              : "";
  const ready = volumes
    ? valid
    : !!wanted &&
      wanted === candidate &&
      checked.data &&
      mayCheck &&
      !checked.isFetching &&
      !checked.error &&
      !checked.data.findings.length &&
      !checked.data.unavailable.length;
  return (
    <Sheet
      open
      title={volumes ? t`New volume` : t`New network`}
      onClose={onClose}
      footer={
        <>
          <Button variant="secondary" onClick={onClose}>{t`Cancel`}</Button>
          <Button
            disabled={!ready}
            onClick={() => {
              onClose();
              sheet.open({
                kind: "plan",
                make: () =>
                  createClient(ChangeService, transport).proposeChange({
                    summary: volumes ? t`Create volume ${name}` : t`Create network ${name}`,
                    edits: [
                      {
                        resource: { kind: volumes ? "volume" : "network", scope: environment, name },
                        servers: [server],
                        mustCreate: true,
                        spec: {
                          kind: volumes
                            ? { case: "volume", value: { driver: "local" } }
                            : { case: "network", value: network },
                        },
                      },
                    ],
                  }),
                onClose: (change) => {
                  refresh();
                  if (change?.state !== ChangeState.APPLIED) return;
                  const wanted = change.plan?.servers
                    .find((s) => s.serverId === server)
                    ?.steps.find((step) => step.resource?.name === name);
                  const operation = wanted?.dockerStorageGuard?.operation;
                  if (!wanted || operation?.case !== "create") return;
                  const engineName = operation.value.engineName;
                  void createClient(DockerService, transport)
                    .listDockerObjects({
                      serverIds: [server],
                      kind: volumes ? DockerObjectKind.VOLUME : DockerObjectKind.NETWORK,
                      filter: { search: engineName },
                    })
                    .then(({ objects }) => {
                      const key = objects.find(
                        (row) => row.name === engineName && row.managedResourceId === wanted.resourceId,
                      )?.key;
                      if (!key) return;
                      if (volumes) {
                        void navigate({
                          to: "/servers/$serverId/docker/volumes/$volumeKey",
                          params: { serverId: server, volumeKey: key.engineId },
                        });
                      } else {
                        void navigate({
                          to: "/servers/$serverId/docker/networks/$networkId",
                          params: { serverId: server, networkId: key.engineId },
                        });
                      }
                    })
                    .catch(() => refresh());
                },
              });
            }}
          >{t`Review plan`}</Button>
        </>
      }
    >
      <div className="docker-storage-form">
        <TextField
          label={volumes ? t`Volume name` : t`Network name`}
          value={name}
          maxLength={63}
          onChange={(event) => setName(event.target.value)}
        />
        {places.isPending ? (
          <Loading label={t`Loading environments`} slow={t`The panel is slow to list environments`}>
            <SkeletonField />
          </Loading>
        ) : (
          <SelectField
            label={t`Environment`}
            value={environment}
            options={[
              ...(places.environments.length !== 1 ? [{ value: "", label: t`Choose an environment` }] : []),
              ...places.environments.map((p) => ({ value: p.id, label: p.label })),
            ]}
            onChange={(event) => setEnvironmentChoice(event.target.value)}
          />
        )}
        <SelectField
          label={t`Server`}
          value={server}
          options={[
            ...(servers.length !== 1 ? [{ value: "", label: t`Choose a server` }] : []),
            ...servers.map((s) => ({ value: s.id, label: s.name })),
          ]}
          onChange={(event) => setServerChoice(event.target.value)}
        />
        {volumes ? (
          <p className="docker-storage-muted">{t`Local driver`}</p>
        ) : (
          <details>
            <summary>{t`Bridge · Advanced`}</summary>
            <div className="docker-storage-form">
              <label>
                <input type="checkbox" checked={isolated} onChange={(event) => setIsolated(event.target.checked)} />{" "}
                {t`Internal network`}
              </label>
              <TextField
                label={t`IPv4 subnet`}
                value={subnet}
                placeholder={t`Automatic`}
                onChange={(event) => setSubnet(event.target.value)}
              />
              <TextField
                label={t`IPv4 gateway`}
                value={gateway}
                placeholder={t`Automatic`}
                onChange={(event) => setGateway(event.target.value)}
              />
              <label>
                <input type="checkbox" checked={ipv6} onChange={(event) => setIPv6(event.target.checked)} />{" "}
                {t`Enable IPv6`}
              </label>
              {ipv6 ? (
                <>
                  <TextField
                    label={t`IPv6 subnet`}
                    value={subnet6}
                    placeholder={t`Automatic`}
                    onChange={(event) => setSubnet6(event.target.value)}
                  />
                  <TextField
                    label={t`IPv6 gateway`}
                    value={gateway6}
                    placeholder={t`Automatic`}
                    onChange={(event) => setGateway6(event.target.value)}
                  />
                </>
              ) : null}
            </div>
          </details>
        )}
        {checking ? (
          <Loading label={t`Checking network settings`} slow={t`Docker is slow to answer on this server`}>
            <SkeletonField />
          </Loading>
        ) : wanted === candidate && checked.error ? (
          <Callout
            tone="warning"
            title={t`Network check failed`}
            actions={<Button size="sm" onClick={() => void checked.refetch()}>{t`Retry`}</Button>}
          >
            {sentence(checked.error)}
          </Callout>
        ) : null}
        {!checking && wanted === candidate && checked.data?.unavailable.length ? (
          <p role="status">{t`The selected Engine couldn't check this network`}</p>
        ) : null}
        {message ? <p role="status">{message}</p> : null}
        {!volumes && ready ? (
          <p role="status">
            <Trans>Checked on the selected server</Trans>
          </p>
        ) : null}
      </div>
    </Sheet>
  );
}
