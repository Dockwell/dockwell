// SPDX-License-Identifier: AGPL-3.0-only

import { create, type MessageInitShape } from "@bufbuild/protobuf";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import {
  ChangeState,
  type Container,
  ContainerSchema,
  DeploymentState,
  FieldOp,
  type Resource,
  ResourceSchema,
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
