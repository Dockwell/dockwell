// SPDX-License-Identifier: AGPL-3.0-only

import { DeploymentState, Protocol, RestartPolicy } from "@dockwell/api";
import { useLingui } from "@lingui/react/macro";
import { useMemo } from "react";

import type { AppState } from "./app";

/** The words the app screens share, in the active locale. */
export interface AppWords {
  /** Where an app stands, in a word or two: "Live", "Waiting for hz-1". */
  state(state: AppState): string;
  /** How a deployment went: "Live", "Failed". */
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
            return t({ message: "Live", context: "An app whose latest revision is deployed" });
          case "deploying":
            return t`Deploying`;
          case "waiting":
            return t`Waiting for ${servers}`;
          case "failed":
            return t`Failed`;
          case "offline":
            return t`${servers} offline`;
          case "rolled-back":
            return t`Rolled back`;
          case "unplaced":
            return t`Not placed`;
        }
      },
      deployment(state) {
        switch (state) {
          case DeploymentState.PENDING:
            return t`Deploying`;
          case DeploymentState.LIVE:
            return t({ message: "Live", context: "An app whose latest revision is deployed" });
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
