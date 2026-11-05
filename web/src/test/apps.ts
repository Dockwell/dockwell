// SPDX-License-Identifier: AGPL-3.0-only

import { create, type MessageInitShape } from "@bufbuild/protobuf";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import {
  ChangeState,
  type Container,
  ContainerSchema,
  DeploymentState,
  FieldOp,
  HealthCheckKind,
  HealthState,
  type Resource,
  ResourceSchema,
  type ServiceHealth,
  ServiceHealthSchema,
  type ServiceInfo,
  ServiceInfoSchema,
  type ServiceRevision,
  ServiceRevisionSchema,
  ServiceSchema,
  Surface,
} from "@dockwell/api";

import { mira } from "./changes";

export const ENV = "env_prod";
export const SERVER = "srv_1";

/** A service running image, live on hz-1 and made an hour ago, with its Container's fields on top. */
export function anApp(
  name: string,
  image: string,
  app: MessageInitShape<typeof ContainerSchema> = {},
  init: Partial<MessageInitShape<typeof ServiceInfoSchema>> = {},
): ServiceInfo {
  const at = timestampFromDate(new Date(Date.now() - 3_600_000));
  const value: Container = create(ContainerSchema, { image, ...app });
  const base: MessageInitShape<typeof ServiceInfoSchema> = {
    id: `svc_${name}`,
    resource: { kind: "service", scope: ENV, name },
    revision: 3n,
    updateTime: at,
    spec: create(ServiceSchema, { runs: { case: "app", value } }),
    deployments: [{ serverId: SERVER, serverName: "hz-1", state: DeploymentState.LIVE, updateTime: at }],
    changeId: `chg_${name}`,
  };
  return create(ServiceInfoSchema, { ...base, ...init } as MessageInitShape<typeof ServiceInfoSchema>);
}

/** How a service runs on a server, as ListHealth has it. */
export interface HealthOn {
  state: HealthState;
  server?: string;
  serverName?: string;
  stale?: boolean;
  check?: HealthCheckKind;
  said?: string;
  passed?: boolean;
  failing?: number;
  /** How long ago it came to this, in ms, and how long ago its agent last looked. */
  forMs?: number;
  scannedMsAgo?: number;
  /** How it last read, for a server that can't be read now. */
  lastSaid?: HealthState;
}

/** How a service runs, on each server, as ListHealth has it: on hz-1, unless it says otherwise. */
export function aHealth(id: string, ...on: HealthOn[]): ServiceHealth {
  const worst: readonly HealthState[] = [
    HealthState.UNHEALTHY,
    HealthState.NOT_RUNNING,
    HealthState.PAUSED,
    HealthState.UNKNOWN,
    HealthState.STARTING,
    HealthState.NO_CHECK,
    HealthState.HEALTHY,
  ];
  const state = worst.find((w) => on.some((o) => o.state === w)) ?? HealthState.UNKNOWN;
  const now = Date.now();
  return create(ServiceHealthSchema, {
    id,
    state,
    servers: on.map((o) => {
      const since = timestampFromDate(new Date(now - (o.forMs ?? 180_000)));
      const known = o.state === HealthState.UNKNOWN && !o.lastSaid;
      return {
        serverId: o.server ?? SERVER,
        serverName: o.serverName ?? "hz-1",
        state: o.state,
        stale: o.stale ?? o.state === HealthState.UNKNOWN,
        ...(o.state === HealthState.UNKNOWN ? {} : { since }),
        scannedAt: timestampFromDate(new Date(now - (o.scannedMsAgo ?? 1_000))),
        instances: known
          ? []
          : [
              {
                container: `env_1-${id}-1`,
                state: o.lastSaid ?? o.state,
                check: o.check ?? (o.state === HealthState.NO_CHECK ? HealthCheckKind.NONE : HealthCheckKind.HTTP),
                failing: o.failing ?? 0,
                since,
                confirmedAt: since,
                ...(o.said === undefined
                  ? {}
                  : {
                      last: {
                        output: o.said,
                        passed: o.passed ?? false,
                        time: timestampFromDate(new Date(now - 4_000)),
                      },
                    }),
              },
            ],
      };
    }),
  });
}

/** A ListHealth with every given service running, and nothing checking it, unless it says otherwise. */
export function running(...services: { id: string }[]) {
  return { listHealth: () => ({ services: services.map((s) => aHealth(s.id, { state: HealthState.NO_CHECK })) }) };
}

/** The services of a Compose file, in a project of its own: they share its default network. */
export function aProject(project: string, ...names: string[]): ServiceInfo[] {
  return names.map((name) =>
    anApp(
      name,
      `ghcr.io/acme/${name}:1.0`,
      { networks: ["default"] },
      { id: `svc_${project}_${name}`, resource: { kind: "service", scope: `env_${project}`, name } },
    ),
  );
}

/** Revision number of a service that deployed image, live unless it says otherwise. */
export function aRevision(
  number: bigint,
  image: string,
  init: Partial<MessageInitShape<typeof ServiceRevisionSchema>> = {},
): ServiceRevision {
  const base: MessageInitShape<typeof ServiceRevisionSchema> = {
    number,
    createTime: timestampFromDate(new Date(Date.now() - 3_600_000)),
    image,
    change: {
      id: `chg_${number}`,
      summary: `Deploy ${image} to web`,
      state: ChangeState.APPLIED,
      proposedBy: mira,
      proposedVia: Surface.PANEL,
      appliedBy: mira,
      appliedVia: Surface.PANEL,
    },
    changes: number > 1n ? [{ path: "app.image", op: FieldOp.MODIFIED, from: "nginx:1.0", to: image }] : [],
    deployments: [{ serverId: SERVER, serverName: "hz-1", state: DeploymentState.LIVE }],
  };
  return create(ServiceRevisionSchema, { ...base, ...init } as MessageInitShape<typeof ServiceRevisionSchema>);
}

function resource(id: string, kind: string, scope: string, name: string): Resource {
  return create(ResourceSchema, { id, resource: { kind, scope, name } });
}

/** The API's answer for what lives where: the hidden project and its production, and any environments more. */
export function places(...more: string[]) {
  const environments = [
    resource(ENV, "environment", "prj_1", "production"),
    ...more.map((name) => resource(`env_${name}`, "environment", "prj_1", name)),
  ];
  return {
    listResources: (req: { kind: string }) => ({
      resources:
        req.kind === "project"
          ? [resource("prj_1", "project", "org_1", "default")]
          : req.kind === "environment"
            ? environments
            : [],
    }),
  };
}

/** The API's answer once Compose files have made projects: the hidden default, then one for each, with its production. */
export function projectPlaces(...projects: string[]) {
  const environments = [
    resource(ENV, "environment", "prj_1", "production"),
    ...projects.map((name) => resource(`env_${name}`, "environment", `prj_${name}`, "production")),
  ];
  return {
    listResources: (req: { kind: string }) => ({
      resources:
        req.kind === "project"
          ? [
              resource("prj_1", "project", "org_1", "default"),
              ...projects.map((name) => resource(`prj_${name}`, "project", "org_1", name)),
            ]
          : req.kind === "environment"
            ? environments
            : [],
    }),
  };
}
