// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import { ChangeService, ChangeState, DesiredRunState, type ResourceRef } from "@dockwell/api";
import { useLingui } from "@lingui/react/macro";

import { useChangeSheet } from "../changes/ChangeSheet";

export function useWorkloadControls() {
  const { t } = useLingui();
  const transport = useTransport();
  const sheet = useChangeSheet();
  const changeRun = (resource: ResourceRef, baseRevision: bigint, start: boolean, onApplied?: () => void) => {
    const { name } = resource;
    const app = { desiredRunState: start ? DesiredRunState.RUNNING : DesiredRunState.STOPPED };
    const kind =
      resource.kind === "container"
        ? { case: "container" as const, value: app }
        : { case: "service" as const, value: { runs: { case: "app" as const, value: app } } };
    sheet.open({
      kind: "propose",
      summary: start ? t`Start ${name}` : t`Stop ${name}`,
      edits: [{ resource, baseRevision, spec: { kind } }],
      onClose: (change) => {
        if (change?.state === ChangeState.APPLIED) onApplied?.();
      },
    });
  };
  const restartInstance = (id: string, serverId: string, containerId: string, onApplied?: () => void) =>
    sheet.open({
      kind: "plan",
      make: () => createClient(ChangeService, transport).restartService({ id, serverId, containerId }),
      onClose: (change) => {
        if (change?.state === ChangeState.APPLIED) onApplied?.();
      },
    });
  return { changeRun, restartInstance };
}
