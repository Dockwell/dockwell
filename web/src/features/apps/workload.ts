// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import { ChangeService, ChangeState, DesiredRunState, type ResourceRef } from "@dockwell/api";
import { useLingui } from "@lingui/react/macro";

import { useChangeSheet } from "../changes/ChangeSheet";

export function runStateEdit(resource: ResourceRef, baseRevision: bigint, start: boolean | DesiredRunState) {
  const app = {
    desiredRunState: typeof start === "boolean" ? (start ? DesiredRunState.RUNNING : DesiredRunState.STOPPED) : start,
  };
  const kind =
    resource.kind === "container"
      ? { case: "container" as const, value: app }
      : { case: "service" as const, value: { runs: { case: "app" as const, value: app } } };
  return { resource, baseRevision, spec: { kind } };
}

export function useWorkloadControls() {
  const { t } = useLingui();
  const transport = useTransport();
  const sheet = useChangeSheet();
  const changeRun = (resource: ResourceRef, baseRevision: bigint, start: boolean, onApplied?: () => void) => {
    const { name } = resource;
    sheet.open({
      kind: "propose",
      summary: start ? t`Start ${name}` : t`Stop ${name}`,
      edits: [runStateEdit(resource, baseRevision, start)],
      onClose: (change) => {
        if (change?.state === ChangeState.APPLIED) onApplied?.();
      },
    });
  };
  /** Restarts a service on every server it runs on, as its own page's Restart does. */
  const restart = (id: string, onApplied?: () => void) =>
    sheet.open({
      kind: "plan",
      make: () => createClient(ChangeService, transport).restartService({ id }),
      onClose: (change) => {
        if (change?.state === ChangeState.APPLIED) onApplied?.();
      },
    });
  const restartInstance = (id: string, serverId: string, containerId: string, onApplied?: () => void) =>
    sheet.open({
      kind: "plan",
      make: () => createClient(ChangeService, transport).restartService({ id, serverId, containerId }),
      onClose: (change) => {
        if (change?.state === ChangeState.APPLIED) onApplied?.();
      },
    });
  /** Starts or stops several workloads as one plan with an edit for each, never a change apiece. */
  const changeRunMany = (
    targets: readonly { resource: ResourceRef; baseRevision: bigint }[],
    start: boolean | DesiredRunState,
    summary: string,
    onApplied?: () => void,
  ) => {
    if (targets.length === 0) return;
    sheet.open({
      kind: "propose",
      summary,
      edits: targets.map(({ resource, baseRevision }) => runStateEdit(resource, baseRevision, start)),
      onClose: (change) => {
        if (change?.state === ChangeState.APPLIED) onApplied?.();
      },
    });
  };
  const changePause = (resource: ResourceRef, baseRevision: bigint, pause: boolean, onApplied?: () => void) => {
    const { name } = resource;
    changeRunMany(
      [{ resource, baseRevision }],
      pause ? DesiredRunState.PAUSED : DesiredRunState.RUNNING,
      pause ? t`Pause ${name}` : t`Unpause ${name}`,
      onApplied,
    );
  };
  return { changeRun, changePause, restartInstance, changeRunMany, restart };
}
