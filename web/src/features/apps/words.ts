// SPDX-License-Identifier: AGPL-3.0-only

import { DeploymentState, Protocol, RestartPolicy } from "@dockwell/api";
import { useLingui } from "@lingui/react/macro";
import { useMemo } from "react";

import type { AppState } from "./app";

/** The words the app screens share, in the active locale. */
export interface AppWords {
  /** A field of a service, as a change names it, in words. */
  field(name: string): string;
  /** Where an app's record stands, in a word or two: "Deployed", "Waiting for server". How it runs is useHealthWords'. */
  state(state: AppState): string;
  /** How a deployment went: "Deployed", "Failed". */
  deployment(state: DeploymentState): string;
  restart(policy: RestartPolicy): string;
  protocol(protocol: Protocol): string;
}

export function useAppWords(): AppWords {
  const { t } = useLingui();
  return useMemo<AppWords>(
    () => ({
      state(state) {
        const servers = state.servers.join(", ");
        switch (state.kind) {
          case "live":
            return t({ message: "Deployed", context: "An app whose latest revision is deployed" });
          case "deploying":
            return t`Deploying`;
          case "waiting":
            return t`Waiting for server`;
          case "failed":
            return t`Failed`;
          case "rollback-failed":
            return t`Rollback failed`;
          case "offline":
            return t`${servers} offline`;
          case "rolled-back":
            return t`Rolled back`;
          case "unplaced":
            return t`Not placed`;
          case "unknown":
            return t({ message: "Deployed", context: "An app whose latest revision is deployed" });
          case "unreadable":
            return t`Can't be read`;
        }
      },
      /** A field of a service, as a change names it, in words. */
      field(name: string): string {
        const fields: Readonly<Record<string, string>> = {
          image: t`image`,
          command: t`command`,
          entrypoint: t`entrypoint`,
          no_entrypoint: t`entrypoint`,
          env: t`environment`,
          variables: t`variables`,
          secrets: t`secrets`,
          ports: t`ports`,
          mounts: t`mounts`,
          networks: t`networks`,
          labels: t`labels`,
          restart: t`restart policy`,
          user: t`user`,
          working_dir: t`working directory`,
          healthcheck: t`health check`,
        };
        return fields[name] ?? name;
      },
      deployment(state) {
        switch (state) {
          case DeploymentState.PENDING:
            return t`Deploying`;
          case DeploymentState.LIVE:
            return t({ message: "Deployed", context: "An app whose latest revision is deployed" });
          case DeploymentState.FAILED:
            return t`Failed`;
          case DeploymentState.SUPERSEDED:
            return t`Replaced`;
        }
        return t`Unknown`;
      },
      restart(policy) {
        switch (policy) {
          case RestartPolicy.NO:
            return t`Never`;
          case RestartPolicy.ON_FAILURE:
            return t`When it exits with an error`;
          case RestartPolicy.ALWAYS:
            return t`Always`;
          case RestartPolicy.UNLESS_STOPPED:
            return t`Unless someone stops it`;
        }
        return t`Unless someone stops it`;
      },
      protocol(protocol) {
        return protocol === Protocol.UDP ? "UDP" : "TCP";
      },
    }),
    [t],
  );
}
