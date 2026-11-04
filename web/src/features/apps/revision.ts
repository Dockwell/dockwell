// SPDX-License-Identifier: AGPL-3.0-only

import { ChangeState, DeploymentState, type ServiceRevision } from "@dockwell/api";
import type { Status } from "@dockwell/ui";
import { useLingui } from "@lingui/react/macro";
import { useCallback } from "react";

/** A revision's place in an app's history, as a status and a word. */
export interface Look {
  status: Status;
  word: string;
}

/** Where a revision stands: live, deploying, failed, rolled back or replaced. */
export function useRevisionLook() {
  const { t } = useLingui();
  return useCallback(
    (rev: ServiceRevision, latest: bigint): Look => {
      const change = rev.change;
      if (rev.deleted) return { status: "stopped", word: t`Deleted` };
      if (change?.state === ChangeState.APPLYING || rev.deployments.some((d) => d.state === DeploymentState.PENDING)) {
        return { status: "deploying", word: t`Deploying` };
      }
      if (change?.state === ChangeState.FAILED || rev.deployments.some((d) => d.state === DeploymentState.FAILED)) {
        return { status: "down", word: t`Failed` };
      }
      if (rev.number === latest)
        return { status: "healthy", word: t({ message: "Live", context: "An app whose latest revision is deployed" }) };
      if (change?.undoneBy) return { status: "stopped", word: t`Rolled back` };
      return { status: "stopped", word: t`Replaced` };
    },
    [t],
  );
}

/**
 * What a revision was for: its change's summary, in the words of whoever made
 * it, or for Dockwell's own rollback after a failure, what happened.
 */
export function useRevisionTitle() {
  const { t } = useLingui();
  return useCallback(
    (rev: ServiceRevision): string => {
      if (rev.change?.rollback) return t`Rolled back after a failed deploy`;
      return rev.change?.summary ?? "";
    },
    [t],
  );
}
