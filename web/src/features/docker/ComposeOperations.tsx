// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import {
  ChangeService,
  ChangeState,
  type ComposeWorkspaceResource,
  type DockerObject,
  ServiceService,
} from "@dockwell/api";
import { useLingui } from "@lingui/react/macro";

import { appOf } from "../apps/app";
import { runStateEdit } from "../apps/workload";
import { useMay } from "../auth/may";
import { useChangeSheet } from "../changes/ChangeSheet";
import { ActionMenu, type MenuAction } from "../shell/ActionMenu";
import type { ComposeRunAction } from "./search";

export function useComposeWorkloadPlan() {
  const { t } = useLingui();
  const sheet = useChangeSheet();
  const transport = useTransport();
  return (
    row: DockerObject,
    services: readonly ComposeWorkspaceResource[],
    action: ComposeRunAction,
    onApplied: () => void,
  ) => {
    const environment = row.detail.case === "compose" ? row.detail.value.environmentId : "";
    const serverId = row.key?.serverId ?? "";
    const change = createClient(ChangeService, transport);
    sheet.open({
      kind: "plan",
      make: async () => {
        if (action === "restart") return change.restartService({ id: environment, serverId });
        const client = createClient(ServiceService, transport);
        const selected = new Map(services.map((item) => [item.resourceId, item]));
        const edits = [];
        let pageToken = "";
        do {
          const page = await client.listServices({ environment, pageToken });
          for (const service of page.services) {
            const expected = selected.get(service.id);
            if (!expected) continue;
            if (!service.resource || !appOf(service)) throw new Error(t`Couldn't read every service in this stack.`);
            const placements = new Set(service.deployments.map((item) => item.serverId));
            if (placements.size !== expected.serverIds.length || expected.serverIds.some((id) => !placements.has(id))) {
              throw new Error(t`The stack's servers changed. Refresh to review the latest placements.`);
            }
            selected.delete(service.id);
            edits.push(runStateEdit(service.resource, service.revision, action === "start"));
          }
          pageToken = page.nextPageToken;
        } while (pageToken && selected.size);
        if (selected.size) throw new Error(t`The stack's servers changed. Refresh to review the latest placements.`);
        if (!edits.length) throw new Error(t`This stack has no services placed on a server.`);
        const name = row.name;
        return change.proposeChange({
          edits,
          summary: action === "start" ? t`Start stack ${name}` : t`Stop stack ${name}`,
        });
      },
      onClose: (change) => {
        if (change?.state === ChangeState.APPLIED) onApplied();
      },
    });
  };
}

export function ComposeOperations({
  row,
  showText = false,
  open,
  takeOn,
  remove,
  onWorkload,
}: {
  row: DockerObject;
  showText?: boolean;
  open?: (() => void) | undefined;
  takeOn?: (() => void) | undefined;
  remove?: (() => void) | undefined;
  onWorkload?: ((action: ComposeRunAction) => void) | undefined;
}) {
  const { t } = useLingui();
  const may = useMay();
  const environment = row.detail.case === "compose" ? row.detail.value.environmentId : "";
  const canPlan = environment && may(ChangeService.method.proposeChange, { environment }).allowed;
  const actions: MenuAction[] = [];
  if (open) actions.push({ id: "open", label: t`Open`, icon: "arrow-right", onSelect: open });
  if (takeOn) actions.push({ id: "take", label: t`Take on`, icon: "download", onSelect: takeOn });
  if (onWorkload && canPlan && may(ServiceService.method.listServices, { environment }).allowed) {
    actions.push(
      { id: "start", label: t`Start stack`, icon: "play", onSelect: () => onWorkload("start") },
      { id: "stop", label: t`Stop stack`, icon: "stop", onSelect: () => onWorkload("stop") },
    );
  }
  if (onWorkload && environment && may(ChangeService.method.restartService, { environment }).allowed) {
    actions.push({
      id: "restart",
      label: t`Restart stack`,
      icon: "refresh",
      onSelect: () => onWorkload("restart"),
    });
  }
  if (remove) actions.push({ id: "delete", label: t`Delete stack`, icon: "trash", danger: true, onSelect: remove });
  if (!actions.length) return null;
  const name = row.name;
  return <ActionMenu label={t`Actions for ${name}`} text={showText ? t`Actions` : undefined} actions={actions} />;
}
